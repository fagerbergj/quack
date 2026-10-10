// Package openaimodel adapts an OpenAI-compatible endpoint to ADK's model.LLM, surfacing reasoning as Thought parts.
// Modified from github.com/byebyebruce/adk-go-openai (MIT).
package openaimodel

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/respjson"
	"github.com/openai/openai-go/v3/shared"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/httpx"
)

var _ model.LLM = &OpenAIModel{}

var ErrNoChoicesInResponse = errors.New("no choices in OpenAI response")

type OpenAIModel struct {
	client    openai.Client
	ModelName string
	// DefaultEffort is models.<name>.effort; a request's own ThinkingConfig wins over it.
	DefaultEffort string
}

func NewOpenAIModel(modelName, endpoint, apiKey, defaultEffort string) *OpenAIModel {
	client := openai.NewClient(
		option.WithBaseURL(endpoint),
		option.WithAPIKey(apiKey),
		option.WithHTTPClient(&http.Client{Transport: httpx.NewTransport(nil)}),
	)
	return &OpenAIModel{
		client:        client,
		ModelName:     modelName,
		DefaultEffort: defaultEffort,
	}
}

func (o *OpenAIModel) Name() string {
	return o.ModelName
}

// reasoningUsage makes output mean answer only, not answer+reasoning. Estimates chars/4 when
// the provider omits reasoning_tokens (llama-server).
func reasoningUsage(ctx context.Context, model string, completionTokens, reasoningTokens int32, reasoningText string) (candidates, thoughts int32) {
	if reasoningTokens == 0 && reasoningText != "" {
		reasoningTokens = int32((len(reasoningText) + 3) / 4)
		slog.DebugContext(ctx, "estimated reasoning tokens from reasoning_content chars/4 (no reasoning_tokens in usage)",
			"component", "inference", "model", model, "reasoning_tokens_estimated", reasoningTokens, "chars", len(reasoningText))
	}
	candidates = completionTokens - reasoningTokens
	if candidates < 0 {
		candidates = 0
	}
	return candidates, reasoningTokens
}

type bestEffortKey struct{}

// WithBestEffort: caller already logs its own degraded outcome, so apiErr
// logs at Debug instead of Error to avoid a duplicate line.
func WithBestEffort(ctx context.Context) context.Context {
	return context.WithValue(ctx, bestEffortKey{}, true)
}

func IsBestEffort(ctx context.Context) bool {
	return ctx.Value(bestEffortKey{}) != nil
}

// inProcess: this whole process is a CLI-driven ephemeral duck (never a real
// `quack server run`), so its own caller reports every failure already.
var inProcess atomic.Bool

// SetInProcess marks the process so apiErr logs at Debug, not Error - the
// CLI already prints the failure to the same terminal.
func SetInProcess() { inProcess.Store(true) }

// apiErr logs a failure (load-bearing: ADK can swallow it into empty output)
// and returns an enriched error.
func (o *OpenAIModel) apiErr(ctx context.Context, op string, err error) error {
	level := slog.LevelError
	// A cancelled request is a stop or shutdown, not a gateway fault.
	if IsBestEffort(ctx) || inProcess.Load() || errors.Is(ctx.Err(), context.Canceled) {
		level = slog.LevelDebug
	}
	var ae *openai.Error
	if errors.As(err, &ae) {
		slog.Log(ctx, level, "openai API error", "component", "inference",
			"model", o.ModelName, "op", op, "status", ae.StatusCode, "body", ae.Error())
		return fmt.Errorf("openai %s (%s): status %d: %s", o.ModelName, op, ae.StatusCode, ae.Error())
	}
	slog.Log(ctx, level, "openai request failed", "component", "inference",
		"model", o.ModelName, "op", op, "err", err)
	return fmt.Errorf("openai %s (%s): %w", o.ModelName, op, err)
}

// EmbedUsage is an /embeddings usage block; with no completion, PromptTokens normally equals TotalTokens.
type EmbedUsage struct {
	PromptTokens int64
	TotalTokens  int64
}

// Embed returns one vector per text, in input order, using ModelName as the embedding model.
func (o *OpenAIModel) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out, _, err := o.EmbedWithUsage(ctx, texts)
	return out, err
}

