// Package ollama adapts Ollama's native chat and tool-calling protocol to
// Eri's provider-neutral Agent Loop messages.
package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/z-chenhao/eri/internal/agent"
)

type Client struct {
	baseURL string
	model   string
	http    *http.Client
	capMu   sync.Mutex
	caps    *agent.ModelCapabilities
}

func New(baseURL, model string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"), model: model,
		http: &http.Client{Timeout: timeout},
	}
}

type functionCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type nativeToolCall struct {
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function functionCall `json:"function"`
}

type chatMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content,omitempty"`
	Images     []string         `json:"images,omitempty"`
	ToolCalls  []nativeToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	ToolName   string           `json:"tool_name,omitempty"`
}

type nativeTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

type chatRequest struct {
	Model    string         `json:"model"`
	Messages []chatMessage  `json:"messages"`
	Tools    []nativeTool   `json:"tools,omitempty"`
	Format   string         `json:"format,omitempty"`
	Stream   bool           `json:"stream"`
	Think    bool           `json:"think"`
	Options  map[string]any `json:"options,omitempty"`
}

type chatResponse struct {
	Message         chatMessage `json:"message"`
	DoneReason      string      `json:"done_reason"`
	PromptEvalCount int         `json:"prompt_eval_count"`
	EvalCount       int         `json:"eval_count"`
	TotalDuration   int64       `json:"total_duration"`
}

type showResponse struct {
	Capabilities []string       `json:"capabilities"`
	ModelInfo    map[string]any `json:"model_info"`
}

func (c *Client) Capabilities(ctx context.Context) (agent.ModelCapabilities, error) {
	c.capMu.Lock()
	defer c.capMu.Unlock()
	if c.caps != nil {
		return *c.caps, nil
	}
	body, err := json.Marshal(map[string]string{"model": c.model})
	if err != nil {
		return agent.ModelCapabilities{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/show", bytes.NewReader(body))
	if err != nil {
		return agent.ModelCapabilities{}, fmt.Errorf("create Ollama capability request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return agent.ModelCapabilities{}, fmt.Errorf("inspect Ollama model: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
		return agent.ModelCapabilities{}, fmt.Errorf("Ollama show returned HTTP %d", resp.StatusCode)
	}
	var show showResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2*1024*1024)).Decode(&show); err != nil {
		return agent.ModelCapabilities{}, fmt.Errorf("decode Ollama capabilities: %w", err)
	}
	result := agent.ModelCapabilities{
		Text: true, Usage: true, Cancellation: true, Streaming: true,
		DataResidency: "local", MaxOutputTokens: 32_768,
	}
	for _, capability := range show.Capabilities {
		switch capability {
		case "vision":
			result.Image = true
		case "tools":
			result.ToolCalling = true
		}
	}
	for key, value := range show.ModelInfo {
		if !strings.HasSuffix(key, ".context_length") {
			continue
		}
		switch number := value.(type) {
		case float64:
			result.ContextTokens = int(number)
		case int:
			result.ContextTokens = number
		}
	}
	if result.ContextTokens <= 0 {
		return agent.ModelCapabilities{}, fmt.Errorf("Ollama model %q did not report context length", c.model)
	}
	c.caps = &result
	return result, nil
}

func (c *Client) Complete(ctx context.Context, input agent.ModelRequest) (agent.ModelResponse, error) {
	messages := make([]chatMessage, 0, len(input.Messages)+1)
	messages = append(messages, chatMessage{Role: "system", Content: input.System})
	toolNames := make(map[string]string)
	for _, message := range input.Messages {
		native := chatMessage{Role: message.Role, Content: message.Content, ToolCallID: message.ToolCallID}
		for _, image := range message.Images {
			native.Images = append(native.Images, image.Data)
		}
		for _, call := range message.ToolCalls {
			native.ToolCalls = append(native.ToolCalls, nativeToolCall{
				ID: call.ID, Type: "function",
				Function: functionCall{Name: call.Name, Arguments: call.Arguments},
			})
			toolNames[call.ID] = call.Name
		}
		if message.Role == "tool" {
			native.ToolName = toolNames[message.ToolCallID]
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
	maxOutput := input.MaxOutputTokens
	if maxOutput <= 0 {
		maxOutput = 1024
	}
	requestBody := chatRequest{
		Model: c.model, Messages: messages, Tools: tools, Stream: false, Think: false,
		Options: map[string]any{"temperature": 0.2, "num_predict": maxOutput},
	}
	if input.JSONOutput {
		requestBody.Format = "json"
	}
	encoded, err := json.Marshal(requestBody)
	if err != nil {
		return agent.ModelResponse{}, fmt.Errorf("encode Ollama request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/chat", bytes.NewReader(encoded))
	if err != nil {
		return agent.ModelResponse{}, fmt.Errorf("create Ollama request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	started := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		return agent.ModelResponse{}, fmt.Errorf("call Ollama: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
		return agent.ModelResponse{}, fmt.Errorf("Ollama returned HTTP %d", resp.StatusCode)
	}
	var response chatResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2*1024*1024)).Decode(&response); err != nil {
		return agent.ModelResponse{}, fmt.Errorf("decode Ollama response: %w", err)
	}
	message := agent.Message{Role: "assistant", Content: response.Message.Content}
	for _, call := range response.Message.ToolCalls {
		message.ToolCalls = append(message.ToolCalls, agent.ToolCall{
			ID: call.ID, Name: call.Function.Name, Arguments: call.Function.Arguments,
		})
	}
	duration := time.Since(started).Milliseconds()
	if response.TotalDuration > 0 {
		duration = response.TotalDuration / int64(time.Millisecond)
	}
	return agent.ModelResponse{
		Message: message, FinishReason: response.DoneReason,
		Usage: agent.Usage{
			Provider: "ollama", Model: c.model, InputTokens: response.PromptEvalCount,
			OutputTokens: response.EvalCount, ModelCalls: 1, DurationMillis: duration,
		},
	}, nil
}
