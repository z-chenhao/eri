// Package deepseek implements DeepSeek's OpenAI-compatible native chat and
// tool-calling protocol. Credentials are accepted only at runtime.
package deepseek

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/z-chenhao/eri/internal/agent"
)

type Client struct {
	baseURL string
	apiKey  string
	model   string
	http    *http.Client
}

func New(baseURL, apiKey, model string, timeout time.Duration) (*Client, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("DEEPSEEK_API_KEY is required when ERI_MODEL_PROVIDER=deepseek")
	}
	return newClient(baseURL, apiKey, model, timeout, nil)
}

// NewViaBroker keeps the DeepSeek credential outside Eri Core. The custom
// transport reaches the isolated Provider Secret Broker over a private socket;
// the Broker attaches the Authorization header immediately before egress.
func NewViaBroker(socketPath, model string, timeout time.Duration) (*Client, error) {
	if !filepath.IsAbs(socketPath) || filepath.Clean(socketPath) == string(filepath.Separator) {
		return nil, fmt.Errorf("provider broker socket must be an absolute non-root path")
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "unix", socketPath)
	}}
	return newClient("http://127.0.0.1", "", model, timeout, transport)
}

func newClient(baseURL, apiKey, model string, timeout time.Duration, transport http.RoundTripper) (*Client, error) {
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("DeepSeek base URL must be absolute and contain no credentials, query or fragment")
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopbackHost(parsed.Hostname())) {
		return nil, fmt.Errorf("DeepSeek base URL must use HTTPS except on numeric loopback")
	}
	configuredOrigin := parsed.Scheme + "://" + parsed.Host
	client := &http.Client{Timeout: timeout, Transport: transport}
	client.CheckRedirect = func(request *http.Request, _ []*http.Request) error {
		if request.URL.Scheme+"://"+request.URL.Host != configuredOrigin {
			return fmt.Errorf("DeepSeek redirect left the configured origin")
		}
		return nil
	}
	return &Client{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"), apiKey: apiKey, model: model,
		http: client,
	}, nil
}

func isLoopbackHost(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (c *Client) Capabilities(context.Context) (agent.ModelCapabilities, error) {
	return agent.ModelCapabilities{
		Text: true, StructuredOutput: true, ToolCalling: true,
		Usage: true, Cancellation: true, ContextTokens: 1_000_000,
		MaxOutputTokens: 384_000, DataResidency: "deepseek_cloud",
	}, nil
}

type functionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type nativeToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function functionCall `json:"function"`
}

type chatMessage struct {
	Role             string           `json:"role"`
	Content          *string          `json:"content"`
	ReasoningContent string           `json:"reasoning_content,omitempty"`
	ToolCalls        []nativeToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string           `json:"tool_call_id,omitempty"`
}

type nativeTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description,omitempty"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

type chatRequest struct {
	Model           string          `json:"model"`
	Messages        []chatMessage   `json:"messages"`
	Tools           []nativeTool    `json:"tools,omitempty"`
	ToolChoice      string          `json:"tool_choice,omitempty"`
	Stream          bool            `json:"stream"`
	Thinking        map[string]any  `json:"thinking"`
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
	Temperature     *float64        `json:"temperature,omitempty"`
	MaxTokens       *int            `json:"max_tokens,omitempty"`
	ResponseFormat  *responseFormat `json:"response_format,omitempty"`
}

type responseFormat struct {
	Type string `json:"type"`
}