// EmbedWithUsage is Embed plus usage, kept off the public Embedder interface.
func (o *OpenAIModel) EmbedWithUsage(ctx context.Context, texts []string) ([][]float32, EmbedUsage, error) {
	if len(texts) == 0 {
		return nil, EmbedUsage{}, nil
	}
	// Embedding has no external side effect, so retrying a server fault is safe.
	resp, err := o.client.Embeddings.New(httpx.WithIdempotent(ctx), openai.EmbeddingNewParams{
		Model: openai.EmbeddingModel(o.ModelName),
		Input: openai.EmbeddingNewParamsInputUnion{OfArrayOfStrings: texts},
	})
	if err != nil {
		return nil, EmbedUsage{}, err
	}
	if len(resp.Data) != len(texts) {
		return nil, EmbedUsage{}, fmt.Errorf("openaimodel: embeddings returned %d vectors for %d inputs", len(resp.Data), len(texts))
	}
	// Place each vector by its declared index - the API need not return them in order.
	out := make([][]float32, len(texts))
	for _, e := range resp.Data {
		if e.Index < 0 || int(e.Index) >= len(out) {
			return nil, EmbedUsage{}, fmt.Errorf("openaimodel: embedding index %d out of range for %d inputs", e.Index, len(texts))
		}
		v := make([]float32, len(e.Embedding))
		for i, f := range e.Embedding {
			v[i] = float32(f)
		}
		out[e.Index] = v
	}
	usage := EmbedUsage{PromptTokens: resp.Usage.PromptTokens, TotalTokens: resp.Usage.TotalTokens}
	return out, usage, nil
}

func (o *OpenAIModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	if stream {
		return o.generateStream(ctx, req)
	}
	return o.generate(ctx, req)
}

// applyDefaultEffort mutates req in place so the ledger, which reads the same req later,
// records the resolved effort too.
func (o *OpenAIModel) applyDefaultEffort(req *model.LLMRequest) {
	if o.DefaultEffort == "" {
		return
	}
	if req.Config == nil {
		req.Config = &genai.GenerateContentConfig{}
	}
	if req.Config.ThinkingConfig != nil {
		return // an explicit ThinkingConfig (e.g. judge thinking_level) wins
	}
	req.Config.ThinkingConfig = effortThinkingConfig(o.DefaultEffort)
}

// effortThinkingConfig maps models.<name>.effort to genai's enum. Config.validate rejects other
// values; one reaching here sends no ThinkingConfig rather than guessing medium.
func effortThinkingConfig(effort string) *genai.ThinkingConfig {
	switch effort {
	case "low":
		return &genai.ThinkingConfig{ThinkingLevel: genai.ThinkingLevelLow}
	case "medium":
		return &genai.ThinkingConfig{ThinkingLevel: genai.ThinkingLevelMedium}
	case "high":
		return &genai.ThinkingConfig{ThinkingLevel: genai.ThinkingLevelHigh}
	default:
		return nil
	}
}

func (o *OpenAIModel) generate(ctx context.Context, req *model.LLMRequest) iter.Seq2[*model.LLMResponse, error] {
	o.applyDefaultEffort(req)
	return func(yield func(*model.LLMResponse, error) bool) {
		openaiReq, err := toOpenAIChatCompletionRequest(req, o.ModelName)
		if err != nil {
			yield(nil, err)
			return
		}

		// A completion has no side effect until quack acts on it, so retrying a 502 is safe.
		resp, err := o.client.Chat.Completions.New(httpx.WithIdempotent(ctx), openaiReq)
		if err != nil {
			yield(nil, o.apiErr(ctx, "generate", err))
			return
		}

		llmResp, err := convertChatCompletionResponse(ctx, resp)
		if err != nil {
			yield(nil, err)
			return
		}

		yield(llmResp, nil)
	}
}

func (o *OpenAIModel) generateStream(ctx context.Context, req *model.LLMRequest) iter.Seq2[*model.LLMResponse, error] {
	o.applyDefaultEffort(req)
	return func(yield func(*model.LLMResponse, error) bool) {
		openaiReq, err := toOpenAIChatCompletionRequest(req, o.ModelName)
		if err != nil {
			yield(nil, err)
			return
		}
		openaiReq.StreamOptions = openai.ChatCompletionStreamOptionsParam{
			IncludeUsage: openai.Bool(true),
		}

		stream := o.client.Chat.Completions.NewStreaming(httpx.WithIdempotent(ctx), openaiReq)
		defer func() { _ = stream.Close() }()

		s := &streamAgg{
			content:   &genai.Content{Role: "model", Parts: []*genai.Part{}},
			toolCalls: make(map[int64]*toolCallBuilder),
		}

		for stream.Next() {
			if !o.processChunk(ctx, stream.Current(), s, yield) {
				return
			}
		}

		if err := stream.Err(); err != nil {
			// Agents stream, so this is where a model 400 actually surfaces.
			yield(nil, o.apiErr(ctx, "generate_stream", err))
			return
		}

		o.emitFinal(ctx, openaiReq, s, yield)
	}
}

// streamAgg accumulates one streaming generation across chunks.
type streamAgg struct {
	content        *genai.Content
	finishReason   genai.FinishReason
	usage          *genai.GenerateContentResponseUsageMetadata
	modelVersion   string
	toolCalls      map[int64]*toolCallBuilder
	lastPartIsText bool
}

