// Package openaimodel is a vendored + modified copy of github.com/byebyebruce/adk-go-openai,
// adapting an OpenAI-compatible endpoint to ADK's model.LLM. Modifications: reasoning_content
// is surfaced as Thought parts (both streaming and non-streaming) so the UI can render thinking.
// Now uses github.com/openai/openai-go/v3 (official SDK) for input_audio support in Phase 2+.
// Upstream is MIT-licensed.
package openaimodel

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"regexp"
	"sort"
	"strings"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

var _ model.LLM = &OpenAIModel{}

var (
	ErrNoChoicesInResponse   = errors.New("no choices in OpenAI response")
	ErrUnknownPartInResponse = errors.New("unknown part type in genai content")
)

type OpenAIModel struct {
	client    openai.Client
	ModelName string
}

func NewOpenAIModel(modelName, endpoint, apiKey string) *OpenAIModel {
	client := openai.NewClient(
		option.WithBaseURL(endpoint),
		option.WithAPIKey(apiKey),
	)
	return &OpenAIModel{
		client:    client,
		ModelName: modelName,
	}
}

// Name implements model.LLM.
func (o *OpenAIModel) Name() string {
	return o.ModelName
}

// apiErr logs an OpenAI-compatible API failure with the model's HTTP status and
// response body, then returns an enriched error. The log is the load-bearing part:
// ADK's runner catches a sub-agent's yielded error and can hand the caller empty
// output with no error (see the adk-swallows-subagent-errors finding), so without
// a log at THIS boundary a model 400 (e.g. context/tool/format) vanishes silently.
func (o *OpenAIModel) apiErr(ctx context.Context, op string, err error) error {
	var ae *openai.Error
	if errors.As(err, &ae) {
		slog.ErrorContext(ctx, "openai API error", "component", "inference",
			"model", o.ModelName, "op", op, "status", ae.StatusCode, "body", ae.Error())
		return fmt.Errorf("openai %s (%s): status %d: %s", o.ModelName, op, ae.StatusCode, ae.Error())
	}
	slog.ErrorContext(ctx, "openai request failed", "component", "inference",
		"model", o.ModelName, "op", op, "err", err)
	return fmt.Errorf("openai %s (%s): %w", o.ModelName, op, err)
}

// Embed returns one embedding vector per input text, in input order, from the
// OpenAI-compatible /embeddings endpoint. It uses o.ModelName as the embedding
// model, so an OpenAIModel constructed with an embedding model name doubles as an
// embedder (the same client/endpoint serves both).
func (o *OpenAIModel) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	resp, err := o.client.Embeddings.New(ctx, openai.EmbeddingNewParams{
		Model: openai.EmbeddingModel(o.ModelName),
		Input: openai.EmbeddingNewParamsInputUnion{OfArrayOfStrings: texts},
	})
	if err != nil {
		return nil, err
	}
	if len(resp.Data) != len(texts) {
		return nil, fmt.Errorf("openaimodel: embeddings returned %d vectors for %d inputs", len(resp.Data), len(texts))
	}
	// Place each vector by its declared index — the API need not return them in order.
	out := make([][]float32, len(texts))
	for _, e := range resp.Data {
		if e.Index < 0 || int(e.Index) >= len(out) {
			return nil, fmt.Errorf("openaimodel: embedding index %d out of range for %d inputs", e.Index, len(texts))
		}
		v := make([]float32, len(e.Embedding))
		for i, f := range e.Embedding {
			v[i] = float32(f)
		}
		out[e.Index] = v
	}
	return out, nil
}

// GenerateContent implements model.LLM.
func (o *OpenAIModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	if stream {
		return o.generateStream(ctx, req)
	}
	return o.generate(ctx, req)
}

func (o *OpenAIModel) generate(ctx context.Context, req *model.LLMRequest) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		openaiReq, err := toOpenAIChatCompletionRequest(req, o.ModelName)
		if err != nil {
			yield(nil, err)
			return
		}

		resp, err := o.client.Chat.Completions.New(ctx, openaiReq)
		if err != nil {
			yield(nil, o.apiErr(ctx, "generate", err))
			return
		}

		llmResp, err := convertChatCompletionResponse(resp)
		if err != nil {
			yield(nil, err)
			return
		}

		yield(llmResp, nil)
	}
}