type chatResponse struct {
	Choices []struct {
		FinishReason string      `json:"finish_reason"`
		Message      chatMessage `json:"message"`
	} `json:"choices"`
	Usage struct {
		CompletionTokens       int `json:"completion_tokens"`
		PromptTokens           int `json:"prompt_tokens"`
		PromptCacheHitTokens   int `json:"prompt_cache_hit_tokens"`
		PromptCacheMissTokens  int `json:"prompt_cache_miss_tokens"`
		CompletionTokenDetails struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
}

func (c *Client) Complete(ctx context.Context, input agent.ModelRequest) (agent.ModelResponse, error) {
	messages := make([]chatMessage, 0, len(input.Messages)+1)
	system := input.System
	messages = append(messages, chatMessage{Role: "system", Content: &system})
	for index, message := range input.Messages {
		if len(message.Images) > 0 {
			return agent.ModelResponse{}, fmt.Errorf("DeepSeek text model does not accept image input")
		}
		if len(message.ToolCalls) > 0 && strings.TrimSpace(message.ReasoningContent) == "" {
			return agent.ModelResponse{}, fmt.Errorf("DeepSeek thinking transcript message %d has Tool Calls without reasoning_content", index)
		}
		content := message.Content
		// DeepSeek's thinking Tool examples replay assistant Tool Calls with
		// content="". Keep that non-null protocol value while replaying the
		// provider-native reasoning_content and Tool Calls unchanged.
		native := chatMessage{
			Role: message.Role, Content: &content, ReasoningContent: message.ReasoningContent,
			ToolCallID: message.ToolCallID,
		}
		if message.Role == "assistant" && content == "" && len(message.ToolCalls) == 0 {
			native.Content = nil
		}
		for _, call := range message.ToolCalls {
			native.ToolCalls = append(native.ToolCalls, nativeToolCall{
				ID: call.ID, Type: "function",
				Function: functionCall{Name: call.Name, Arguments: string(call.Arguments)},
			})
		}
		messages = append(messages, native)
	}
	tools := make([]nativeTool, 0, len(input.Tools))
	for _, definition := range input.Tools {
		var candidate nativeTool
		candidate.Type = "function"
		candidate.Function.Name = definition.Name
		candidate.Function.Description = definition.Description
		candidate.Function.Parameters = definition.Parameters
		tools = append(tools, candidate)
	}
	// The Agent Loop does not impose a model-output token ceiling.
	// MaxOutputTokens normally reserves context room only; a native structured
	// protocol such as Judge explicitly bounds its small JSON result below.
	requestBody := chatRequest{Model: c.model, Messages: messages, Tools: tools, Stream: false}
	if input.JSONOutput {
		requestBody.ResponseFormat = &responseFormat{Type: "json_object"}
		if input.MaxOutputTokens > 0 {
			requestBody.MaxTokens = &input.MaxOutputTokens
		}
	}
	// Thinking is part of DeepSeek's native Tool protocol. Eri stores each
	// assistant reasoning_content beside its Tool Calls in the durable model
	// transcript and sends it back on every later request that contains that
	// assistant message, including after checkpoint recovery.
	requestBody.Thinking = map[string]any{"type": "enabled"}
	// DeepSeek accepts the OpenAI-compatible medium spelling but currently
	// maps it to its lowest supported thinking tier, high. Keep the requested
	// balanced wire value explicit rather than allowing Agent-like requests to
	// be promoted automatically to max.
	requestBody.ReasoningEffort = "medium"
	encoded, err := json.Marshal(requestBody)
	if err != nil {
		return agent.ModelResponse{}, fmt.Errorf("encode DeepSeek request: %w", err)
	}
	debugRequestBody(os.Stderr, encoded)
	started := time.Now()
	response, err := c.completeWithRecovery(ctx, encoded)
	if err != nil {
		return agent.ModelResponse{}, err
	}
	if len(response.Choices) != 1 {
		return agent.ModelResponse{}, fmt.Errorf("DeepSeek returned %d choices, want 1", len(response.Choices))
	}
	choice := response.Choices[0]
	message := agent.Message{Role: "assistant", ReasoningContent: choice.Message.ReasoningContent}
	if choice.Message.Content != nil {
		message.Content = *choice.Message.Content
	}
	for _, call := range choice.Message.ToolCalls {
		arguments := json.RawMessage(call.Function.Arguments)
		message.ToolCalls = append(message.ToolCalls, agent.ToolCall{
			ID: call.ID, Name: call.Function.Name, Arguments: arguments,
		})
	}
	if len(message.ToolCalls) > 0 && strings.TrimSpace(message.ReasoningContent) == "" {
		return agent.ModelResponse{}, fmt.Errorf("DeepSeek thinking Tool Call omitted reasoning_content")
	}
	return agent.ModelResponse{
		Message: message, FinishReason: choice.FinishReason,
		Usage: agent.Usage{
			Provider: "deepseek", Model: c.model,
			InputTokens: response.Usage.PromptTokens, OutputTokens: response.Usage.CompletionTokens,
			CacheHitTokens:  response.Usage.PromptCacheHitTokens,
			CacheMissTokens: response.Usage.PromptCacheMissTokens,
			ReasoningTokens: response.Usage.CompletionTokenDetails.ReasoningTokens,
			ModelCalls:      1, DurationMillis: time.Since(started).Milliseconds(),
		},
	}, nil
}

func debugRequestBody(writer io.Writer, encoded []byte) {
	if os.Getenv("ERI_DEBUG_DEEPSEEK_REQUEST_BODY") != "1" {
		return
	}
	// Developer-only explicit opt-in: encoded may contain the complete private
	// conversation, Memory, Tool arguments, and provider continuation state.
	fmt.Fprintf(writer, "DeepSeek requestBody: %s\n", encoded)
}

const deepSeekAttempts = 3

func (c *Client) completeWithRecovery(ctx context.Context, encoded []byte) (chatResponse, error) {
	for attempt := 1; attempt <= deepSeekAttempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(encoded))
		if err != nil {
			return chatResponse{}, fmt.Errorf("create DeepSeek request: %w", err)
		}
		if c.apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+c.apiKey)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return chatResponse{}, ctx.Err()
			}
			if attempt == deepSeekAttempts {
				return chatResponse{}, fmt.Errorf("call DeepSeek after %d attempts: %w", attempt, err)
			}
			if err := waitForRetry(ctx, attempt); err != nil {
				return chatResponse{}, err
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
			resp.Body.Close()
			if retryableStatus(resp.StatusCode) && attempt < deepSeekAttempts {
				if err := waitForRetry(ctx, attempt); err != nil {
					return chatResponse{}, err
				}
				continue
			}
			return chatResponse{}, fmt.Errorf("DeepSeek returned HTTP %d", resp.StatusCode)
		}
		var response chatResponse
		err = json.NewDecoder(io.LimitReader(resp.Body, 2*1024*1024)).Decode(&response)
		resp.Body.Close()
		if err != nil {
			return chatResponse{}, fmt.Errorf("decode DeepSeek response: %w", err)
		}
		return response, nil
	}
	return chatResponse{}, fmt.Errorf("DeepSeek recovery ended without a response")
}

func retryableStatus(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
}

func waitForRetry(ctx context.Context, attempt int) error {
	delay := time.Duration(1<<uint(attempt-1)) * 250 * time.Millisecond
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