// processChunk returns false once the consumer stops.
func (o *OpenAIModel) processChunk(ctx context.Context, chunk openai.ChatCompletionChunk, s *streamAgg, yield func(*model.LLMResponse, error) bool) bool {
	if chunk.Model != "" {
		s.modelVersion = chunk.Model
	}

	// Usage arrives on the final usage-only chunk when IncludeUsage is set.
	if chunk.Usage.TotalTokens > 0 {
		s.usage = &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:        int32(chunk.Usage.PromptTokens),
			CandidatesTokenCount:    int32(chunk.Usage.CompletionTokens),
			TotalTokenCount:         int32(chunk.Usage.TotalTokens),
			CachedContentTokenCount: int32(chunk.Usage.PromptTokensDetails.CachedTokens),
			ThoughtsTokenCount:      int32(chunk.Usage.CompletionTokensDetails.ReasoningTokens),
		}
	}

	if len(chunk.Choices) == 0 {
		return true
	}

	choice := chunk.Choices[0]

	if choice.Delta.Content != "" {
		part := &genai.Part{Text: choice.Delta.Content}
		if s.lastPartIsText {
			s.content.Parts[len(s.content.Parts)-1].Text += part.Text
		} else {
			s.content.Parts = append(s.content.Parts, part)
		}
		s.lastPartIsText = true
		if !yield(&model.LLMResponse{
			Content:      &genai.Content{Role: "model", Parts: []*genai.Part{part}},
			Partial:      true,
			TurnComplete: false,
		}, nil) {
			return false
		}
	} else {
		s.lastPartIsText = false
	}

	if !emitReasoningPart(choice, s, yield) {
		return false
	}

	// Tool calls arrive split across chunks, keyed by index.
	for _, toolCall := range choice.Delta.ToolCalls {
		idx := toolCall.Index
		builder, exists := s.toolCalls[idx]
		if !exists {
			builder = &toolCallBuilder{}
			s.toolCalls[idx] = builder
		}
		if toolCall.ID != "" {
			builder.id = toolCall.ID
		}
		if toolCall.Function.Name != "" {
			builder.name = toolCall.Function.Name
		}
		builder.args += toolCall.Function.Arguments
	}

	if choice.FinishReason != "" {
		s.finishReason = convertFinishReason(choice.FinishReason)
	}
	return true
}

// emitReasoningPart surfaces reasoning as a Thought part so the UI can render thinking.
func emitReasoningPart(choice openai.ChatCompletionChunkChoice, s *streamAgg, yield func(*model.LLMResponse, error) bool) bool {
	text := reasoningExtra(choice.Delta.JSON.ExtraFields)
	if text == "" {
		return true
	}
	part := &genai.Part{Text: text, Thought: true}
	s.content.Parts = append(s.content.Parts, part)
	s.lastPartIsText = false
	return yield(&model.LLMResponse{
		Content:      &genai.Content{Role: "model", Parts: []*genai.Part{part}},
		Partial:      true,
		TurnComplete: false,
	}, nil)
}

// emitFinal yields the single TurnComplete response, after tool-call aggregation and the fallback ladder.
func (o *OpenAIModel) emitFinal(ctx context.Context, openaiReq openai.ChatCompletionNewParams, s *streamAgg, yield func(*model.LLMResponse, error) bool) {
	if len(s.toolCalls) > 0 {
		for _, idx := range slices.Sorted(maps.Keys(s.toolCalls)) {
			b := s.toolCalls[idx]
			s.content.Parts = append(s.content.Parts, &genai.Part{
				FunctionCall: &genai.FunctionCall{
					ID:   b.id,
					Name: b.name,
					Args: parseJSONArgs(b.args),
				},
			})
		}
	}

	var hasAnswer, hadThinking bool
	var promotedChars int
	s.content.Parts, hasAnswer, hadThinking, promotedChars = applyFallbackLadder(
		ctx, o.ModelName, s.content.Parts, len(s.toolCalls) > 0)
	if promotedChars > 0 {
		slog.WarnContext(ctx, "promoted reasoning to answer (empty content, unclosed </think>)",
			"component", "inference", "model", o.ModelName, "chars", promotedChars)
	}

	if s.modelVersion == "" {
		s.modelVersion = string(openaiReq.Model)
	}
	if !hasAnswer {
		// Often a reasoning model spending its whole budget thinking. Only the streaming path logs this.
		var compl int32
		if s.usage != nil {
			compl = s.usage.CandidatesTokenCount
		}
		slog.WarnContext(ctx, "model returned no answer content (empty turn)",
			"component", "inference", "model", o.ModelName, "finish_reason", string(s.finishReason),
			"had_thinking", hadThinking, "completion_tokens", compl)
	}
	if s.usage != nil {
		var reasoningText strings.Builder
		for _, p := range s.content.Parts {
			if p.Thought && p.Text != "" {
				reasoningText.WriteString(p.Text)
			}
		}
		s.usage.CandidatesTokenCount, s.usage.ThoughtsTokenCount = reasoningUsage(ctx, s.modelVersion,
			s.usage.CandidatesTokenCount, s.usage.ThoughtsTokenCount, reasoningText.String())
	}

	yield(&model.LLMResponse{
		Content:       s.content,
		UsageMetadata: s.usage,
		FinishReason:  s.finishReason,
		ModelVersion:  s.modelVersion,
		Partial:       false,
		TurnComplete:  true,
	}, nil)
}