func (o *OpenAIModel) generateStream(ctx context.Context, req *model.LLMRequest) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		openaiReq, err := toOpenAIChatCompletionRequest(req, o.ModelName)
		if err != nil {
			yield(nil, err)
			return
		}
		openaiReq.StreamOptions = openai.ChatCompletionStreamOptionsParam{
			IncludeUsage: openai.Bool(true),
		}

		stream := o.client.Chat.Completions.NewStreaming(ctx, openaiReq)
		defer stream.Close()

		aggregatedContent := &genai.Content{
			Role:  "model",
			Parts: []*genai.Part{},
		}
		var finishReason genai.FinishReason
		var usageMetadata *genai.GenerateContentResponseUsageMetadata

		// Track tool calls by index to properly aggregate them across chunks.
		toolCallsMap := make(map[int64]*toolCallBuilder)

		var modelVersion string
		lastPartIsText := false

		for stream.Next() {
			chunk := stream.Current()

			if chunk.Model != "" {
				modelVersion = chunk.Model
			}

			// Capture usage — present on the final usage-only chunk when IncludeUsage is set.
			if chunk.Usage.TotalTokens > 0 {
				usageMetadata = &genai.GenerateContentResponseUsageMetadata{
					PromptTokenCount:        int32(chunk.Usage.PromptTokens),
					CandidatesTokenCount:    int32(chunk.Usage.CompletionTokens),
					TotalTokenCount:         int32(chunk.Usage.TotalTokens),
					CachedContentTokenCount: int32(chunk.Usage.PromptTokensDetails.CachedTokens),
				}
			}

			if len(chunk.Choices) == 0 {
				continue
			}

			choice := chunk.Choices[0]

			// Handle delta content.
			if choice.Delta.Content != "" {
				part := &genai.Part{Text: choice.Delta.Content}
				if lastPartIsText {
					aggregatedContent.Parts[len(aggregatedContent.Parts)-1].Text += part.Text
				} else {
					aggregatedContent.Parts = append(aggregatedContent.Parts, part)
				}
				lastPartIsText = true
				llmResp := &model.LLMResponse{
					Content:      &genai.Content{Role: "model", Parts: []*genai.Part{part}},
					Partial:      true,
					TurnComplete: false,
				}
				if !yield(llmResp, nil) {
					return
				}
			} else {
				lastPartIsText = false
			}

			// Surface reasoning_content as a Thought part so the UI can render thinking.
			// openai-go marks untyped ExtraFields as status "invalid" (no typed extras
			// decoder is registered for this struct), so Valid() is always false here —
			// gate on the raw bytes instead, the way an omitted/null field already does.
			if rc := choice.Delta.JSON.ExtraFields["reasoning_content"]; rc.Raw() != "" {
				if raw := rc.Raw(); raw != "" && raw != "null" {
					var text string
					if jsonErr := json.Unmarshal([]byte(raw), &text); jsonErr == nil && text != "" {
						part := &genai.Part{Text: text, Thought: true}
						aggregatedContent.Parts = append(aggregatedContent.Parts, part)
						lastPartIsText = false
						llmResp := &model.LLMResponse{
							Content:      &genai.Content{Role: "model", Parts: []*genai.Part{part}},
							Partial:      true,
							TurnComplete: false,
						}
						if !yield(llmResp, nil) {
							return
						}
					}
				}
			}

			// Handle tool calls in delta — aggregate across chunks keyed by index.
			for _, toolCall := range choice.Delta.ToolCalls {
				idx := toolCall.Index
				builder, exists := toolCallsMap[idx]
				if !exists {
					builder = &toolCallBuilder{}
					toolCallsMap[idx] = builder
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
				finishReason = convertFinishReason(choice.FinishReason)
			}
		}

		if err := stream.Err(); err != nil {
			// The streaming path is what the agents use, so this is where a model
			// 400 (context/tool/format) actually surfaces — log status+body here.
			yield(nil, o.apiErr(ctx, "generate_stream", err))
			return
		}

		// Emit aggregated tool calls as FunctionCall parts, ordered by index.
		if len(toolCallsMap) > 0 {
			indices := make([]int64, 0, len(toolCallsMap))
			for idx := range toolCallsMap {
				indices = append(indices, idx)
			}
			sort.Slice(indices, func(i, j int) bool { return indices[i] < indices[j] })
			for _, idx := range indices {
				b := toolCallsMap[idx]
				aggregatedContent.Parts = append(aggregatedContent.Parts, &genai.Part{
					FunctionCall: &genai.FunctionCall{
						ID:   b.id,
						Name: b.name,
						Args: parseJSONArgs(b.args),
					},
				})
			}
		}

		// Qwen3.x streams tool calls inside reasoning_content as <tool_call> XML
		// instead of delta.tool_calls (llama.cpp#22684). When no proper tool calls
		// arrived, recover them from the thinking so the agent acts on them instead
		// of stalling on an empty turn (the empty-node bug).
		recoveredFromThought := false
		if len(toolCallsMap) == 0 {
			var rb strings.Builder
			for _, p := range aggregatedContent.Parts {
				if p.Thought && p.Text != "" {
					rb.WriteString(p.Text)
				}
			}
			if calls, cleaned := reasoningToolCalls(rb.String()); len(calls) > 0 {
				// A leaked block can span several streamed thought parts, so per-part
				// regex stripping leaves residue — re-emit the cleaned reasoning as
				// one thought part in place of the originals.
				rebuilt := make([]*genai.Part, 0, len(aggregatedContent.Parts)+len(calls))
				thoughtReplaced := false
				for _, p := range aggregatedContent.Parts {
					if p.Thought && p.Text != "" {
						if !thoughtReplaced {
							thoughtReplaced = true
							if strings.TrimSpace(cleaned) != "" {
								rebuilt = append(rebuilt, &genai.Part{Text: cleaned, Thought: true})
							}
						}
						continue
					}
					rebuilt = append(rebuilt, p)
				}
				aggregatedContent.Parts = rebuilt
				for _, c := range calls {
					aggregatedContent.Parts = append(aggregatedContent.Parts, &genai.Part{FunctionCall: c})
				}
				recoveredFromThought = true
				slog.WarnContext(ctx, "recovered tool calls from reasoning_content (Qwen/llama.cpp#22684)",
					"component", "inference", "model", o.ModelName, "count", len(calls))
			}
		}

		// #427: the same leak can land in the plain answer text instead of
		// reasoning_content (ask_advisor leaked as literal "<function=…>" in
		// content) — scan that too when nothing proper or recovered above.
		if len(toolCallsMap) == 0 && !recoveredFromThought {
			var cb strings.Builder
			for _, p := range aggregatedContent.Parts {
				if !p.Thought && p.Text != "" {
					cb.WriteString(p.Text)
				}
			}
			if calls, cleaned := reasoningToolCalls(cb.String()); len(calls) > 0 {
				rebuilt := make([]*genai.Part, 0, len(aggregatedContent.Parts)+len(calls))
				contentReplaced := false
				for _, p := range aggregatedContent.Parts {
					if !p.Thought && p.Text != "" {
						if !contentReplaced {
							contentReplaced = true
							if strings.TrimSpace(cleaned) != "" {
								rebuilt = append(rebuilt, &genai.Part{Text: cleaned})
							}
						}
						continue
					}
					rebuilt = append(rebuilt, p)
				}
				aggregatedContent.Parts = rebuilt
				for _, c := range calls {
					aggregatedContent.Parts = append(aggregatedContent.Parts, &genai.Part{FunctionCall: c})
				}
				slog.WarnContext(ctx, "recovered tool calls leaked into answer content (bare <function=> form, #427)",
					"component", "inference", "model", o.ModelName, "count", len(calls))
			}
		}

		if modelVersion == "" {
			modelVersion = string(openaiReq.Model)
		}
		// Reasoning-model failure mode: a turn with neither answer text nor a tool
		// call (the model often spends its whole output budget thinking and hits the
		// length limit). Otherwise invisible — it surfaces downstream only as a
		// mysteriously empty node — so log finish_reason + whether it was thinking.
		hasAnswer, hadThinking := false, false
		for _, p := range aggregatedContent.Parts {
			switch {
			case p.FunctionCall != nil, !p.Thought && p.Text != "":
				hasAnswer = true
			case p.Thought && p.Text != "":
				hadThinking = true
			}
		}
		if !hasAnswer {
			// Content-side of #22684: the model wrote its answer inside an unclosed
			// <think>, so it landed in reasoning_content and content came back empty.
			// Promote the thinking to the answer rather than emit an empty turn — a
			// reasoning-only turn is terminal anyway (nothing for the agent to act on),
			// and the judge/revise gates its quality. Only tool-less answer turns reach
			// here; tool calls were already recovered above.
			if hadThinking {
				var rb strings.Builder
				for _, p := range aggregatedContent.Parts {
					if p.Thought && p.Text != "" {
						rb.WriteString(p.Text)
					}
				}
				if txt := strings.TrimSpace(rb.String()); txt != "" {
					aggregatedContent.Parts = append(aggregatedContent.Parts, &genai.Part{Text: txt})
					hasAnswer = true
					slog.WarnContext(ctx, "promoted reasoning to answer (empty content, unclosed </think>)",
						"component", "inference", "model", o.ModelName, "chars", len(txt))
				}
			}
			if !hasAnswer {
				var compl int32
				if usageMetadata != nil {
					compl = usageMetadata.CandidatesTokenCount
				}
				slog.WarnContext(ctx, "model returned no answer content (empty turn)",
					"component", "inference", "model", o.ModelName, "finish_reason", string(finishReason),
					"had_thinking", hadThinking, "completion_tokens", compl)
			}
		}
		yield(&model.LLMResponse{
			Content:       aggregatedContent,
			UsageMetadata: usageMetadata,
			FinishReason:  finishReason,
			ModelVersion:  modelVersion,
			Partial:       false,
			TurnComplete:  true,
		}, nil)
	}
}

