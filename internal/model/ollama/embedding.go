package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Embedder uses Ollama's local embedding endpoint. It deliberately remains
// separate from Client because Eri may use DeepSeek for chat while keeping
// private Memory embeddings on the user's device.
type Embedder struct {
	baseURL string
	model   string
	http    *http.Client
}

func NewEmbedder(baseURL, model string, timeout time.Duration) (*Embedder, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	model = strings.TrimSpace(model)
	if baseURL == "" || model == "" {
		return nil, fmt.Errorf("Ollama embedding URL and model are required")
	}
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	return &Embedder{baseURL: baseURL, model: model, http: &http.Client{Timeout: timeout}}, nil
}

func (e *Embedder) ID() string { return "ollama:" + e.model }

func (e *Embedder) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	if len(inputs) == 0 || len(inputs) > 128 {
		return nil, fmt.Errorf("Ollama embedding input count must be between 1 and 128")
	}
	for _, input := range inputs {
		if strings.TrimSpace(input) == "" || len([]byte(input)) > 64*1024 {
			return nil, fmt.Errorf("Ollama embedding input must be between 1 byte and 64 KiB")
		}
	}
	body, err := json.Marshal(map[string]any{"model": e.model, "input": inputs, "truncate": false})
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := e.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("Ollama embedding request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64*1024))
		return nil, fmt.Errorf("Ollama embedding request returned HTTP %d", response.StatusCode)
	}
	var result struct {
		Model      string      `json:"model"`
		Embeddings [][]float32 `json:"embeddings"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64*1024*1024)).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode Ollama embeddings: %w", err)
	}
	if len(result.Embeddings) != len(inputs) {
		return nil, fmt.Errorf("Ollama returned %d embeddings for %d inputs", len(result.Embeddings), len(inputs))
	}
	dimensions := 0
	for _, vector := range result.Embeddings {
		if dimensions == 0 {
			dimensions = len(vector)
		}
		if len(vector) == 0 || len(vector) != dimensions || len(vector) > 16384 {
			return nil, fmt.Errorf("Ollama returned inconsistent embedding dimensions")
		}
		for _, value := range vector {
			if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
				return nil, fmt.Errorf("Ollama returned a non-finite embedding value")
			}
		}
	}
	return result.Embeddings, nil
}

// DiscoverEmbeddingModel returns a stable local model choice. preferred is
// checked first; otherwise Eri ranks Ollama's documented embedding families
// ahead of any other installed model that advertises embedding capability.
func DiscoverEmbeddingModel(ctx context.Context, baseURL, preferred string) (string, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	client := &http.Client{Timeout: 3 * time.Second}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/tags", nil)
	if err != nil {
		return "", err
	}
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Ollama model inventory returned HTTP %d", response.StatusCode)
	}
	var inventory struct {
		Models []struct {
			Name  string `json:"name"`
			Model string `json:"model"`
		} `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 2*1024*1024)).Decode(&inventory); err != nil {
		return "", err
	}
	names := make([]string, 0, len(inventory.Models))
	for _, candidate := range inventory.Models {
		name := strings.TrimSpace(candidate.Name)
		if name == "" {
			name = strings.TrimSpace(candidate.Model)
		}
		if name != "" {
			names = append(names, name)
		}
	}
	preferred = strings.TrimSpace(preferred)
	sort.SliceStable(names, func(left, right int) bool {
		return embeddingPreference(names[left], preferred) < embeddingPreference(names[right], preferred)
	})
	for _, name := range names {
		capable, err := modelSupportsEmbedding(ctx, client, baseURL, name)
		if err != nil {
			continue
		}
		if capable {
			return name, nil
		}
	}
	return "", nil
}

func embeddingPreference(name, preferred string) int {
	if preferred != "" && name == preferred {
		return 0
	}
	lower := strings.ToLower(name)
	for index, family := range []string{"qwen3-embedding", "embeddinggemma", "all-minilm"} {
		if strings.Contains(lower, family) {
			return index + 1
		}
	}
	return 10
}

func modelSupportsEmbedding(ctx context.Context, client *http.Client, baseURL, model string) (bool, error) {
	body, _ := json.Marshal(map[string]string{"model": model})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/show", bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false, fmt.Errorf("Ollama model details returned HTTP %d", response.StatusCode)
	}
	var details struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 2*1024*1024)).Decode(&details); err != nil {
		return false, err
	}
	for _, capability := range details.Capabilities {
		if capability == "embedding" {
			return true, nil
		}
	}
	return false, nil
}