// logRequestTail logs the last 12 entries the model receives as role/kind at Debug: ground truth for
// loop and compaction diagnosis (is the tool result actually in the request?).
func logRequestTail(req *model.LLMRequest, modelName string) {
	if !slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		return
	}
	start := len(req.Contents) - 12
	if start < 0 {
		start = 0
	}
	tail := make([]string, 0, len(req.Contents)-start)
	for _, c := range req.Contents[start:] {
		kind := "text"
		for _, p := range c.Parts {
			switch {
			case p.FunctionCall != nil:
				kind = "CALL:" + p.FunctionCall.Name
			case p.FunctionResponse != nil:
				rb, _ := json.Marshal(p.FunctionResponse.Response)
				kind = fmt.Sprintf("RESP:%s(%db)", p.FunctionResponse.Name, len(rb))
			}
		}
		tail = append(tail, c.Role+"/"+kind)
	}
	slog.Debug("request tail", "component", "inference", "model", modelName,
		"n_contents", len(req.Contents), "tail", strings.Join(tail, " | "))
}

type toolCallBuilder struct {
	id   string
	name string
	args string
}

// thoughtText / answerText: the part predicates the leak-recovery scan keys on.
func thoughtText(p *genai.Part) bool { return p.Thought && p.Text != "" }
func answerText(p *genai.Part) bool  { return !p.Thought && p.Text != "" }

// concatTarget: the concatenated text of the parts matching isTarget.
func concatTarget(parts []*genai.Part, isTarget func(*genai.Part) bool) string {
	var rb strings.Builder
	for _, p := range parts {
		if isTarget(p) {
			rb.WriteString(p.Text)
		}
	}
	return rb.String()
}

// recoverLeakedCalls replaces the isTarget parts with one cleaned part (a block can span
// several), then appends the recovered calls.
func recoverLeakedCalls(ctx context.Context, modelName string, parts []*genai.Part, isTarget func(*genai.Part) bool, thought bool, logMsg string) ([]*genai.Part, bool) {
	calls, cleaned := reasoningToolCalls(concatTarget(parts, isTarget))
	if len(calls) == 0 {
		return parts, false
	}
	rebuilt := make([]*genai.Part, 0, len(parts)+len(calls))
	replaced := false
	for _, p := range parts {
		if isTarget(p) {
			if !replaced {
				replaced = true
				if strings.TrimSpace(cleaned) != "" {
					rebuilt = append(rebuilt, &genai.Part{Text: cleaned, Thought: thought})
				}
			}
			continue
		}
		rebuilt = append(rebuilt, p)
	}
	for _, c := range calls {
		rebuilt = append(rebuilt, &genai.Part{FunctionCall: c})
	}
	slog.WarnContext(ctx, logMsg, "component", "inference", "model", modelName, "count", len(calls))
	return rebuilt, true
}

// applyFallbackLadder recovers tool calls leaked as XML into thinking (llama.cpp#22684) or the
// answer, and promotes thinking to the answer when content is empty. Callers log promotion.
func applyFallbackLadder(ctx context.Context, modelName string, parts []*genai.Part, haveToolCalls bool) (result []*genai.Part, hasAnswer, hadThinking bool, promotedChars int) {
	result = parts

	if !haveToolCalls {
		if recovered, ok := recoverLeakedCalls(ctx, modelName, result, thoughtText, true, "recovered tool calls from reasoning_content (Qwen/llama.cpp#22684)"); ok {
			result = recovered
		} else if recovered, ok := recoverLeakedCalls(ctx, modelName, result, answerText, false, "recovered tool calls leaked into answer content (bare <function=> form, #427)"); ok {
			result = recovered
		}
	}

	for _, p := range result {
		switch {
		case p.FunctionCall != nil, !p.Thought && p.Text != "":
			hasAnswer = true
		case p.Thought && p.Text != "":
			hadThinking = true
		}
	}

	// The answer can land entirely in reasoning_content; promote it rather than emit an empty turn.
	if !hasAnswer && hadThinking {
		txt := strings.TrimSpace(concatTarget(result, thoughtText))
		if txt != "" {
			result = append(result, &genai.Part{Text: txt})
			hasAnswer = true
			promotedChars = len(txt)
		}
	}

	return result, hasAnswer, hadThinking, promotedChars
}

