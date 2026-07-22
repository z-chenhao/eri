// Package config resolves Eri's technical bootstrap configuration.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	defaultConversationAddr = "127.0.0.1:7780"
	defaultObservatoryAddr  = "127.0.0.1:7781"
	defaultOllamaURL        = "http://127.0.0.1:11434"
	defaultDeepSeekURL      = "https://api.deepseek.com"
	defaultOllamaModel      = "qwen3.6:35b-a3b-q4_K_M"
	defaultDeepSeekModel    = "deepseek-v4-flash"
)

// DefaultOllamaModel is the creator-instance recommendation shown when it is
// present in the discovered local inventory.
func DefaultOllamaModel() string { return defaultOllamaModel }

// DefaultDeepSeekModel is the ordinary single-model cloud recommendation.
func DefaultDeepSeekModel() string { return defaultDeepSeekModel }

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

// Config contains daemon bootstrap values. Assistant-level preferences are
// deliberately not represented here; they are learned through conversation.
type Config struct {
	DataRoot                 string
	DatabasePath             string
	SocketPath               string
	ConversationAddr         string
	ObservatoryAddr          string
	ModelProvider            string
	ModelConfigured          bool
	ModelEnvironmentOverride bool
	ModelConfigPath          string
	ProviderBrokerSocket     string
	OllamaURL                string
	DeepSeekURL              string
	DeepSeekKeySet           bool
	DebugLog                 bool
	Model                    string
	ModelTimeout             time.Duration
	MaxEvalAttempts          int
	MaxOutputTokens          int
	ApprovalTTL              time.Duration
	PollInterval             time.Duration
	WorkspaceRoot            string
	UserSkillRoot            string
	TavilyKeySet             bool
	TavilySearchDepth        string
	TavilyExtractDepth       string
	MCPServersJSON           string
	CodexPath                string
	CodexTimeout             time.Duration
	MemoryEmbeddingModel     string
	LarkEnabled              bool
	LarkAppID                string
	LarkAppSecretSet         bool
	LarkOwnerOpenID          string
	LarkBrand                string
}

// ModelProfile is the non-secret, user-owned model binding selected during
// first-run setup. Provider credentials never belong in this file.
type ModelProfile struct {
	Version              int    `json:"version"`
	Provider             string `json:"provider"`
	Model                string `json:"model"`
	OllamaURL            string `json:"ollama_url,omitempty"`
	DeepSeekURL          string `json:"deepseek_url,omitempty"`
	MemoryEmbeddingModel string `json:"memory_embedding_model,omitempty"`
}