// logRequestTail logs, at Debug, the shape of the request the model actually
// receives: content count and the last 12 entries as role/kind (CALL:name /
// RESP:name(bytes) / text). This is the ground truth for loop and compaction
// diagnosis — "is the tool result the model should act on actually IN the
// request?" — which the #252 investigation could otherwise only answer by
// shipping a temporary instrumented image. QUACK_LOG_LEVEL=debug turns it on.
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

// toolCallBuilder helps aggregate tool call information across streaming chunks.
type toolCallBuilder struct {
	id   string
	name string
	args string
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

	if req.Config.ThinkingConfig != nil {
		switch req.Config.ThinkingConfig.ThinkingLevel {
		case genai.ThinkingLevelLow:
			openaiReq.ReasoningEffort = "low"
		case genai.ThinkingLevelHigh:
			openaiReq.ReasoningEffort = "high"
		default:
			openaiReq.ReasoningEffort = "medium"
		}
	}

	if req.Config.ResponseSchema != nil {
		openaiReq.ResponseFormat = openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{
				JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{
					Name:   "response",
					Strict: openai.Bool(true),
					Schema: req.Config.ResponseSchema,
				},
			},
		}
	} else if req.Config.ResponseMIMEType == "application/json" {
		openaiReq.ResponseFormat = openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONObject: &shared.ResponseFormatJSONObjectParam{},
		}
	}

	if len(req.Config.Tools) > 0 {
		tools, err := convertTools(req.Config.Tools)
		if err != nil {
			return openai.ChatCompletionNewParams{}, err
		}
		openaiReq.Tools = tools
	}

	if req.Config.Temperature != nil {
		openaiReq.Temperature = openai.Float(float64(*req.Config.Temperature))
	}
	if req.Config.MaxOutputTokens > 0 {
		openaiReq.MaxTokens = openai.Int(int64(req.Config.MaxOutputTokens))
	}
	if req.Config.TopP != nil {
		openaiReq.TopP = openai.Float(float64(*req.Config.TopP))
	}
	if len(req.Config.StopSequences) > 0 {
		openaiReq.Stop = openai.ChatCompletionNewParamsStopUnion{
			OfStringArray: req.Config.StopSequences,
		}
	}

	if req.Config.SystemInstruction != nil {
		sysMsg := openai.SystemMessage(extractTextFromContent(req.Config.SystemInstruction))
		openaiReq.Messages = append([]openai.ChatCompletionMessageParamUnion{sysMsg}, openaiReq.Messages...)
	}

	return openaiReq, nil
}