func toOpenAIChatCompletionRequest(req *model.LLMRequest, modelName string) (openai.ChatCompletionNewParams, error) {
	messages := make([]openai.ChatCompletionMessageParamUnion, 0, len(req.Contents))
	for _, content := range req.Contents {
		msgs, err := toOpenAIChatCompletionMessage(content)
		if err != nil {
			return openai.ChatCompletionNewParams{}, err
		}
		messages = append(messages, msgs...)
	}
	logRequestTail(req, modelName)

	openaiReq := openai.ChatCompletionNewParams{
		Model:    shared.ChatModel(modelName),
		Messages: messages,
	}

	if req.Config == nil {
		return openaiReq, nil
	}
	if err := applyConfigKnobs(&openaiReq, req.Config); err != nil {
		return openai.ChatCompletionNewParams{}, err
	}

	if req.Config.SystemInstruction != nil {
		sysMsg := openai.SystemMessage(extractTextFromContent(req.Config.SystemInstruction))
		openaiReq.Messages = append([]openai.ChatCompletionMessageParamUnion{sysMsg}, openaiReq.Messages...)
	}

	return openaiReq, nil
}

func applyConfigKnobs(openaiReq *openai.ChatCompletionNewParams, cfg *genai.GenerateContentConfig) error {
	if cfg.ThinkingConfig != nil {
		switch cfg.ThinkingConfig.ThinkingLevel {
		case genai.ThinkingLevelLow:
			openaiReq.ReasoningEffort = "low"
		case genai.ThinkingLevelHigh:
			openaiReq.ReasoningEffort = "high"
		default:
			openaiReq.ReasoningEffort = "medium"
		}
	}

	if cfg.ResponseSchema != nil {
		openaiReq.ResponseFormat = openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{
				JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{
					Name:   "response",
					Strict: openai.Bool(true),
					Schema: cfg.ResponseSchema,
				},
			},
		}
	} else if cfg.ResponseMIMEType == "application/json" {
		openaiReq.ResponseFormat = openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONObject: &shared.ResponseFormatJSONObjectParam{},
		}
	}

	if err := applyTools(openaiReq, cfg); err != nil {
		return err
	}

	if cfg.Temperature != nil {
		openaiReq.Temperature = openai.Float(float64(*cfg.Temperature))
	}
	if cfg.MaxOutputTokens > 0 {
		openaiReq.MaxTokens = openai.Int(int64(cfg.MaxOutputTokens))
	}
	if cfg.TopP != nil {
		openaiReq.TopP = openai.Float(float64(*cfg.TopP))
	}

	return nil
}

// applyTools keeps tools declared under NONE mode (tool_choice "none") so the prompt head and the
// server's prefix cache are unchanged.
func applyTools(openaiReq *openai.ChatCompletionNewParams, cfg *genai.GenerateContentConfig) error {
	if len(cfg.Tools) == 0 {
		return nil
	}
	tools, err := convertTools(cfg.Tools)
	if err != nil {
		return err
	}
	openaiReq.Tools = tools
	if tc := cfg.ToolConfig; tc != nil && tc.FunctionCallingConfig != nil && tc.FunctionCallingConfig.Mode == genai.FunctionCallingConfigModeNone {
		openaiReq.ToolChoice = openai.ChatCompletionToolChoiceOptionUnionParam{OfAuto: openai.String(string(openai.ChatCompletionToolChoiceOptionAutoNone))}
	}
	return nil
}

// leadingToolResponses returns FunctionResponse parts as tool messages and the index past the last one.
func leadingToolResponses(content *genai.Content) ([]openai.ChatCompletionMessageParamUnion, int, error) {
	toolRespMessages := make([]openai.ChatCompletionMessageParamUnion, 0)
	skipIdx := 0
	for idx, part := range content.Parts {
		if part.FunctionResponse != nil {
			responseJSON, err := json.Marshal(part.FunctionResponse.Response)
			if err != nil {
				return nil, 0, fmt.Errorf("failed to marshal function response: %w", err)
			}
			toolRespMessages = append(toolRespMessages,
				openai.ToolMessage(string(responseJSON), part.FunctionResponse.ID))
			skipIdx = idx + 1
			continue
		}
	}
	return toolRespMessages, skipIdx, nil
}

func roleMessage(role, text string) openai.ChatCompletionMessageParamUnion {
	switch role {
	case "assistant":
		return openai.AssistantMessage(text)
	case "system":
		return openai.SystemMessage(text)
	}
	return openai.UserMessage(text)
}

