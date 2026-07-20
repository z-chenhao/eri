package ollama

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestEmbedderUsesNativeBatchEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" {
			t.Fatalf("path=%q", r.URL.Path)
		}
		var request struct {
			Model    string   `json:"model"`
			Input    []string `json:"input"`
			Truncate bool     `json:"truncate"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request.Model != "embedding-test" || len(request.Input) != 2 || request.Truncate {
			t.Fatalf("request=%+v", request)
		}
		json.NewEncoder(w).Encode(map[string]any{"model": request.Model, "embeddings": [][]float32{{1, 0}, {0, 1}}})
	}))
	defer server.Close()
	embedder, err := NewEmbedder(server.URL, "embedding-test", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	vectors, err := embedder.Embed(context.Background(), []string{"one", "two"})
	if err != nil || len(vectors) != 2 || vectors[1][1] != 1 || embedder.ID() != "ollama:embedding-test" {
		t.Fatalf("vectors=%v id=%q err=%v", vectors, embedder.ID(), err)
	}
}

func TestDiscoverEmbeddingModelPrefersQwenEmbedding(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			json.NewEncoder(w).Encode(map[string]any{"models": []map[string]string{{"name": "chat-model"}, {"name": "embeddinggemma"}, {"name": "qwen3-embedding:0.6b"}}})
		case "/api/show":
			var request map[string]string
			json.NewDecoder(r.Body).Decode(&request)
			capabilities := []string{"completion"}
			if request["model"] != "chat-model" {
				capabilities = []string{"embedding"}
			}
			json.NewEncoder(w).Encode(map[string]any{"capabilities": capabilities})
		default:
			t.Fatalf("path=%q", r.URL.Path)
		}
	}))
	defer server.Close()
	model, err := DiscoverEmbeddingModel(context.Background(), server.URL, "")
	if err != nil || model != "qwen3-embedding:0.6b" {
		t.Fatalf("model=%q err=%v", model, err)
	}
}
