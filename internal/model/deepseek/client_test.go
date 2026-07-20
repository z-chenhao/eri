package deepseek

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/z-chenhao/eri/internal/agent"
)

func TestClientUsesNativeToolsAndParsesCacheUsage(t *testing.T) {
	t.Parallel()
	var observed chatRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-secret" {
			t.Fatal("authorization header missing")
		}
		if err := json.NewDecoder(r.Body).Decode(&observed); err != nil {
			t.Fatal(err)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"finish_reason": "tool_calls",
				"message": map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
					map[string]any{"id": "call-1", "type": "function", "function": map[string]any{"name": "builtin_files", "arguments": `{"operation":"read","path":"brief.txt"}`}},
				}},
			}},
			"usage": map[string]any{"prompt_tokens": 120, "completion_tokens": 20, "prompt_cache_hit_tokens": 100, "prompt_cache_miss_tokens": 20},
		})
	}))
	defer server.Close()
	client, err := New(server.URL, "test-secret", "deepseek-v4-flash", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Complete(context.Background(), agent.ModelRequest{
		System: "stable soul", Messages: []agent.Message{{Role: "user", Content: "read it"}},
		Tools:           []agent.ToolDefinition{{Name: "builtin_files", Description: "files", Parameters: map[string]any{"type": "object"}}},
		MaxOutputTokens: 256,
	})
	if err != nil {
		t.Fatal(err)
	}
	if observed.Thinking["type"] != "disabled" || observed.ToolChoice != "auto" || observed.MaxTokens != nil || observed.ReasoningEffort != "" {
		t.Fatalf("request = %+v", observed)
	}
	if len(response.Message.ToolCalls) != 1 || !json.Valid(response.Message.ToolCalls[0].Arguments) {
		t.Fatalf("tool response = %+v", response.Message)
	}
	if response.Usage.CacheHitTokens != 100 || response.Usage.CacheMissTokens != 20 {
		t.Fatalf("cache usage = %+v", response.Usage)
	}
}

func TestClientUsesThinkingForToolFreeJudgment(t *testing.T) {
	t.Parallel()
	var observed chatRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&observed); err != nil {
			t.Fatal(err)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": "ready"}}},
			"usage":   map[string]any{"prompt_tokens": 12, "completion_tokens": 3},
		})
	}))
	defer server.Close()
	client, _ := New(server.URL, "test-secret", "deepseek-v4-flash", time.Second)
	if _, err := client.Complete(context.Background(), agent.ModelRequest{System: "judge carefully", JSONOutput: true, MaxOutputTokens: 128}); err != nil {
		t.Fatal(err)
	}
	if observed.Thinking["type"] != "enabled" || observed.ReasoningEffort != "high" || observed.ToolChoice != "" || observed.Temperature != nil || observed.MaxTokens != nil || observed.ResponseFormat == nil || observed.ResponseFormat.Type != "json_object" {
		t.Fatalf("tool-free request = %+v", observed)
	}
}

func TestClientDisablesThinkingForToolHistoryWithoutCurrentTools(t *testing.T) {
	t.Parallel()
	var observed chatRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&observed); err != nil {
			t.Fatal(err)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": `{"result":"pass"}`}}},
			"usage":   map[string]any{"prompt_tokens": 24, "completion_tokens": 4},
		})
	}))
	defer server.Close()
	client, _ := New(server.URL, "test-secret", "deepseek-v4-flash", time.Second)
	_, err := client.Complete(context.Background(), agent.ModelRequest{
		System: "return JSON",
		Messages: []agent.Message{
			{Role: "user", Content: "look it up"},
			{Role: "assistant", ToolCalls: []agent.ToolCall{{ID: "call-1", Name: "lookup", Arguments: json.RawMessage(`{}`)}}},
			{Role: "tool", ToolCallID: "call-1", Content: `{"found":true}`},
			{Role: "assistant", Content: "I found a reliable source and am checking one more detail."},
			{Role: "user", Content: "evaluate the progress candidate"},
		},
		JSONOutput: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if observed.Thinking["type"] != "disabled" || observed.ReasoningEffort != "" || observed.ToolChoice != "" || observed.Temperature != nil {
		t.Fatalf("historical tool request = %+v", observed)
	}
}

func TestClientRecoversTransientProviderFailure(t *testing.T) {
	t.Parallel()
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		if attempts < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": "recovered"}}},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 2},
		})
	}))
	defer server.Close()
	client, _ := New(server.URL, "test-secret", "deepseek-v4-flash", 3*time.Second)
	response, err := client.Complete(context.Background(), agent.ModelRequest{System: "stable"})
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 3 || response.Message.Content != "recovered" {
		t.Fatalf("attempts=%d response=%+v", attempts, response)
	}
}

