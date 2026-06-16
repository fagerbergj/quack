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
	"sort"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
	"google.golang.org/adk/model"
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
			yield(nil, err)
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
			if rc := choice.Delta.JSON.ExtraFields["reasoning_content"]; rc.Valid() {
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
			yield(nil, err)
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

		if modelVersion == "" {
			modelVersion = string(openaiReq.Model)
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
	if rc := choice.Message.JSON.ExtraFields["reasoning_content"]; rc.Valid() {
		if raw := rc.Raw(); raw != "" && raw != "null" {
			var text string
			if err := json.Unmarshal([]byte(raw), &text); err == nil && text != "" {
				content.Parts = append(content.Parts, &genai.Part{Text: text, Thought: true})
			}
		}
	}

	if choice.Message.Content != "" {
		content.Parts = append(content.Parts, &genai.Part{Text: choice.Message.Content})
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
	return joinTexts(texts)
}

func joinTexts(texts []string) string {
	if len(texts) == 0 {
		return ""
	}
	if len(texts) == 1 {
		return texts[0]
	}
	result := ""
	for i, text := range texts {
		if i > 0 {
			result += "\n"
		}
		result += text
	}
	return result
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