// convertParts returns joined text, user content parts and tool calls; audio and video are rejected.
func convertParts(parts []*genai.Part) (string, []openai.ChatCompletionContentPartUnionParam, []openai.ChatCompletionMessageToolCallUnionParam, error) {
	var texts []string
	var userParts []openai.ChatCompletionContentPartUnionParam
	var toolCalls []openai.ChatCompletionMessageToolCallUnionParam
	for _, part := range parts {
		if part.Text != "" {
			texts = append(texts, part.Text)
			if len(parts) > 1 {
				userParts = append(userParts, openai.TextContentPart(part.Text))
			}
		}
		if part.FunctionCall != nil {
			argsJSON, err := json.Marshal(part.FunctionCall.Args)
			if err != nil {
				return "", nil, nil, fmt.Errorf("failed to marshal function args: %w", err)
			}
			toolCalls = append(toolCalls, openai.ChatCompletionMessageToolCallUnionParam{
				OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
					ID: part.FunctionCall.ID,
					Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
						Name:      part.FunctionCall.Name,
						Arguments: string(argsJSON),
					},
				},
			})
		}
		if part.InlineData != nil {
			switch part.InlineData.MIMEType {
			case "image/jpg", "image/jpeg", "image/png", "image/gif", "image/webp":
				base64Data := base64.StdEncoding.EncodeToString(part.InlineData.Data)
				userParts = append(userParts, openai.ImageContentPart(
					openai.ChatCompletionContentPartImageImageURLParam{
						URL:    fmt.Sprintf("data:%s;base64,%s", part.InlineData.MIMEType, base64Data),
						Detail: "auto",
					},
				))
			case "audio/mpeg", "audio/mp3", "audio/wav", "audio/ogg", "audio/webm":
				return "", nil, nil, fmt.Errorf("unsupported audio MIME type: %s", part.InlineData.MIMEType)
			case "video/mp4", "video/webm", "video/ogg":
				return "", nil, nil, fmt.Errorf("unsupported video MIME type: %s", part.InlineData.MIMEType)
			case "application/pdf":
				// Vision models take images, not documents: one image part per rendered page.
				imgParts, err := pdfToImageParts(part.InlineData.Data)
				if err != nil {
					return "", nil, nil, err
				}
				userParts = append(userParts, imgParts...)
			default:
				userParts = append(userParts, openai.TextContentPart(string(part.InlineData.Data)))
			}
		}
		// FileData has no OpenAI equivalent and is skipped.
	}
	return strings.Join(texts, "\n"), userParts, toolCalls, nil
}

// DropThoughts strips thought parts, copying only the contents it changes; past reasoning is never re-sent.
func DropThoughts(contents []*genai.Content) []*genai.Content {
	var out []*genai.Content
	for i, c := range contents {
		if c == nil || !slices.ContainsFunc(c.Parts, isThought) {
			if out != nil {
				out = append(out, c)
			}
			continue
		}
		if out == nil {
			out = append(make([]*genai.Content, 0, len(contents)), contents[:i]...)
		}
		if kept := slices.DeleteFunc(slices.Clone(c.Parts), isThought); len(kept) > 0 {
			out = append(out, &genai.Content{Role: c.Role, Parts: kept})
		}
	}
	if out == nil {
		return contents
	}
	return out
}

func isThought(p *genai.Part) bool { return p != nil && p.Thought }

func toOpenAIChatCompletionMessage(content *genai.Content) ([]openai.ChatCompletionMessageParamUnion, error) {
	toolRespMessages, skipIdx, err := leadingToolResponses(content)
	if err != nil {
		return nil, err
	}
	parts := content.Parts[skipIdx:]
	if len(parts) == 0 {
		return toolRespMessages, nil
	}
	if len(parts) == 1 && parts[0].Text != "" {
		return append(toolRespMessages, roleMessage(convertRoleToOpenAI(content.Role), parts[0].Text)), nil
	}
	textContent, userParts, toolCalls, err := convertParts(parts)
	if err != nil {
		return nil, err
	}
	role := convertRoleToOpenAI(content.Role)
	if len(toolCalls) > 0 || role == "assistant" {
		// Model-authored text stays assistant-role even when split across parts.
		var assistant openai.ChatCompletionAssistantMessageParam
		if textContent != "" {
			assistant.Content.OfString = openai.String(textContent)
		}
		assistant.ToolCalls = toolCalls
		return append(toolRespMessages, openai.ChatCompletionMessageParamUnion{OfAssistant: &assistant}), nil
	}
	if len(userParts) > 0 {
		return append(toolRespMessages, openai.UserMessage(userParts)), nil
	}
	return append(toolRespMessages, roleMessage(role, textContent)), nil
}

func convertChatCompletionResponse(ctx context.Context, resp *openai.ChatCompletion) (*model.LLMResponse, error) {
	if len(resp.Choices) == 0 {
		return nil, ErrNoChoicesInResponse
	}

	choice := resp.Choices[0]
	content := &genai.Content{
		Role:  genai.RoleModel,
		Parts: []*genai.Part{},
	}

	reasoningText := reasoningContentText(choice.Message)
	if reasoningText != "" {
		content.Parts = append(content.Parts, &genai.Part{Text: reasoningText, Thought: true})
	}
	answerText := choice.Message.Content
	if strings.TrimSpace(answerText) != "" {
		content.Parts = append(content.Parts, &genai.Part{Text: answerText})
	}

	haveToolCalls := len(choice.Message.ToolCalls) > 0
	// Tool-call parts must precede the ladder, or promotion fires on a tool-call turn.
	for _, toolCall := range choice.Message.ToolCalls {
		if toolCall.Type == "function" {
			content.Parts = append(content.Parts, &genai.Part{
				FunctionCall: &genai.FunctionCall{
					ID:   toolCall.ID,
					Name: toolCall.Function.Name,
					Args: parseJSONArgs(toolCall.Function.Arguments),
				},
			})
		}
	}

	var promotedChars int
	content.Parts, _, _, promotedChars = applyFallbackLadder(ctx, resp.Model, content.Parts, haveToolCalls)
	if promotedChars > 0 {
		slog.Warn("promoted reasoning to answer (empty content, reasoning_content held the answer)",
			"component", "inference", "model", resp.Model, "chars", promotedChars)
	}

	// Estimate from post-recovery thinking, so a stripped leaked block doesn't inflate it.
	var finalThought strings.Builder
	for _, p := range content.Parts {
		if p.Thought && p.Text != "" {
			finalThought.WriteString(p.Text)
		}
	}

	usageMetadata := usageMetadataFromResp(ctx, resp, finalThought.String())

	return &model.LLMResponse{
		Content:       content,
		UsageMetadata: usageMetadata,
		FinishReason:  convertFinishReason(choice.FinishReason),
		ModelVersion:  resp.Model,
		TurnComplete:  true,
	}, nil
}