func toOpenAIChatCompletionMessage(content *genai.Content) ([]openai.ChatCompletionMessageParamUnion, error) {
	// Collect leading FunctionResponse parts as individual tool messages.
	toolRespMessages := make([]openai.ChatCompletionMessageParamUnion, 0)
	skipIdx := 0
	for idx, part := range content.Parts {
		if part.FunctionResponse != nil {
			responseJSON, err := json.Marshal(part.FunctionResponse.Response)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal function response: %w", err)
			}
			toolRespMessages = append(toolRespMessages,
				openai.ToolMessage(string(responseJSON), part.FunctionResponse.ID))
			skipIdx = idx + 1
			continue
		}
	}

	parts := content.Parts[skipIdx:]
	if len(parts) == 0 {
		return toolRespMessages, nil
	}

	// Simple case: single text part — use string variant of the message constructor.
	if len(parts) == 1 && parts[0].Text != "" {
		role := convertRoleToOpenAI(content.Role)
		var msg openai.ChatCompletionMessageParamUnion
		switch role {
		case "assistant":
			msg = openai.AssistantMessage(parts[0].Text)
		case "system":
			msg = openai.SystemMessage(parts[0].Text)
		default:
			msg = openai.UserMessage(parts[0].Text)
		}
		return append(toolRespMessages, msg), nil
	}

	// Complex case: multiple parts or special part types (tool calls, images, etc.).
	var textContent string
	var userParts []openai.ChatCompletionContentPartUnionParam
	var toolCalls []openai.ChatCompletionMessageToolCallUnionParam

	for _, part := range parts {
		if part.Text != "" {
			if len(parts) == 1 {
				textContent = part.Text
			} else {
				userParts = append(userParts, openai.TextContentPart(part.Text))
			}
		}

		if part.FunctionCall != nil {
			argsJSON, err := json.Marshal(part.FunctionCall.Args)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal function args: %w", err)
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
				return nil, fmt.Errorf("unsupported audio MIME type: %s", part.InlineData.MIMEType)
			case "video/mp4", "video/webm", "video/ogg":
				return nil, fmt.Errorf("unsupported video MIME type: %s", part.InlineData.MIMEType)
			case "application/pdf":
				return nil, fmt.Errorf("unsupported PDF MIME type: %s", part.InlineData.MIMEType)
			default:
				userParts = append(userParts, openai.TextContentPart(string(part.InlineData.Data)))
			}
		}

		// FileData: OpenAI doesn't support file references directly; skip for now.
	}

	role := convertRoleToOpenAI(content.Role)

	if len(toolCalls) > 0 {
		// Assistant message carrying tool calls (and optional text).
		var assistant openai.ChatCompletionAssistantMessageParam
		if textContent != "" {
			assistant.Content.OfString = openai.String(textContent)
		}
		assistant.ToolCalls = toolCalls
		return append(toolRespMessages, openai.ChatCompletionMessageParamUnion{OfAssistant: &assistant}), nil
	}

	if len(userParts) > 0 {
		// Multi-part user message (e.g. text + image).
		return append(toolRespMessages, openai.UserMessage(userParts)), nil
	}

	// Fallback: plain text message.
	var msg openai.ChatCompletionMessageParamUnion
	switch role {
	case "assistant":
		msg = openai.AssistantMessage(textContent)
	case "system":
		msg = openai.SystemMessage(textContent)
	default:
		msg = openai.UserMessage(textContent)
	}
	return append(toolRespMessages, msg), nil
}