// Load returns local-first defaults with narrow environment overrides for
// developer and deployment use.
func Load() (Config, error) {
	workspaceRoot, err := resolveWorkspaceRoot()
	if err != nil {
		return Config{}, err
	}
	root, err := resolveDataRoot(workspaceRoot)
	if err != nil {
		return Config{}, err
	}
	userSkillRoot := ""
	if userHome, homeErr := os.UserHomeDir(); homeErr == nil && strings.TrimSpace(userHome) != "" {
		userSkillRoot = filepath.Join(userHome, ".eri", "skills")
	}

	modelConfigPath := filepath.Join(root, "configuration", "model.json")
	profile, profileFound, err := loadModelProfile(modelConfigPath)
	if err != nil {
		return Config{}, err
	}
	explicitProvider := strings.TrimSpace(os.Getenv("ERI_MODEL_PROVIDER"))
	providerFallback := "ollama"
	if profileFound {
		providerFallback = profile.Provider
	}
	provider := strings.ToLower(envOr("ERI_MODEL_PROVIDER", providerFallback))
	defaultModel := defaultOllamaModel
	if provider == "deepseek" {
		defaultModel = defaultDeepSeekModel
	}
	if profileFound && profile.Provider == provider {
		defaultModel = profile.Model
	}
	ollamaURLFallback := defaultOllamaURL
	deepSeekURLFallback := defaultDeepSeekURL
	if profileFound {
		if profile.OllamaURL != "" {
			ollamaURLFallback = profile.OllamaURL
		}
		if profile.DeepSeekURL != "" {
			deepSeekURLFallback = profile.DeepSeekURL
		}
	}
	maxOutputTokens, err := positiveIntEnv("ERI_MAX_OUTPUT_TOKENS", 1024)
	if err != nil {
		return Config{}, err
	}
	maxEvalAttempts, err := positiveIntEnv("ERI_MAX_EVAL_ATTEMPTS", 3)
	if err != nil {
		return Config{}, err
	}
	debugLog, err := boolEnv("ERI_DEBUG_LOG", false)
	if err != nil {
		return Config{}, err
	}
	approvalTTL, err := durationEnv("ERI_APPROVAL_TTL", 15*time.Minute, time.Minute, 24*time.Hour)
	if err != nil {
		return Config{}, err
	}
	codexTimeout, err := durationEnv("ERI_CODEX_TIMEOUT", 30*time.Minute, time.Minute, 6*time.Hour)
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		DataRoot:                 root,
		DatabasePath:             filepath.Join(root, "metadata", "eri.db"),
		SocketPath:               filepath.Join(root, "runtime", "eri.sock"),
		ConversationAddr:         envOr("ERI_CONVERSATION_ADDR", defaultConversationAddr),
		ObservatoryAddr:          envOr("ERI_OBSERVATORY_ADDR", defaultObservatoryAddr),
		ModelProvider:            provider,
		ModelConfigured:          profileFound || explicitProvider != "" || strings.TrimSpace(os.Getenv("ERI_MODEL")) != "",
		ModelEnvironmentOverride: explicitProvider != "" || strings.TrimSpace(os.Getenv("ERI_MODEL")) != "",
		ModelConfigPath:          modelConfigPath,
		ProviderBrokerSocket:     filepath.Join(root, "runtime", "provider-secret-broker.sock"),
		OllamaURL:                strings.TrimRight(envOr("ERI_OLLAMA_URL", ollamaURLFallback), "/"),
		DeepSeekURL:              strings.TrimRight(envOr("ERI_DEEPSEEK_URL", deepSeekURLFallback), "/"),
		DeepSeekKeySet:           strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY")) != "",
		DebugLog:                 debugLog,
		Model:                    envOr("ERI_MODEL", defaultModel),
		ModelTimeout:             5 * time.Minute,
		MaxEvalAttempts:          maxEvalAttempts,
		MaxOutputTokens:          maxOutputTokens,
		ApprovalTTL:              approvalTTL,
		PollInterval:             100 * time.Millisecond,
		WorkspaceRoot:            workspaceRoot,
		UserSkillRoot:            userSkillRoot,
		TavilyKeySet:             strings.TrimSpace(os.Getenv("TAVILY_API_KEY")) != "",
		TavilySearchDepth:        strings.ToLower(strings.TrimSpace(envOr("TAVILY_SEARCH_DEPTH", "basic"))),
		TavilyExtractDepth:       strings.ToLower(strings.TrimSpace(envOr("TAVILY_EXTRACT_DEPTH", "basic"))),
		MCPServersJSON:           strings.TrimSpace(os.Getenv("ERI_MCP_SERVERS_JSON")),
		CodexPath:                strings.TrimSpace(os.Getenv("ERI_CODEX_PATH")),
		CodexTimeout:             codexTimeout,
		MemoryEmbeddingModel:     strings.TrimSpace(envOr("ERI_MEMORY_EMBEDDING_MODEL", profile.MemoryEmbeddingModel)),
		LarkAppID:                strings.TrimSpace(os.Getenv("LARK_ERI_API_KEY")),
		LarkAppSecretSet:         strings.TrimSpace(os.Getenv("LARK_ERI_API_SECRET")) != "",
		LarkOwnerOpenID:          strings.TrimSpace(os.Getenv("LARK_ERI_OWNER_OPEN_ID")),
		LarkBrand:                strings.ToLower(strings.TrimSpace(envOr("LARK_ERI_BRAND", "feishu"))),
	}
	larkCredentials := 0
	if cfg.LarkAppID != "" {
		larkCredentials++
	}
	if cfg.LarkAppSecretSet {
		larkCredentials++
	}
	if larkCredentials == 1 {
		return Config{}, fmt.Errorf("LARK_ERI_API_KEY and LARK_ERI_API_SECRET must be configured together")
	}
	if cfg.LarkBrand != "feishu" && cfg.LarkBrand != "lark" {
		return Config{}, fmt.Errorf("LARK_ERI_BRAND must be feishu or lark")
	}
	if !oneOf(cfg.TavilySearchDepth, "basic", "advanced", "fast", "ultra-fast") {
		return Config{}, fmt.Errorf("TAVILY_SEARCH_DEPTH must be basic, advanced, fast, or ultra-fast")
	}
	if !oneOf(cfg.TavilyExtractDepth, "basic", "advanced") {
		return Config{}, fmt.Errorf("TAVILY_EXTRACT_DEPTH must be basic or advanced")
	}
	if larkCredentials == 2 {
		if !strings.HasPrefix(cfg.LarkAppID, "cli_") || (cfg.LarkOwnerOpenID != "" && !strings.HasPrefix(cfg.LarkOwnerOpenID, "ou_")) {
			return Config{}, fmt.Errorf("Lark App ID and owner Open ID have invalid formats")
		}
		cfg.LarkEnabled = true
	}
	for name, address := range map[string]string{
		"ERI_CONVERSATION_ADDR": cfg.ConversationAddr,
		"ERI_OBSERVATORY_ADDR":  cfg.ObservatoryAddr,
	} {
		if err := validateLoopbackListenAddress(address); err != nil {
			return Config{}, fmt.Errorf("%s: %w", name, err)
		}
	}
	if err := validateProviderURL(cfg.OllamaURL, true); err != nil {
		return Config{}, fmt.Errorf("ERI_OLLAMA_URL: %w", err)
	}
	if err := validateProviderURL(cfg.DeepSeekURL, false); err != nil {
		return Config{}, fmt.Errorf("ERI_DEEPSEEK_URL: %w", err)
	}
	return cfg, nil
}