func reasoningContentText(msg openai.ChatCompletionMessage) string {
	return reasoningExtra(msg.JSON.ExtraFields)
}

// reasoningExtra reads raw bytes: openai-go marks untyped ExtraFields "invalid". vLLM >= 0.11 sends
// `reasoning`; llama.cpp and older vLLM send `reasoning_content`.
func reasoningExtra(fields map[string]respjson.Field) string {
	for _, key := range []string{"reasoning_content", "reasoning"} {
		raw := fields[key].Raw()
		if raw == "" || raw == "null" {
			continue
		}
		var text string
		if err := json.Unmarshal([]byte(raw), &text); err == nil && text != "" {
			return text
		}
	}
	return ""
}

// usageMetadataFromResp is nil when the endpoint sent no totals.
func usageMetadataFromResp(ctx context.Context, resp *openai.ChatCompletion, finalThoughtText string) *genai.GenerateContentResponseUsageMetadata {
	if resp.Usage.TotalTokens <= 0 {
		return nil
	}
	candidates, thoughts := reasoningUsage(ctx, resp.Model, int32(resp.Usage.CompletionTokens),
		int32(resp.Usage.CompletionTokensDetails.ReasoningTokens), finalThoughtText)
	return &genai.GenerateContentResponseUsageMetadata{
		PromptTokenCount:        int32(resp.Usage.PromptTokens),
		CandidatesTokenCount:    candidates,
		TotalTokenCount:         int32(resp.Usage.TotalTokens),
		CachedContentTokenCount: int32(resp.Usage.PromptTokensDetails.CachedTokens),
		ThoughtsTokenCount:      thoughts,
	}
}

func convertTools(genaiTools []*genai.Tool) ([]openai.ChatCompletionToolUnionParam, error) {
	var tools []openai.ChatCompletionToolUnionParam

	for _, genaiTool := range genaiTools {
		if genaiTool == nil {
			continue
		}

		converted, err := convertOneTool(genaiTool)
		if err != nil {
			return nil, err
		}
		tools = append(tools, converted...)
	}

	return tools, nil
}

// convertOneTool rejects genai's built-in tools; only function declarations map.
func convertOneTool(genaiTool *genai.Tool) ([]openai.ChatCompletionToolUnionParam, error) {
	var tools []openai.ChatCompletionToolUnionParam

	if genaiTool.GoogleSearch != nil ||
		genaiTool.CodeExecution != nil ||
		genaiTool.FileSearch != nil ||
		genaiTool.Retrieval != nil ||
		genaiTool.ComputerUse != nil {
		return nil, fmt.Errorf("GoogleSearch is not supported")
	}

	for _, funcDecl := range genaiTool.FunctionDeclarations {
		var params shared.FunctionParameters
		if funcDecl.ParametersJsonSchema != nil {
			b, err := json.Marshal(funcDecl.ParametersJsonSchema)
			if err != nil {
				return nil, fmt.Errorf("marshal tool %s schema: %w", funcDecl.Name, err)
			}
			if err := json.Unmarshal(b, &params); err != nil {
				return nil, fmt.Errorf("unmarshal tool %s schema: %w", funcDecl.Name, err)
			}
		}
		if params == nil && funcDecl.Parameters != nil {
			m, err := convertSchema(funcDecl.Parameters)
			if err != nil {
				return nil, err
			}
			params = shared.FunctionParameters(m)
		}
		if params == nil {
			params = shared.FunctionParameters{
				"type":       "object",
				"properties": map[string]any{},
			}
		}

		tools = append(tools, openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
			Name:        funcDecl.Name,
			Description: openai.String(funcDecl.Description),
			Parameters:  params,
		}))
	}

	return tools, nil
}

