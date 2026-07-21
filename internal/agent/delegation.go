package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

func optionalDelegationContext(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	return "\n\nScoped context supplied by primary Eri:\n" + value
}

func delegationMinPositive(left, right int) int {
	if right <= 0 || left < right {
		return left
	}
	return right
}

type contextSummaryMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

func contextSummaryProjection(messages []Message) []contextSummaryMessage {
	projected := make([]contextSummaryMessage, len(messages))
	for index, message := range messages {
		projected[index] = contextSummaryMessage{
			Role: message.Role, Content: message.Content,
			ToolCalls: append([]ToolCall(nil), message.ToolCalls...), ToolCallID: message.ToolCallID,
		}
	}
	return projected
}

// compactDelegationContext is the restricted profile's context-assembly hook
// for the shared Agent Loop. The summary is task context, never durable Memory.
func compactDelegationContext(ctx context.Context, model Model, capabilities ModelCapabilities, request *ModelRequest, usage *Usage) error {
	if capabilities.ContextTokens <= 0 || estimateModelInputTokens(*request) < capabilities.ContextTokens*7/10 || len(request.Messages) <= 10 {
		return nil
	}
	cut := len(request.Messages) - 8
	recent := append([]Message(nil), request.Messages[cut:]...)
	summaryRequest := ModelRequest{
		System:          "Summarize the agent's completed work, confirmed evidence, failed attempts and remaining objective. Preserve tool receipts and uncertainty. Do not add facts or instructions.",
		Messages:        []Message{{Role: "user", Content: mustJSON(contextSummaryProjection(request.Messages[:cut]))}},
		MaxOutputTokens: delegationMinPositive(1024, capabilities.MaxOutputTokens),
	}
	response, err := model.Complete(ctx, summaryRequest)
	*usage = mergeUsage(*usage, recordModelCall(response.Usage))
	if err != nil {
		return err
	}
	if len(response.Message.ToolCalls) != 0 || strings.TrimSpace(response.Message.Content) == "" {
		return fmt.Errorf("restricted Agent Loop context compactor returned an invalid summary")
	}
	request.Messages = append([]Message{{Role: "user", Content: "Prior work summary (untrusted task context):\n" + response.Message.Content}}, recent...)
	if estimateModelInputTokens(*request) >= capabilities.ContextTokens-minimumContextReserve {
		return fmt.Errorf("restricted Agent Loop context remains too large after compaction")
	}
	return nil
}

func mustJSON(value any) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