func TestClientRequestPrefixRemainsByteStableAcrossToolTurn(t *testing.T) {
	t.Parallel()
	requests := make([]chatRequest, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request chatRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		requests = append(requests, request)
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"finish_reason": "stop",
				"message":       map[string]any{"role": "assistant", "content": "done"},
			}},
			"usage": map[string]any{"prompt_tokens": 30, "completion_tokens": 2, "prompt_cache_hit_tokens": 20, "prompt_cache_miss_tokens": 10},
		})
	}))
	defer server.Close()
	client, _ := New(server.URL, "test-secret", "deepseek-v4-flash", time.Second)
	base := agent.ModelRequest{
		System: "exact stable prefix", Messages: []agent.Message{{Role: "user", Content: "question"}},
		Tools: []agent.ToolDefinition{{Name: "lookup", Description: "lookup", Parameters: map[string]any{"type": "object"}}},
	}
	if _, err := client.Complete(context.Background(), base); err != nil {
		t.Fatal(err)
	}
	base.Messages = append(base.Messages,
		agent.Message{Role: "assistant", ToolCalls: []agent.ToolCall{{ID: "call", Name: "lookup", Arguments: json.RawMessage(`{}`)}}},
		agent.Message{Role: "tool", ToolCallID: "call", Content: `{"ok":true}`},
	)
	if _, err := client.Complete(context.Background(), base); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 {
		t.Fatalf("requests = %d", len(requests))
	}
	firstPrefix, _ := json.Marshal(struct {
		Messages []chatMessage `json:"messages"`
		Tools    []nativeTool  `json:"tools"`
	}{requests[0].Messages, requests[0].Tools})
	secondPrefix, _ := json.Marshal(struct {
		Messages []chatMessage `json:"messages"`
		Tools    []nativeTool  `json:"tools"`
	}{requests[1].Messages[:len(requests[0].Messages)], requests[1].Tools})
	if string(firstPrefix) != string(secondPrefix) {
		t.Fatalf("cache prefix changed:\n%s\n%s", firstPrefix, secondPrefix)
	}
}

func TestClientNeverExposesCredentialOrProviderBody(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte("provider secret detail"))
	}))
	defer server.Close()
	client, _ := New(server.URL, "credential-that-must-not-leak", "deepseek-v4-flash", time.Second)
	_, err := client.Complete(context.Background(), agent.ModelRequest{System: "stable"})
	if err == nil || strings.Contains(err.Error(), "credential-that-must-not-leak") || strings.Contains(err.Error(), "provider secret detail") {
		t.Fatalf("unsafe error: %v", err)
	}
	if err.Error() != "DeepSeek returned HTTP 401" {
		t.Fatalf("error = %v", err)
	}
}

func TestNewRequiresEnvironmentCredentialValue(t *testing.T) {
	t.Parallel()
	if _, err := New("https://api.deepseek.com", "", "deepseek-v4-flash", time.Second); err == nil {
		t.Fatal("missing credential unexpectedly accepted")
	}
}

func TestNewRejectsInsecureOrCredentialBearingProviderURLs(t *testing.T) {
	for _, baseURL := range []string{
		"http://api.deepseek.com",
		"https://user:secret@api.deepseek.com",
		"https://api.deepseek.com?token=secret",
	} {
		if _, err := New(baseURL, "runtime-secret", "deepseek-v4-flash", time.Second); err == nil {
			t.Fatalf("unsafe base URL %q accepted", baseURL)
		}
	}
}

func TestClientDoesNotForwardCredentialAcrossRedirectOrigins(t *testing.T) {
	var receivedAuthorization string
	target := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		receivedAuthorization = r.Header.Get("Authorization")
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	client, err := New(source.URL, "runtime-secret", "deepseek-v4-flash", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Complete(context.Background(), agent.ModelRequest{System: "stable"}); err == nil || !strings.Contains(err.Error(), "configured origin") {
		t.Fatalf("cross-origin redirect was not rejected: %v", err)
	}
	if receivedAuthorization != "" {
		t.Fatal("provider credential reached redirect target")
	}
}

func TestLivePromptCacheProbe(t *testing.T) {
	if os.Getenv("ERI_DEEPSEEK_LIVE_TEST") != "1" {
		t.Skip("set ERI_DEEPSEEK_LIVE_TEST=1 for the bounded two-request cache probe")
	}
	key := strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY"))
	if key == "" {
		t.Fatal("DEEPSEEK_API_KEY is required for the live cache probe")
	}
	model := strings.TrimSpace(os.Getenv("ERI_MODEL"))
	if model == "" {
		model = "deepseek-v4-flash"
	}
	client, err := New("https://api.deepseek.com", key, model, 90*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	stable := strings.Repeat("Eri controlled prompt-cache probe. This is inert repeated context used only to verify byte-stable prefix reuse. ", 48)
	firstRequest := agent.ModelRequest{
		System:          stable,
		Messages:        []agent.Message{{Role: "user", Content: "Reply with exactly OK."}},
		MaxOutputTokens: 8,
	}
	first, err := client.Complete(context.Background(), firstRequest)
	if err != nil {
		t.Fatal(err)
	}
	secondRequest := firstRequest
	secondRequest.Messages = append(append([]agent.Message(nil), firstRequest.Messages...),
		first.Message,
		agent.Message{Role: "user", Content: "Reply with exactly OK again."},
	)
	second, err := client.Complete(context.Background(), secondRequest)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("first usage: input=%d output=%d cache_hit=%d cache_miss=%d", first.Usage.InputTokens, first.Usage.OutputTokens, first.Usage.CacheHitTokens, first.Usage.CacheMissTokens)
	t.Logf("second usage: input=%d output=%d cache_hit=%d cache_miss=%d", second.Usage.InputTokens, second.Usage.OutputTokens, second.Usage.CacheHitTokens, second.Usage.CacheMissTokens)
	if second.Usage.CacheHitTokens <= 0 {
		t.Fatal("second request reported no cache hit; do not claim live cache verification")
	}
}
