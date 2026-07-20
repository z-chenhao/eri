// Package bootstrap owns the short-lived terminal setup boundary before Eri's
// durable Agent Runtime is composed.
package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/z-chenhao/eri/internal/config"
	"github.com/z-chenhao/eri/internal/model/ollama"
	"github.com/z-chenhao/eri/internal/providersecret"
)

var ErrInteractiveTerminalRequired = errors.New("first-run model setup requires an interactive terminal")

type ollamaModel struct {
	Name string
	Size string
}

// Prompter is the small user-input boundary for first-run setup. Production
// uses a real terminal; tests provide a deterministic implementation.
type Prompter interface {
	ReadLine(prompt string) (string, error)
	ReadSecret(prompt string) (string, error)
}

type terminalPrompter struct {
	input  *os.File
	output io.Writer
}

// NewTerminalPrompter accepts only a TTY. This prevents a launchd process or a
// redirected stdin from silently consuming setup answers or a cloud secret.
func NewTerminalPrompter(input io.Reader, output io.Writer) (Prompter, error) {
	file, ok := input.(*os.File)
	if !ok || !term.IsTerminal(int(file.Fd())) {
		return nil, ErrInteractiveTerminalRequired
	}
	return &terminalPrompter{input: file, output: output}, nil
}

func (p *terminalPrompter) ReadLine(prompt string) (string, error) {
	if _, err := fmt.Fprint(p.output, prompt); err != nil {
		return "", err
	}
	return readTerminalLine(p.input)
}

func (p *terminalPrompter) ReadSecret(prompt string) (string, error) {
	if _, err := fmt.Fprint(p.output, prompt); err != nil {
		return "", err
	}
	value, err := term.ReadPassword(int(p.input.Fd()))
	_, _ = fmt.Fprintln(p.output)
	if err != nil {
		return "", err
	}
	return string(value), nil
}

// readTerminalLine reads only through the newline. A buffered reader could
// prefetch a subsequently pasted API key before ReadPassword disables echo.
func readTerminalLine(input *os.File) (string, error) {
	var value strings.Builder
	one := make([]byte, 1)
	for {
		count, err := input.Read(one)
		if count > 0 {
			switch one[0] {
			case '\n':
				return value.String(), nil
			case '\r':
			default:
				value.WriteByte(one[0])
			}
		}
		if err != nil {
			if value.Len() > 0 && errors.Is(err, io.EOF) {
				return value.String(), nil
			}
			return "", err
		}
	}
}