func convertChatCompletionResponse(resp *openai.ChatCompletion) (*model.LLMResponse, error) {
	if len(resp.Choices) == 0 {
		return nil, ErrNoChoicesInResponse
	}

	choice := resp.Choices[0]
	content := &genai.Content{
		Role:  genai.RoleModel,
		Parts: []*genai.Part{},
	}

	// Surface reasoning_content as a Thought part (reasoning precedes the answer).
	// openai-go marks untyped ExtraFields as status "invalid" (no typed extras
	// decoder is registered for this struct), so Valid() is always false here —
	// gate on the raw bytes instead, the way an omitted/null field already does.
	var reasoningText string
	if rc := choice.Message.JSON.ExtraFields["reasoning_content"]; rc.Raw() != "" {
		if raw := rc.Raw(); raw != "" && raw != "null" {
			var text string
			if err := json.Unmarshal([]byte(raw), &text); err == nil && text != "" {
				reasoningText = text
			}
		}
	}

	// Same recovery as the streaming path: qwen leaks <tool_call> XML into
	// reasoning_content. Remap it into real function calls BEFORE the
	// empty-content fallback below, so the raw XML is never promoted to the answer.
	var recovered []*genai.FunctionCall
	if reasoningText != "" && len(choice.Message.ToolCalls) == 0 {
		var cleaned string
		if recovered, cleaned = reasoningToolCalls(reasoningText); len(recovered) > 0 {
			reasoningText = cleaned
			slog.Warn("recovered tool calls from reasoning_content (Qwen/llama.cpp#22684)",
				"component", "inference", "model", resp.Model, "count", len(recovered))
		}
	}
	if reasoningText != "" {
		content.Parts = append(content.Parts, &genai.Part{Text: reasoningText, Thought: true})
	}

	// #427: the same leak can land in the plain answer content instead of
	// reasoning_content (ask_advisor leaked as literal "<function=…>" text in
	// the answer) — scan it too when no proper/recovered tool call exists yet.
	answerText := choice.Message.Content
	var recoveredFromContent []*genai.FunctionCall
	if len(choice.Message.ToolCalls) == 0 && len(recovered) == 0 && strings.TrimSpace(answerText) != "" {
		var cleaned string
		if recoveredFromContent, cleaned = reasoningToolCalls(answerText); len(recoveredFromContent) > 0 {
			answerText = cleaned
			slog.Warn("recovered tool calls leaked into answer content (bare <function=> form, #427)",
				"component", "inference", "model", resp.Model, "count", len(recoveredFromContent))
		}
	}

	if strings.TrimSpace(answerText) != "" {
		content.Parts = append(content.Parts, &genai.Part{Text: answerText})
	} else if reasoningText != "" && len(choice.Message.ToolCalls) == 0 && len(recovered) == 0 && len(recoveredFromContent) == 0 {
		// Content-side of #22684 (non-streaming path): the synthesized answer
		// sometimes lands entirely inside reasoning_content, leaving content
		// empty. Promote the reasoning to the answer instead of dropping it — a
		// reasoning-only turn is terminal anyway, and the judge/revise gate still
		// evaluates its quality. Skip when tool calls arrived; those already make
		// the turn non-terminal.
		slog.Warn("promoted reasoning to answer (empty content, reasoning_content held the answer)",
			"component", "inference", "model", resp.Model, "chars", len(reasoningText))
		content.Parts = append(content.Parts, &genai.Part{Text: reasoningText})
	}

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
	for _, c := range recovered {
		content.Parts = append(content.Parts, &genai.Part{FunctionCall: c})
	}
	for _, c := range recoveredFromContent {
		content.Parts = append(content.Parts, &genai.Part{FunctionCall: c})
	}

	var usageMetadata *genai.GenerateContentResponseUsageMetadata
	if resp.Usage.TotalTokens > 0 {
		usageMetadata = &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:        int32(resp.Usage.PromptTokens),
			CandidatesTokenCount:    int32(resp.Usage.CompletionTokens),
			TotalTokenCount:         int32(resp.Usage.TotalTokens),
			CachedContentTokenCount: int32(resp.Usage.PromptTokensDetails.CachedTokens),
		}
	}

	return &model.LLMResponse{
		Content:       content,
		UsageMetadata: usageMetadata,
		FinishReason:  convertFinishReason(choice.FinishReason),
		ModelVersion:  resp.Model,
		TurnComplete:  true,
	}, nil
}