// SaveModelProfile atomically persists only non-secret model configuration.
func SaveModelProfile(path string, profile ModelProfile) error {
	profile.Version = 1
	profile.Provider = strings.ToLower(strings.TrimSpace(profile.Provider))
	profile.Model = strings.TrimSpace(profile.Model)
	profile.OllamaURL = strings.TrimRight(strings.TrimSpace(profile.OllamaURL), "/")
	profile.DeepSeekURL = strings.TrimRight(strings.TrimSpace(profile.DeepSeekURL), "/")
	profile.MemoryEmbeddingModel = strings.TrimSpace(profile.MemoryEmbeddingModel)
	if profile.Provider != "ollama" && profile.Provider != "deepseek" {
		return fmt.Errorf("model provider must be ollama or deepseek")
	}
	if profile.Model == "" {
		return fmt.Errorf("model name is required")
	}
	if profile.Provider == "ollama" {
		if err := validateProviderURL(profile.OllamaURL, true); err != nil {
			return fmt.Errorf("Ollama URL: %w", err)
		}
	} else if err := validateProviderURL(profile.DeepSeekURL, false); err != nil {
		return fmt.Errorf("DeepSeek URL: %w", err)
	}
	if profile.MemoryEmbeddingModel != "" {
		if err := validateProviderURL(profile.OllamaURL, true); err != nil {
			return fmt.Errorf("Ollama embedding URL: %w", err)
		}
	}
	body, err := json.MarshalIndent(profile, "", "  ")
	if err != nil {
		return fmt.Errorf("encode model profile: %w", err)
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create model configuration directory: %w", err)
	}
	info, err := os.Lstat(directory)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("model configuration directory must be a real directory")
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return fmt.Errorf("protect model configuration directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, "model-*.json")
	if err != nil {
		return fmt.Errorf("create temporary model profile: %w", err)
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(body, '\n')); err != nil {
		temporary.Close()
		return fmt.Errorf("write model profile: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("commit model profile: %w", err)
	}
	return os.Chmod(path, 0o600)
}

func loadModelProfile(path string) (ModelProfile, bool, error) {
	info, statErr := os.Lstat(path)
	if statErr == nil && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular()) {
		return ModelProfile{}, false, fmt.Errorf("model profile must be a regular file")
	}
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return ModelProfile{}, false, fmt.Errorf("inspect model profile: %w", statErr)
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return ModelProfile{}, false, nil
	}
	if err != nil {
		return ModelProfile{}, false, fmt.Errorf("open model profile: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 64*1024))
	decoder.DisallowUnknownFields()
	var profile ModelProfile
	if err := decoder.Decode(&profile); err != nil {
		return ModelProfile{}, false, fmt.Errorf("decode model profile: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ModelProfile{}, false, fmt.Errorf("decode model profile: trailing content is not allowed")
	}
	if profile.Version != 1 || strings.TrimSpace(profile.Model) == "" {
		return ModelProfile{}, false, fmt.Errorf("model profile is invalid")
	}
	profile.Provider = strings.ToLower(strings.TrimSpace(profile.Provider))
	if profile.Provider != "ollama" && profile.Provider != "deepseek" {
		return ModelProfile{}, false, fmt.Errorf("model profile uses unsupported provider %q", profile.Provider)
	}
	return profile, true, nil
}

func validateLoopbackListenAddress(address string) error {
	host, port, err := net.SplitHostPort(strings.TrimSpace(address))
	if err != nil {
		return fmt.Errorf("must be a numeric loopback host and port")
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("must bind only to a numeric loopback address")
	}
	parsedPort, err := strconv.Atoi(port)
	if err != nil || parsedPort < 0 || parsedPort > 65535 {
		return fmt.Errorf("port must be between 0 and 65535")
	}
	return nil
}

func validateProviderURL(value string, loopbackOnly bool) error {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("must be an absolute provider URL without credentials, query or fragment")
	}
	ip := net.ParseIP(parsed.Hostname())
	loopback := ip != nil && ip.IsLoopback()
	if loopbackOnly && !loopback {
		return fmt.Errorf("must use a numeric loopback host for the local Ollama provider")
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && loopback) {
		return fmt.Errorf("must use HTTPS, except for a numeric loopback HTTP endpoint")
	}
	return nil
}

func positiveIntEnv(name string, fallback int) (int, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return parsed, nil
}

func durationEnv(name string, fallback, minimum, maximum time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed < minimum || parsed > maximum {
		return 0, fmt.Errorf("%s must be a duration between %s and %s", name, minimum, maximum)
	}
	return parsed, nil
}

func boolEnv(name string, fallback bool) (bool, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean", name)
	}
	return parsed, nil
}

// ResolveDataRoot returns the absolute root shared by Eri and its
// out-of-process helpers. By default all non-credential runtime data belongs
// to the current workspace's ignored .eri directory.
func ResolveDataRoot() (string, error) {
	workspaceRoot, err := resolveWorkspaceRoot()
	if err != nil {
		return "", err
	}
	return resolveDataRoot(workspaceRoot)
}

func resolveWorkspaceRoot() (string, error) {
	workspaceRoot, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve current workspace: %w", err)
	}
	if value := strings.TrimSpace(os.Getenv("ERI_WORKSPACE_ROOT")); value != "" {
		workspaceRoot = value
	}
	workspaceRoot, err = filepath.Abs(workspaceRoot)
	if err != nil {
		return "", fmt.Errorf("resolve ERI_WORKSPACE_ROOT: %w", err)
	}
	return workspaceRoot, nil
}

func resolveDataRoot(workspaceRoot string) (string, error) {
	root := strings.TrimSpace(os.Getenv("ERI_DATA_ROOT"))
	if root == "" {
		root = filepath.Join(workspaceRoot, ".eri")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve ERI_DATA_ROOT: %w", err)
	}
	return root, nil
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