// Run interactively selects, validates, and persists one model binding. It
// never starts an HTTP listener; a cloud credential is passed only to the
// isolated Broker for Keychain storage and never enters Eri's data files.
func Run(ctx context.Context, cfg config.Config, broker interface{ Ensure(context.Context) error }, prompt Prompter, output io.Writer) (config.Config, error) {
	if broker == nil {
		return config.Config{}, fmt.Errorf("provider broker process manager is required")
	}
	if prompt == nil {
		return config.Config{}, ErrInteractiveTerminalRequired
	}
	if output == nil {
		output = io.Discard
	}

	fmt.Fprintln(output, "Eri needs a model resource before the first start.")
	models, ollamaReachable := discoverOllama(ctx, cfg.OllamaURL, config.DefaultOllamaModel())
	if ollamaReachable {
		fmt.Fprintf(output, "  1. Local Ollama (local-first, %d models found)\n", len(models))
	} else {
		fmt.Fprintln(output, "  1. Local Ollama (local-first, currently unavailable)")
	}
	if runtime.GOOS == "darwin" {
		fmt.Fprintln(output, "  2. DeepSeek (cloud, usage-based billing; the key is stored only in macOS Keychain)")
	} else {
		fmt.Fprintln(output, "  2. DeepSeek (secure credential storage is unavailable on this system)")
	}

	provider, err := chooseProvider(ctx, prompt, output)
	if err != nil {
		return config.Config{}, err
	}
	var profile config.ModelProfile
	switch provider {
	case "ollama":
		profile, err = configureOllama(ctx, cfg, models, ollamaReachable, prompt, output)
	case "deepseek":
		profile, err = configureDeepSeek(ctx, cfg, broker, prompt, output)
	default:
		return config.Config{}, fmt.Errorf("unsupported model provider %q", provider)
	}
	if err != nil {
		return config.Config{}, err
	}
	if ollamaReachable {
		if embeddingModel, discoveryErr := ollama.DiscoverEmbeddingModel(ctx, cfg.OllamaURL, cfg.MemoryEmbeddingModel); discoveryErr == nil && embeddingModel != "" {
			profile.MemoryEmbeddingModel = embeddingModel
			profile.OllamaURL = cfg.OllamaURL
			fmt.Fprintf(output, "Semantic Memory: local Ollama / %s\n", embeddingModel)
		} else {
			fmt.Fprintln(output, "Semantic Memory: lexical and associative retrieval; install a local Ollama embedding model to enable semantic recall.")
		}
	}
	if err := config.SaveModelProfile(cfg.ModelConfigPath, profile); err != nil {
		if profile.Provider == "deepseek" {
			_ = providersecret.NewClient(cfg.ProviderBrokerSocket).DeleteDeepSeek(ctx)
		}
		return config.Config{}, fmt.Errorf("save model configuration: %w", err)
	}

	cfg.ModelConfigured = true
	cfg.ModelProvider = profile.Provider
	cfg.Model = profile.Model
	if profile.OllamaURL != "" {
		cfg.OllamaURL = profile.OllamaURL
	}
	if profile.DeepSeekURL != "" {
		cfg.DeepSeekURL = profile.DeepSeekURL
	}
	cfg.DeepSeekKeySet = false
	cfg.MemoryEmbeddingModel = profile.MemoryEmbeddingModel
	fmt.Fprintf(output, "Model resource configured: %s / %s\n", profile.Provider, profile.Model)
	return cfg, nil
}

func chooseProvider(ctx context.Context, prompt Prompter, output io.Writer) (string, error) {
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		value, err := prompt.ReadLine("Select provider [1]: ")
		if err != nil {
			return "", fmt.Errorf("read provider selection: %w", err)
		}
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "", "1", "ollama":
			return "ollama", nil
		case "2", "deepseek":
			return "deepseek", nil
		default:
			fmt.Fprintln(output, "Enter 1 or 2.")
		}
	}
}

func configureOllama(ctx context.Context, cfg config.Config, models []ollamaModel, reachable bool, prompt Prompter, output io.Writer) (config.ModelProfile, error) {
	if !reachable {
		return config.ModelProfile{}, fmt.Errorf("Ollama is unavailable at %s; start Ollama and run `eri daemon` again", cfg.OllamaURL)
	}
	if len(models) == 0 {
		return config.ModelProfile{}, fmt.Errorf("Ollama has no installed models; run `ollama pull %s` and start Eri again", config.DefaultOllamaModel())
	}
	fmt.Fprintln(output, "\nAvailable Ollama models:")
	for index, model := range models {
		suffix := ""
		if model.Size != "" {
			suffix = " · " + model.Size
		}
		fmt.Fprintf(output, "  %d. %s%s\n", index+1, model.Name, suffix)
	}
	selection, err := chooseModel(ctx, prompt, output, models)
	if err != nil {
		return config.ModelProfile{}, err
	}
	fmt.Fprintf(output, "Validating the context window and native tool calling for %s…\n", selection.Name)
	capabilities, err := ollama.New(cfg.OllamaURL, selection.Name, 30*time.Second).Capabilities(ctx)
	if err != nil {
		return config.ModelProfile{}, fmt.Errorf("inspect selected Ollama model: %w", err)
	}
	if !capabilities.Text || !capabilities.ToolCalling || capabilities.ContextTokens <= 0 {
		return config.ModelProfile{}, fmt.Errorf("Eri requires a text model with native Tool Calling and a declared context window")
	}
	return config.ModelProfile{Provider: "ollama", Model: selection.Name, OllamaURL: cfg.OllamaURL}, nil
}