func convertTools(genaiTools []*genai.Tool) ([]openai.ChatCompletionToolUnionParam, error) {
	var tools []openai.ChatCompletionToolUnionParam

	for _, genaiTool := range genaiTools {
		if genaiTool == nil {
			continue
		}

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
				// Tool has no declared parameters — use an empty object schema.
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
	}

	return tools, nil
}

func convertSchema(schema *genai.Schema) (map[string]any, error) {
	if schema == nil {
		return map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		}, nil
	}

	result := make(map[string]any)

	if schema.Type != genai.TypeUnspecified {
		result["type"] = convertSchemaType(schema.Type)
	}

	if schema.Description != "" {
		result["description"] = schema.Description
	}

	if len(schema.Properties) > 0 {
		properties := make(map[string]any)
		for propName, propSchema := range schema.Properties {
			convertedProp, err := convertSchema(propSchema)
			if err != nil {
				return nil, err
			}
			properties[propName] = convertedProp
		}
		result["properties"] = properties
	}

	if len(schema.Required) > 0 {
		result["required"] = schema.Required
	}

	if schema.Items != nil {
		items, err := convertSchema(schema.Items)
		if err != nil {
			return nil, err
		}
		result["items"] = items
	}

	if len(schema.Enum) > 0 {
		result["enum"] = schema.Enum
	}

	return result, nil
}

