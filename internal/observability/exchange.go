package observability

import (
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/z-chenhao/eri/internal/content"
	"github.com/z-chenhao/eri/internal/secret"
)

const maxProjectedExchangeBytes = 64 * 1024

var sensitiveExchangeKeys = map[string]bool{
	"authorization": true, "api_key": true, "apikey": true, "access_token": true,
	"refresh_token": true, "session_token": true, "token": true, "password": true,
	"passwd": true, "secret": true, "cookie": true, "set_cookie": true,
}

func (s *Service) hydrateEffectExchanges(ctx context.Context, detail *RunDetail) error {
	if s.content == nil {
		return nil
	}
	for index := range detail.Effects {
		effect := &detail.Effects[index]
		request, requestState, err := s.projectContent(ctx, effect.PayloadRef)
		if err != nil {
			return err
		}
		response, responseState, err := s.projectContent(ctx, effect.ResultRef)
		if err != nil {
			return err
		}
		if effect.ResultRef.ObjectID == "" {
			response = map[string]any{"status": effect.Status, "error_code": effect.ErrorCode}
			responseState = "No confirmed result body is available."
		}
		effect.Exchange = &CallExchange{
			Request: request, Response: response,
			Disclosure: "Governed Tool request and confirmed response. Credential-like fields are redacted and large bodies are truncated. " + requestState + " " + responseState,
		}
	}
	return nil
}

func (s *Service) projectContent(ctx context.Context, ref content.Ref) (any, string, error) {
	if ref.ObjectID == "" {
		return nil, "Body not recorded.", nil
	}
	body, err := s.content.Get(ctx, ref)
	if err != nil {
		return nil, "", err
	}
	truncated := len(body) > maxProjectedExchangeBytes
	if truncated {
		body = body[:maxProjectedExchangeBytes]
		for len(body) > 0 && !utf8.Valid(body) {
			body = body[:len(body)-1]
		}
	}
	var value any
	if json.Unmarshal(body, &value) == nil {
		value = redactExchangeValue(value)
		redacted, _ := json.Marshal(value)
		if secret.LooksLikeCredential(redacted) {
			return map[string]any{"redacted": true}, "Body withheld because credential material remained after field redaction.", nil
		}
	} else {
		if secret.LooksLikeCredential(body) {
			return map[string]any{"redacted": true}, "Body withheld because it matched credential material.", nil
		}
		value = string(body)
	}
	if truncated {
		return map[string]any{"value": value, "truncated": true, "original_size_bytes": ref.SizeBytes}, "Body was truncated for display.", nil
	}
	return value, "", nil
}

func redactExchangeValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, child := range typed {
			normalized := strings.ToLower(strings.ReplaceAll(key, "-", "_"))
			if sensitiveExchangeKeys[normalized] {
				result[key] = "[REDACTED]"
				continue
			}
			result[key] = redactExchangeValue(child)
		}
		return result
	case []any:
		result := make([]any, len(typed))
		for index, child := range typed {
			result[index] = redactExchangeValue(child)
		}
		return result
	default:
		return value
	}
}

func modelExchange(turn persistedModelTurn) *CallExchange {
	toolCalls := make([]map[string]string, 0, len(turn.Assistant.ToolCalls))
	for _, call := range turn.Assistant.ToolCalls {
		toolCalls = append(toolCalls, map[string]string{"id": call.ID, "name": call.Name})
	}
	request := map[string]any{
		"message_count": turn.Request.MessageCount, "message_roles": turn.Request.MessageRoles,
		"tool_names": turn.Request.ToolNames, "max_output_tokens": turn.Request.MaxOutputTokens,
		"estimated_input_tokens": turn.Request.EstimatedInputTokens,
	}
	response := map[string]any{
		"finish_reason": turn.FinishReason, "content_bytes": len([]byte(turn.Assistant.Content)),
		"tool_calls": toolCalls, "provider": turn.Usage.Provider, "model": turn.Usage.Model,
		"input_tokens": turn.Usage.InputTokens, "output_tokens": turn.Usage.OutputTokens,
		"cache_hit_tokens": turn.Usage.CacheHitTokens, "cache_miss_tokens": turn.Usage.CacheMissTokens,
		"reasoning_tokens": turn.Usage.ReasoningTokens, "duration_ms": turn.Usage.DurationMillis,
	}
	return &CallExchange{
		Request: request, Response: response,
		Disclosure: "Safe Model exchange summary. Message bodies, the system prompt, tool arguments, candidate text, and private reasoning are intentionally not exposed.",
	}
}
