package bootstrap

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/z-chenhao/eri/internal/config"
)

type unusedBroker struct{}

func (unusedBroker) Ensure(context.Context) error { return nil }

type scriptedPrompter struct {
	lines   []string
	secrets []string
}

func (p *scriptedPrompter) ReadLine(string) (string, error) {
	if len(p.lines) == 0 {
		return "", io.EOF
	}
	value := p.lines[0]
	p.lines = p.lines[1:]
	return value, nil
}

func (p *scriptedPrompter) ReadSecret(string) (string, error) {
	if len(p.secrets) == 0 {
		return "", io.EOF
	}
	value := p.secrets[0]
	p.secrets = p.secrets[1:]
	return value, nil
}

func TestTerminalSetupValidatesOllamaBeforeCommittingProfile(t *testing.T) {
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			io.WriteString(w, `{"models":[{"name":"qwen-test","size":1073741824}]}`)
		case "/api/show":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"capabilities":["tools"],"model_info":{"qwen.context_length":32768}}`)
		default:
			t.Fatalf("Ollama path = %q", r.URL.Path)
		}
	}))
	defer ollama.Close()
	root := t.TempDir()
	cfg := config.Config{
		OllamaURL: ollama.URL, ModelConfigPath: filepath.Join(root, "configuration", "model.json"),
	}
	prompt := &scriptedPrompter{lines: []string{"", ""}}
	var output bytes.Buffer
	configured, err := Run(context.Background(), cfg, unusedBroker{}, prompt, &output)
	if err != nil {
		t.Fatal(err)
	}
	if !configured.ModelConfigured || configured.ModelProvider != "ollama" || configured.Model != "qwen-test" {
		t.Fatalf("configured model = %+v", configured)
	}
	body, err := os.ReadFile(cfg.ModelConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"provider": "ollama"`) || strings.Contains(strings.ToLower(string(body)), "api_key") {
		t.Fatalf("unsafe model profile: %s", body)
	}
	if !strings.Contains(output.String(), "native tool calling") || !strings.Contains(output.String(), "ollama / qwen-test") {
		t.Fatalf("setup output = %q", output.String())
	}
}

func TestTerminalSetupRejectsNonTTYInput(t *testing.T) {
	if _, err := NewTerminalPrompter(strings.NewReader("1\n"), io.Discard); !errors.Is(err, ErrInteractiveTerminalRequired) {
		t.Fatalf("non-TTY input error = %v", err)
	}
}

func TestDiscoverOllamaPrioritizesConfiguredRecommendation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"models":[{"name":"qwen3.6:27b-q4_K_M","size":10},{"name":"qwen3.6:35b-a3b-q4_K_M","size":20}]}`)
	}))
	defer server.Close()
	models, reachable := discoverOllama(context.Background(), server.URL, "qwen3.6:35b-a3b-q4_K_M")
	if !reachable || len(models) != 2 || models[0].Name != "qwen3.6:35b-a3b-q4_K_M" {
		t.Fatalf("discovered models = reachable:%v models:%+v", reachable, models)
	}
}