func convertSchemaType(t genai.Type) string {
	switch t {
	case genai.TypeString:
		return "string"
	case genai.TypeNumber:
		return "number"
	case genai.TypeInteger:
		return "integer"
	case genai.TypeBoolean:
		return "boolean"
	case genai.TypeArray:
		return "array"
	case genai.TypeObject:
		return "object"
	default:
		return "string"
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

// toolCallRe matches a Hermes-style <tool_call>{json}</tool_call> block.
var toolCallRe = regexp.MustCompile(`(?s)<tool_call>\s*(\{.*?\})\s*</tool_call>`)

// toolCallXMLRe matches qwen's other leak format inside <tool_call>:
//
//	<function=web_fetch>
//	<parameter=url>
//	https://…
//	</parameter>
//	</function>
var toolCallXMLRe = regexp.MustCompile(`(?s)<tool_call>\s*<function=([^>]+)>(.*?)</function>\s*</tool_call>`)

// bareFunctionRe matches the same qwen <function=…> shape WITHOUT the
// <tool_call> wrapper — seen when a call leaks straight into the assistant's
// content instead of reasoning_content (#427: ask_advisor leaked as literal
// "<function=ask_advisor>…" text in the answer). Matched blocks are only
// treated as real calls once their body passes the parameter-block guard in
// reasoningToolCalls — this regex alone is not enough to avoid misfiring on
// prose that merely mentions "<function=" in passing.
var bareFunctionRe = regexp.MustCompile(`(?s)<function=([^>]+)>(.*?)</function>`)

// paramRe matches one <parameter=name>value</parameter> entry; values may span lines.
var paramRe = regexp.MustCompile(`(?s)<parameter=([^>]+)>\s*(.*?)\s*</parameter>`)

// parseXMLParams extracts <parameter=name>value</parameter> entries from a
// <function=…> body into call args, shared by the wrapped and bare recovery paths.
func parseXMLParams(body string) map[string]any {
	args := map[string]any{}
	for _, pm := range paramRe.FindAllStringSubmatch(body, -1) {
		raw := strings.TrimSpace(pm[2])
		var v any
		// JSON-typed values (numbers, bools, objects) keep their type, as
		// in qwen-agent's own converter; anything unparseable stays a string.
		if json.Unmarshal([]byte(raw), &v) == nil {
			args[strings.TrimSpace(pm[1])] = v
		} else {
			args[strings.TrimSpace(pm[1])] = raw
		}
	}
	return args
}

// reasoningToolCalls recovers tool calls that a model leaked as literal XML
// instead of proper delta.tool_calls — Qwen3.x's <tool_call> wrapper
// (llama.cpp#22684, closed not-planned — so the client must parse them) plus
// the bare <function=…> form seen leaking straight into content (#427,
// recurrence of #402). Handles, in order: Hermes JSON
// (<tool_call>{...}</tool_call>), qwen's wrapped <function>/<parameter> form,
// and the same form without the <tool_call> wrapper. Without this the agent
// sees no tool call — either an empty answer (stalls the node) or the raw XML
// shown to the user. Returns the parsed calls and the text with matched
// blocks removed.
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
			ID:   fmt.Sprintf("rtc_%d_%s", len(calls), tc.Name),
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
			ID:   fmt.Sprintf("rtc_%d_%s", len(calls), name),
			Name: name,
			Args: parseXMLParams(m[2]),
		})
	}

	// Strip the two wrapped forms before scanning for bare <function=…>
	// blocks, so one already counted above isn't double-counted below.
	withoutWrapped := toolCallXMLRe.ReplaceAllString(toolCallRe.ReplaceAllString(reasoning, ""), "")

	cleaned := bareFunctionRe.ReplaceAllStringFunc(withoutWrapped, func(block string) string {
		bm := bareFunctionRe.FindStringSubmatch(block)
		name := strings.TrimSpace(bm[1])
		body := bm[2]
		// Guard against prose that merely mentions "<function=…>" in passing:
		// the body must be fully accounted for by <parameter=…> blocks (or
		// empty) — natural-language text between the tags fails this check
		// and the block is left untouched as ordinary text.
		if name == "" || strings.TrimSpace(paramRe.ReplaceAllString(body, "")) != "" {
			return block
		}
		calls = append(calls, &genai.FunctionCall{
			ID:   fmt.Sprintf("rtc_%d_%s", len(calls), name),
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