func chooseModel(ctx context.Context, prompt Prompter, output io.Writer, models []ollamaModel) (ollamaModel, error) {
	for {
		if err := ctx.Err(); err != nil {
			return ollamaModel{}, err
		}
		value, err := prompt.ReadLine("Select model [1]: ")
		if err != nil {
			return ollamaModel{}, fmt.Errorf("read model selection: %w", err)
		}
		value = strings.TrimSpace(value)
		if value == "" {
			return models[0], nil
		}
		index, err := strconv.Atoi(value)
		if err == nil && index >= 1 && index <= len(models) {
			return models[index-1], nil
		}
		for _, model := range models {
			if value == model.Name {
				return model, nil
			}
		}
		fmt.Fprintf(output, "Enter a number from 1 to %d.\n", len(models))
	}
}

func configureDeepSeek(ctx context.Context, cfg config.Config, broker interface{ Ensure(context.Context) error }, prompt Prompter, output io.Writer) (config.ModelProfile, error) {
	if runtime.GOOS != "darwin" {
		return config.ModelProfile{}, fmt.Errorf("secure DeepSeek credential storage currently requires macOS Keychain")
	}
	model, err := prompt.ReadLine("DeepSeek model [" + config.DefaultDeepSeekModel() + "]: ")
	if err != nil {
		return config.ModelProfile{}, fmt.Errorf("read DeepSeek model: %w", err)
	}
	model = strings.TrimSpace(model)
	if model == "" {
		model = config.DefaultDeepSeekModel()
	}
	apiKey, err := prompt.ReadSecret("DeepSeek API key (input is hidden): ")
	if err != nil {
		return config.ModelProfile{}, fmt.Errorf("read DeepSeek API key: %w", err)
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return config.ModelProfile{}, fmt.Errorf("DeepSeek API key is required")
	}
	fmt.Fprintln(output, "Validating the DeepSeek credential and model…")
	if err := broker.Ensure(ctx); err != nil {
		return config.ModelProfile{}, fmt.Errorf("start secure credential broker: %w", err)
	}
	_, err = providersecret.NewClient(cfg.ProviderBrokerSocket).ConfigureDeepSeek(ctx, cfg.DeepSeekURL, apiKey, model)
	if err != nil {
		if errors.Is(err, providersecret.ErrModelUnavailable) {
			return config.ModelProfile{}, fmt.Errorf("DeepSeek model %q is unavailable to this account", model)
		}
		return config.ModelProfile{}, fmt.Errorf("DeepSeek rejected the credential or could not be reached")
	}
	return config.ModelProfile{Provider: "deepseek", Model: model, DeepSeekURL: cfg.DeepSeekURL}, nil
}

func discoverOllama(ctx context.Context, baseURL, preferredModel string) ([]ollamaModel, bool) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/api/tags", nil)
	if err != nil {
		return nil, false
	}
	response, err := (&http.Client{Timeout: 3 * time.Second}).Do(request)
	if err != nil {
		return nil, false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64*1024))
		return nil, false
	}
	var inventory struct {
		Models []struct {
			Name  string `json:"name"`
			Model string `json:"model"`
			Size  int64  `json:"size"`
		} `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 2*1024*1024)).Decode(&inventory); err != nil {
		return nil, true
	}
	models := make([]ollamaModel, 0, len(inventory.Models))
	for _, candidate := range inventory.Models {
		name := strings.TrimSpace(candidate.Name)
		if name == "" {
			name = strings.TrimSpace(candidate.Model)
		}
		if name == "" {
			continue
		}
		models = append(models, ollamaModel{Name: name, Size: humanSize(candidate.Size)})
	}
	sort.SliceStable(models, func(i, j int) bool {
		preferredI := models[i].Name == preferredModel
		preferredJ := models[j].Name == preferredModel
		if preferredI != preferredJ {
			return preferredI
		}
		return models[i].Name < models[j].Name
	})
	return models, true
}

func humanSize(size int64) string {
	if size <= 0 {
		return ""
	}
	const gib = 1024 * 1024 * 1024
	if size >= gib {
		return fmt.Sprintf("%.1f GB", float64(size)/gib)
	}
	return fmt.Sprintf("%.0f MB", float64(size)/(1024*1024))
}
