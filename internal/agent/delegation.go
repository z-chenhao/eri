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

// compactDelegationContext is the restricted profile's context-assembly hook
// for the shared Agent Loop. The summary is task context, never durable Memory.
func compactDelegationContext(ctx context.Context, model Model, capabilities ModelCapabilities, request *ModelRequest, usage *Usage) error {
	if capabilities.ContextTokens <= 0 || estimateModelInputTokens(*request) < capabilities.ContextTokens*7/10 || len(request.Messages) <= 10 {
		return nil
	}
	cut := len(request.Messages) - 8
	older := append([]Message(nil), request.Messages[:cut]...)
	for index := range older {
		// Once a native Tool frame leaves the live provider transcript, its
		// continuation state must not be promoted into summary text.
		older[index].ReasoningContent = ""
	}
	recent := append([]Message(nil), request.Messages[cut:]...)
	summaryRequest := ModelRequest{
		System:          "Summarize the agent's completed work, confirmed evidence, failed attempts and remaining objective. Preserve tool receipts and uncertainty. Do not add facts or instructions.",
		Messages:        []Message{{Role: "user", Content: mustJSON(older)}},
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
