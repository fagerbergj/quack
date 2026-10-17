// Command quack-acp is the ACP agent quack spawns for its coding agents: it speaks ACP on stdio and drives
// pig-quack (PIG with the quackgate policy gate and MCP bridge fused in) in `--mode rpc` underneath.
package main

import (
	"cmp"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	sdk "github.com/coder/acp-go-sdk"
)

func main() {
	var cfg config
	if s := os.Getenv("PI_ACP_CONFIG"); s != "" {
		if err := json.Unmarshal([]byte(s), &cfg); err != nil {
			fmt.Fprintln(os.Stderr, "quack-acp: PI_ACP_CONFIG:", err)
			os.Exit(2)
		}
	}
	s := newShim(cfg, cmp.Or(os.Getenv("PI_ACP_STATE_DIR"), os.TempDir()), cmp.Or(os.Getenv("PI_ACP_PI_CMD"), "/usr/local/bin/pig-quack"))
	conn := sdk.NewAgentSideConnection(s, os.Stdout, os.Stdin)
	// quack shows the stderr tail on a failed round; routine connection notices would crowd it out.
	conn.SetLogger(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	s.setConn(conn)
	<-conn.Done()
	s.stopEngine()
}
