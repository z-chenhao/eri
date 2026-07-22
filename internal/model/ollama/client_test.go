package ollama

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/z-chenhao/eri/internal/agent"
)

func TestClientUsesNativeToolCalling(t *testing.T) {
	t.Parallel()
	var observed chatRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&observed); err != nil {
			t.Fatal(err)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"message": map[string]any{"role": "assistant", "tool_calls": []any{
				map[string]any{"id": "call-1", "type": "function", "function": map[string]any{"name": "files", "arguments": map[string]any{"operation": "read", "path": "brief.txt"}}},
			}},
			"done_reason": "tool_calls", "prompt_eval_count": 42, "eval_count": 12,
		})
	}))
	defer server.Close()

	client := New(server.URL, "test-model", time.Second)
	response, err := client.Complete(context.Background(), agent.ModelRequest{
		System: "stable soul", Messages: []agent.Message{{Role: "user", Content: "read it"}},
		Tools:           []agent.ToolDefinition{{Name: "files", Description: "files", Parameters: map[string]any{"type": "object"}}},
		MaxOutputTokens: 333,
	})
	if err != nil {
		t.Fatal(err)
	}
	if observed.Stream || observed.Think || len(observed.Tools) != 1 {
		t.Fatalf("request did not use native non-streaming tools: %+v", observed)
	}
	if _, exists := observed.Options["format"]; exists {
		t.Fatal("synthetic structured decision format must not be sent")
	}
	if observed.Options["num_predict"] != float64(333) {
		t.Fatalf("output budget = %#v", observed.Options["num_predict"])
	}
	if len(response.Message.ToolCalls) != 1 || response.Message.ToolCalls[0].Name != "files" {
		t.Fatalf("response = %+v", response)
	}
	if response.Usage.InputTokens != 42 || response.Usage.OutputTokens != 12 {
		t.Fatalf("usage = %+v", response.Usage)
	}
}

func TestClientPreservesAssistantCallAndMatchingToolResult(t *testing.T) {
	t.Parallel()
	var observed chatRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&observed)
		json.NewEncoder(w).Encode(map[string]any{"message": map[string]any{"role": "assistant", "content": "done"}, "done_reason": "stop"})
	}))
	defer server.Close()
	client := New(server.URL, "test", time.Second)
	_, err := client.Complete(context.Background(), agent.ModelRequest{
		System: "stable", Messages: []agent.Message{
			{Role: "user", Content: "read"},
			{Role: "assistant", ToolCalls: []agent.ToolCall{{ID: "call-1", Name: "files", Arguments: json.RawMessage(`{"operation":"read","path":"brief.txt"}`)}}},
			{Role: "tool", ToolCallID: "call-1", Content: `{"success":true}`},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	toolResult := observed.Messages[len(observed.Messages)-1]
	if toolResult.Role != "tool" || toolResult.ToolCallID != "call-1" || toolResult.ToolName != "files" {
		t.Fatalf("tool result = %+v", toolResult)
	}
}

func TestClientRequestsNativeJSONOutputForStructuredEvaluation(t *testing.T) {
	var observed chatRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&observed)
		json.NewEncoder(w).Encode(map[string]any{"message": map[string]any{"role": "assistant", "content": `{"result":"pass"}`}, "done_reason": "stop"})
	}))
	defer server.Close()
	client := New(server.URL, "judge", time.Second)
	if _, err := client.Complete(context.Background(), agent.ModelRequest{System: "return JSON", JSONOutput: true}); err != nil {
		t.Fatal(err)
	}
	if observed.Format != "json" {
		t.Fatalf("format = %q, want json", observed.Format)
	}
}

func TestClientForwardsVisionImagesInNativeOllamaMessage(t *testing.T) {
	var observed chatRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&observed)
		json.NewEncoder(w).Encode(map[string]any{"message": map[string]any{"role": "assistant", "content": "image inspected"}, "done_reason": "stop"})
	}))
	defer server.Close()
	client := New(server.URL, "vision", time.Second)
	_, err := client.Complete(context.Background(), agent.ModelRequest{Messages: []agent.Message{{
		Role: "user", Content: "inspect", Images: []agent.Image{{MediaType: "image/png", Data: "aW1hZ2U="}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(observed.Messages) < 2 || len(observed.Messages[1].Images) != 1 || observed.Messages[1].Images[0] != "aW1hZ2U=" {
		t.Fatalf("messages=%+v", observed.Messages)
	}
}

func TestClientDoesNotExposeProviderErrorBody(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("secret provider detail"))
	}))
	defer server.Close()
	client := New(server.URL, "test-model", time.Second)
	_, err := client.Complete(context.Background(), agent.ModelRequest{System: "soul"})
	if err == nil || err.Error() != "Ollama returned HTTP 500" {
		t.Fatalf("error = %v", err)
	}
}
