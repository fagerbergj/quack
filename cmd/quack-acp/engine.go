package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	sdk "github.com/coder/acp-go-sdk"
)

// engine is one pig-quack process in RPC mode: JSON commands on stdin, JSON events on stdout.
type engine struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	wmu    sync.Mutex
	exited chan struct{}
}

func (e *engine) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	e.wmu.Lock()
	defer e.wmu.Unlock()
	_, err = e.stdin.Write(append(b, '\n'))
	return err
}

func (e *engine) kill() {
	_ = e.cmd.Process.Kill()
	<-e.exited
}

// turn is one session/prompt in flight, settled by the engine's agent_settled event.
type turn struct {
	done      chan struct{}
	cancelled bool
	lastStop  map[string]any
}

// engineEvent is the subset of the engine's RPC events the shim relays.
type engineEvent struct {
	Type    string `json:"type"`
	Message *struct {
		Role         string `json:"role"`
		StopReason   string `json:"stopReason"`
		ErrorMessage string `json:"errorMessage"`
		Usage        *struct {
			Input       int `json:"input"`
			Output      int `json:"output"`
			CacheRead   int `json:"cacheRead"`
			CacheWrite  int `json:"cacheWrite"`
			TotalTokens int `json:"totalTokens"`
		} `json:"usage"`
	} `json:"message"`
	AssistantMessageEvent *struct {
		Type  string `json:"type"`
		Delta string `json:"delta"`
	} `json:"assistantMessageEvent"`
	ToolCallID string         `json:"toolCallId"`
	ToolName   string         `json:"toolName"`
	Args       map[string]any `json:"args"`
	IsError    bool           `json:"isError"`
	Result     *struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"result"`
}

func (s *shim) spawn(id sdk.SessionId, cwd, dir, prefix string, bridged []string, resume bool) (*engine, error) {
	// --session-dir must differ from the agent dir: equal, the engine's session lookup misses on a second launch.
	args := []string{"--mode", "rpc", "--session-id", string(id), "--session-dir", filepath.Join(dir, "pi-sessions")}
	if s.cfg.Model != "" {
		args = append(args, "--provider", "quack", "--model", s.cfg.Model)
	}
	// --tools also filters extension tools, so the bridge's names join pi's four built-ins.
	args = append(args, "--no-extensions", "--tools", strings.Join(append([]string{"read", "bash", "edit", "write"}, bridged...), ","))
	cmd := exec.Command(s.engineCmd, args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "PIG_CODING_AGENT_DIR="+dir, "PI_OFFLINE=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", s.engineCmd, err)
	}
	e := &engine{cmd: cmd, stdin: stdin, exited: make(chan struct{})}
	r := &relay{s: s, sid: id, prefix: prefix}
	s.mu.Lock()
	s.resumeFailed = false
	s.mu.Unlock()
	go s.watchStderr(stderr, resume)
	go func() {
		r.read(stdout)
		_ = cmd.Wait()
		close(e.exited)
	}()
	return e, nil
}

// watchStderr forwards the engine's stderr; a resume that the engine turned into a blank session fails the prompt.
func (s *shim) watchStderr(r io.Reader, resume bool) {
	sc := bufio.NewScanner(r)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		line := sc.Text()
		fmt.Fprintln(os.Stderr, line)
		if resume && strings.Contains(line, "No project session found") {
			s.mu.Lock()
			s.resumeFailed = true
			s.mu.Unlock()
		}
	}
}

// relay turns one engine's events into ACP session/update notifications for its session.
type relay struct {
	s      *shim
	sid    sdk.SessionId
	prefix string
}

func (r *relay) read(stdout io.Reader) {
	br := bufio.NewReader(stdout)
	for {
		line, err := br.ReadBytes('\n')
		var ev engineEvent
		if len(line) > 1 && json.Unmarshal(line, &ev) == nil {
			r.handle(&ev)
		}
		if err != nil {
			return
		}
	}
}

func (r *relay) notify(u sdk.SessionUpdate) {
	_ = r.s.client().SessionUpdate(context.Background(), sdk.SessionNotification{SessionId: r.sid, Update: u})
}

func (r *relay) handle(ev *engineEvent) {
	switch ev.Type {
	case "message_end":
		r.messageEnd(ev)
	case "message_update":
		if e := ev.AssistantMessageEvent; e != nil && e.Type == "text_delta" {
			r.notify(sdk.UpdateAgentMessageText(e.Delta))
		} else if e != nil && e.Type == "thinking_delta" {
			r.notify(sdk.UpdateAgentThoughtText(e.Delta))
		}
	case "tool_execution_start":
		r.notify(r.toolStart(ev))
	case "tool_execution_end":
		r.notify(toolEnd(ev))
	case "agent_settled":
		r.s.settle()
	}
}

// messageEnd records the stop for the prompt response and reports the finished call's usage: one
// usage_update per model call, which quack records as one llm.call.
func (r *relay) messageEnd(ev *engineEvent) {
	m := ev.Message
	if m == nil || m.Role != "assistant" {
		return
	}
	stop := map[string]any{"pi_stop_reason": m.StopReason}
	if m.ErrorMessage != "" {
		stop["pi_error"] = m.ErrorMessage
	}
	r.s.recordStop(stop)
	if u := m.Usage; u != nil && u.TotalTokens > 0 {
		// The engine's input excludes cache reads and writes; quack's prompt tokens include them, cached a subset.
		r.notify(sdk.SessionUpdate{UsageUpdate: &sdk.SessionUsageUpdate{Used: u.TotalTokens, Meta: map[string]any{
			"quack_prompt_tokens":     u.Input + u.CacheRead + u.CacheWrite,
			"quack_cached_tokens":     u.CacheRead,
			"quack_completion_tokens": u.Output,
		}}})
	}
}

// toolStart carries a bridged MCP tool's real name in _meta, so quack's relay never parses it from the title.
func (r *relay) toolStart(ev *engineEvent) sdk.SessionUpdate {
	args := ev.Args
	if args == nil {
		args = map[string]any{}
	}
	u := sdk.StartToolCall(sdk.ToolCallId(ev.ToolCallID), ev.ToolName, sdk.WithStartKind(nativeKind(ev.ToolName)),
		sdk.WithStartStatus(sdk.ToolCallStatusInProgress), sdk.WithStartRawInput(args))
	if name, ok := strings.CutPrefix(ev.ToolName, r.prefix+"_"); ok {
		u.ToolCall.Kind = mcpKind(name)
		u.ToolCall.Meta = map[string]any{"quack_mcp_tool": name}
	}
	return u
}

func toolEnd(ev *engineEvent) sdk.SessionUpdate {
	var txt strings.Builder
	if ev.Result != nil {
		for _, c := range ev.Result.Content {
			if c.Type == "text" {
				txt.WriteString(c.Text)
			}
		}
	}
	status := sdk.ToolCallStatusCompleted
	if ev.IsError {
		status = sdk.ToolCallStatusFailed
	}
	opts := []sdk.ToolCallUpdateOpt{sdk.WithUpdateStatus(status), sdk.WithUpdateRawOutput(map[string]any{"output": txt.String()})}
	if txt.Len() > 0 {
		opts = append(opts, sdk.WithUpdateContent([]sdk.ToolCallContent{sdk.ToolContent(sdk.TextBlock(txt.String()))}))
	}
	return sdk.UpdateToolCall(sdk.ToolCallId(ev.ToolCallID), opts...)
}

func nativeKind(tool string) sdk.ToolKind {
	switch tool {
	case "bash":
		return sdk.ToolKindExecute
	case "read":
		return sdk.ToolKindRead
	case "edit", "write":
		return sdk.ToolKindEdit
	}
	return sdk.ToolKindOther
}

var (
	readVerb   = regexp.MustCompile(`^(read|list|load|recall|get|check)_`)
	deleteVerb = regexp.MustCompile(`^(delete|unstage)_`)
)

// mcpKind guesses a bridged tool's kind from its verb; a miss costs a wrong icon, never a broken call.
func mcpKind(name string) sdk.ToolKind {
	switch {
	case readVerb.MatchString(name):
		return sdk.ToolKindRead
	case deleteVerb.MatchString(name):
		return sdk.ToolKindDelete
	}
	return sdk.ToolKindEdit
}

func newSessionID() sdk.SessionId {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return sdk.SessionId("pi-" + hex.EncodeToString(b))
}