// convertSchema marshals a genai.Schema to JSON Schema; genai's types are upper-case enums.
func convertSchema(schema *genai.Schema) (map[string]any, error) {
	b, err := json.Marshal(schema)
	if err != nil {
		return nil, fmt.Errorf("marshal schema: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("unmarshal schema: %w", err)
	}
	lowercaseTypes(m)
	return m, nil
}

func lowercaseTypes(v any) {
	switch v := v.(type) {
	case map[string]any:
		if t, ok := v["type"].(string); ok {
			v["type"] = strings.ToLower(t)
		}
		for _, c := range v {
			lowercaseTypes(c)
		}
	case []any:
		for _, c := range v {
			lowercaseTypes(c)
		}
	}
}

func convertRoleToOpenAI(role string) string {
	switch role {
	case "user":
		return "user"
	case "model":
		return "assistant"
	case "system":
		return "system"
	default:
		return "user"
	}
}

func convertFinishReason(reason string) genai.FinishReason {
	switch reason {
	case "stop":
		return genai.FinishReasonStop
	case "length":
		return genai.FinishReasonMaxTokens
	case "tool_calls", "function_call":
		return genai.FinishReasonStop
	case "content_filter":
		return genai.FinishReasonSafety
	default:
		return genai.FinishReasonUnspecified
	}
}

func extractTextFromContent(content *genai.Content) string {
	if content == nil {
		return ""
	}
	var texts []string
	for _, part := range content.Parts {
		if part.Text != "" {
			texts = append(texts, part.Text)
		}
	}
	return strings.Join(texts, "\n")
}

func parseJSONArgs(argsJSON string) map[string]any {
	if argsJSON == "" {
		return make(map[string]any)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return make(map[string]any)
	}
	return args
}

var toolCallRe = regexp.MustCompile(`(?s)<tool_call>\s*(\{.*?\})\s*</tool_call>`)

// toolCallXMLRe matches qwen's <tool_call><function=x><parameter=k>v</parameter></function> leak.
var toolCallXMLRe = regexp.MustCompile(`(?s)<tool_call>\s*<function=([^>]+)>(.*?)</function>\s*</tool_call>`)

// bareFunctionRe is the unwrapped form, seen leaking into answer content. Alone it misfires on prose;
// reasoningToolCalls guards the body.
var bareFunctionRe = regexp.MustCompile(`(?s)<function=([^>]+)>(.*?)</function>`)

// paramRe values may span lines.
var paramRe = regexp.MustCompile(`(?s)<parameter=([^>]+)>\s*(.*?)\s*</parameter>`)

func parseXMLParams(body string) map[string]any {
	args := map[string]any{}
	for _, pm := range paramRe.FindAllStringSubmatch(body, -1) {
		raw := strings.TrimSpace(pm[2])
		var v any
		// JSON values keep their type, as in qwen-agent's converter; the rest stay strings.
		if json.Unmarshal([]byte(raw), &v) == nil {
			args[strings.TrimSpace(pm[1])] = v
		} else {
			args[strings.TrimSpace(pm[1])] = raw
		}
	}
	return args
}

// reasoningToolCalls recovers calls leaked as text (Hermes JSON, qwen wrapped, qwen bare) and
// returns them with the matched blocks removed.
func reasoningToolCalls(reasoning string) ([]*genai.FunctionCall, string) {
	var calls []*genai.FunctionCall

	for _, m := range toolCallRe.FindAllStringSubmatch(reasoning, -1) {
		var tc struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if json.Unmarshal([]byte(m[1]), &tc) != nil || tc.Name == "" {
			continue
		}
		calls = append(calls, &genai.FunctionCall{
			ID:   recoveredCallID(len(calls), tc.Name),
			Name: tc.Name,
			Args: tc.Arguments,
		})
	}

	for _, m := range toolCallXMLRe.FindAllStringSubmatch(reasoning, -1) {
		name := strings.TrimSpace(m[1])
		if name == "" {
			continue
		}
		calls = append(calls, &genai.FunctionCall{
			ID:   recoveredCallID(len(calls), name),
			Name: name,
			Args: parseXMLParams(m[2]),
		})
	}

	// Strip the wrapped forms first so a block isn't counted twice.
	withoutWrapped := toolCallXMLRe.ReplaceAllString(toolCallRe.ReplaceAllString(reasoning, ""), "")

	cleaned := bareFunctionRe.ReplaceAllStringFunc(withoutWrapped, func(block string) string {
		bm := bareFunctionRe.FindStringSubmatch(block)
		name := strings.TrimSpace(bm[1])
		body := bm[2]
		// Prose mentioning "<function=" fails this: the body must be only parameter blocks (or empty).
		if name == "" || strings.TrimSpace(paramRe.ReplaceAllString(body, "")) != "" {
			return block
		}
		calls = append(calls, &genai.FunctionCall{
			ID:   recoveredCallID(len(calls), name),
			Name: name,
			Args: parseXMLParams(body),
		})
		return ""
	})

	if len(calls) == 0 {
		return nil, reasoning
	}
	return calls, cleaned
}

// recoveredCallID is unique: an id reused across turns pairs a later result with an earlier call.
func recoveredCallID(n int, name string) string {
	return fmt.Sprintf("rtc_%d_%s_%s", n, name, rand.Text()[:10])
}
