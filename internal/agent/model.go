package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/z-chenhao/eri/internal/execution"
	"github.com/z-chenhao/eri/internal/tool"
)

// Message is Eri's provider-neutral representation of the native chat
// protocol. Assistant tool calls and their matching tool result IDs remain
// first-class messages; they are never encoded into a synthetic decision.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	Images     []Image    `json:"images,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type Image struct {
	MediaType string `json:"media_type"`
	Data      string `json:"data_base64"`
}

type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type ToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type ModelRequest struct {
	System          string           `json:"system"`
	Messages        []Message        `json:"messages"`
	Tools           []ToolDefinition `json:"tools,omitempty"`
	JSONOutput      bool             `json:"json_output,omitempty"`
	MaxOutputTokens int              `json:"max_output_tokens"`
}

type ModelResponse struct {
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
	Usage        Usage   `json:"usage"`
}

// ModelCapabilities is the agent-facing name for provider facts owned by the
// execution domain and shared with durable runtime projections.
type ModelCapabilities = execution.ModelCapabilities

type Usage struct {
	Provider        string `json:"provider"`
	Model           string `json:"model"`
	InputTokens     int    `json:"input_tokens"`
	OutputTokens    int    `json:"output_tokens"`
	CacheHitTokens  int    `json:"cache_hit_tokens"`
	CacheMissTokens int    `json:"cache_miss_tokens"`
	ReasoningTokens int    `json:"reasoning_tokens"`
	ModelCalls      int    `json:"model_calls"`
	DurationMillis  int64  `json:"duration_ms"`
}

// Completer is the narrow provider boundary for consumers that only make a
// model request, such as Eval and evolution proposal generation.
type Completer interface {
	Complete(context.Context, ModelRequest) (ModelResponse, error)
}

// Model is the cognitive boundary used by the Agent Loop. A provider must
// report its real capabilities before Eri assembles or sends context; guessing
// a context window or modality would make the safety boundary provider-
// dependent and can leak data through a failed probe request.
type Model interface {
	Completer
	Capabilities(context.Context) (ModelCapabilities, error)
}

func capabilitiesFor(ctx context.Context, model Model) (ModelCapabilities, error) {
	capabilities, err := model.Capabilities(ctx)
	if err != nil {
		return ModelCapabilities{}, err
	}
	if capabilities.ContextTokens <= 0 {
		return ModelCapabilities{}, fmt.Errorf("model provider returned no context window")
	}
	return capabilities, nil
}

func buildToolDefinitions(descriptors []tool.Descriptor) ([]ToolDefinition, map[string]string, error) {
	sorted := append([]tool.Descriptor(nil), descriptors...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	definitions := make([]ToolDefinition, 0, len(sorted))
	toolIDs := make(map[string]string, len(sorted))
	for _, descriptor := range sorted {
		name := modelToolName(descriptor.ID)
		if previous, exists := toolIDs[name]; exists {
			return nil, nil, fmt.Errorf("tool IDs %q and %q map to the same model name %q", previous, descriptor.ID, name)
		}
		parameters := descriptor.InputSchema
		if parameters == nil {
			parameters = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		definitions = append(definitions, ToolDefinition{
			Name: name, Description: descriptor.Purpose, Parameters: parameters,
		})
		toolIDs[name] = descriptor.ID
	}
	return definitions, toolIDs, nil
}

func modelToolName(id string) string {
	var b strings.Builder
	for _, r := range id {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	name := strings.Trim(b.String(), "_-")
	if name == "" {
		name = "tool"
	}
	if len(name) <= 64 {
		return name
	}
	digest := sha256.Sum256([]byte(id))
	return name[:51] + "_" + hex.EncodeToString(digest[:6])
}

func validateAssistantMessage(message Message) error {
	if message.Role != "" && message.Role != "assistant" {
		return fmt.Errorf("model returned role %q, want assistant", message.Role)
	}
	seen := make(map[string]struct{}, len(message.ToolCalls))
	for _, call := range message.ToolCalls {
		if strings.TrimSpace(call.ID) == "" || strings.TrimSpace(call.Name) == "" {
			return fmt.Errorf("model tool call requires id and name")
		}
		if _, duplicate := seen[call.ID]; duplicate {
			return fmt.Errorf("model returned duplicate tool call ID %q", call.ID)
		}
		seen[call.ID] = struct{}{}
		if len(call.Arguments) == 0 || !json.Valid(call.Arguments) {
			return fmt.Errorf("model tool call %q has invalid JSON arguments", call.ID)
		}
	}
	if len(message.ToolCalls) == 0 && strings.TrimSpace(message.Content) == "" {
		return fmt.Errorf("model returned neither text nor tool calls")
	}
	return nil
}

// validateModelTranscript enforces the provider-neutral tool protocol before a
// request leaves the Agent Loop. An assistant tool-call message opens one
// protocol frame; every declared call must receive exactly one matching tool
// message before any user, system, or assistant message can follow.
func validateModelTranscript(messages []Message) error {
	pending := map[string]struct{}{}
	frameOpen := false
	for index, message := range messages {
		switch message.Role {
		case "tool":
			if !frameOpen {
				return fmt.Errorf("message %d is an orphan tool result", index)
			}
			if strings.TrimSpace(message.ToolCallID) == "" {
				return fmt.Errorf("message %d tool result has no tool_call_id", index)
			}
			if _, exists := pending[message.ToolCallID]; !exists {
				return fmt.Errorf("message %d tool result %q is duplicate or undeclared", index, message.ToolCallID)
			}
			delete(pending, message.ToolCallID)
			if len(pending) == 0 {
				frameOpen = false
			}
		case "assistant":
			if frameOpen {
				return fmt.Errorf("message %d interrupts an open tool-call frame", index)
			}
			if len(message.ToolCalls) == 0 {
				continue
			}
			if err := validateAssistantMessage(message); err != nil {
				return fmt.Errorf("message %d: %w", index, err)
			}
			pending = make(map[string]struct{}, len(message.ToolCalls))
			for _, call := range message.ToolCalls {
				pending[call.ID] = struct{}{}
			}
			frameOpen = true
		case "user", "system":
			if frameOpen {
				return fmt.Errorf("message %d interrupts an open tool-call frame", index)
			}
		default:
			return fmt.Errorf("message %d has unsupported role %q", index, message.Role)
		}
	}
	if frameOpen {
		ids := make([]string, 0, len(pending))
		for id := range pending {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		return fmt.Errorf("tool-call frame is missing results for %s", strings.Join(ids, ", "))
	}
	return nil
}

func mergeUsage(total, next Usage) Usage {
	if total.Provider == "" {
		total.Provider = next.Provider
		total.Model = next.Model
	}
	total.InputTokens += next.InputTokens
	total.OutputTokens += next.OutputTokens
	total.CacheHitTokens += next.CacheHitTokens
	total.CacheMissTokens += next.CacheMissTokens
	total.ReasoningTokens += next.ReasoningTokens
	total.ModelCalls += next.ModelCalls
	total.DurationMillis += next.DurationMillis
	return total
}

// recordModelCall normalizes one completed provider/Judge invocation before it
// is merged into an aggregate. Keeping this separate from mergeUsage matters:
// optional work such as context compaction legitimately returns an empty Usage
// value when no model was called, and aggregating that value must not invent a
// call. Providers that do not report request counts still account for the real
// invocation at this boundary.
func recordModelCall(usage Usage) Usage {
	if usage.ModelCalls == 0 {
		usage.ModelCalls = 1
	}
	return usage
}
