package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadRequiresFirstRunSetupWithoutUserModelBinding(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ERI_DATA_ROOT", root)
	t.Setenv("ERI_MODEL_PROVIDER", "")
	t.Setenv("ERI_MODEL", "")
	t.Setenv("TAVILY_API_KEY", "")
	t.Setenv("TAVILY_SEARCH_DEPTH", "")
	t.Setenv("TAVILY_EXTRACT_DEPTH", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ModelConfigured {
		t.Fatal("default model fallback was mistaken for an explicit user binding")
	}
	if cfg.ModelConfigPath != filepath.Join(root, "configuration", "model.json") {
		t.Fatalf("model config path = %q", cfg.ModelConfigPath)
	}
	if cfg.TavilyKeySet || cfg.TavilySearchDepth != "basic" || cfg.TavilyExtractDepth != "basic" {
		t.Fatalf("default Tavily config = key:%v search:%q extract:%q", cfg.TavilyKeySet, cfg.TavilySearchDepth, cfg.TavilyExtractDepth)
	}
}

func TestLoadValidatesTavilyRuntimeConfiguration(t *testing.T) {
	t.Setenv("ERI_DATA_ROOT", t.TempDir())
	t.Setenv("ERI_WORKSPACE_ROOT", t.TempDir())
	t.Setenv("TAVILY_API_KEY", "runtime-only-test-value")
	t.Setenv("TAVILY_SEARCH_DEPTH", "advanced")
	t.Setenv("TAVILY_EXTRACT_DEPTH", "advanced")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.TavilyKeySet || cfg.TavilySearchDepth != "advanced" || cfg.TavilyExtractDepth != "advanced" {
		t.Fatalf("Tavily config = key:%v search:%q extract:%q", cfg.TavilyKeySet, cfg.TavilySearchDepth, cfg.TavilyExtractDepth)
	}
	t.Setenv("TAVILY_SEARCH_DEPTH", "expensive-magic")
	if _, err := Load(); err == nil {
		t.Fatal("invalid Tavily search depth unexpectedly accepted")
	}
}

func TestLoadHonorsExplicitDataRootWithoutHomeEnvironment(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", "")
	t.Setenv("ERI_DATA_ROOT", root)
	if _, err := Load(); err != nil {
		t.Fatalf("explicit data root still depended on HOME: %v", err)
	}
}

func TestLoadDefaultsDataRootToIgnoredWorkspaceDirectory(t *testing.T) {
	workspace := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ERI_DATA_ROOT", "")
	t.Setenv("ERI_WORKSPACE_ROOT", workspace)
	t.Setenv("ERI_MODEL_PROVIDER", "")
	t.Setenv("ERI_MODEL", "")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(workspace, ".eri")
	if cfg.DataRoot != want || cfg.DatabasePath != filepath.Join(want, "metadata", "eri.db") || cfg.SocketPath != filepath.Join(want, "runtime", "eri.sock") {
		t.Fatalf("project-local paths = root:%q database:%q socket:%q", cfg.DataRoot, cfg.DatabasePath, cfg.SocketPath)
	}
	if cfg.UserSkillRoot != filepath.Join(home, ".eri", "skills") {
		t.Fatalf("Eri user Skill root = %q", cfg.UserSkillRoot)
	}
}

func TestSaveModelProfileBecomesAuthoritativeWithoutPersistingSecrets(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ERI_DATA_ROOT", root)
	t.Setenv("ERI_MODEL_PROVIDER", "")
	t.Setenv("ERI_MODEL", "")
	path := filepath.Join(root, "configuration", "model.json")
	if err := SaveModelProfile(path, ModelProfile{
		Provider: "deepseek", Model: "deepseek-v4-flash", DeepSeekURL: "https://api.deepseek.com",
		OllamaURL: "http://127.0.0.1:11434", MemoryEmbeddingModel: "qwen3-embedding:0.6b",
	}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"api_key", "token", "secret", "password"} {
		if _, found := raw[forbidden]; found {
			t.Fatalf("model profile contains secret field %q", forbidden)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("model profile permissions = %o", info.Mode().Perm())
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.ModelConfigured || cfg.ModelProvider != "deepseek" || cfg.Model != "deepseek-v4-flash" || cfg.MemoryEmbeddingModel != "qwen3-embedding:0.6b" {
		t.Fatalf("loaded model binding = configured:%v provider:%q model:%q memory:%q", cfg.ModelConfigured, cfg.ModelProvider, cfg.Model, cfg.MemoryEmbeddingModel)
	}
}

func TestLoadSelectsCostEfficientDeepSeekDefaultsFromEnvironment(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ERI_DATA_ROOT", filepath.Join(root, "data"))
	t.Setenv("ERI_WORKSPACE_ROOT", root)
	t.Setenv("ERI_MODEL_PROVIDER", "deepseek")
	t.Setenv("ERI_MODEL", "")
	t.Setenv("DEEPSEEK_API_KEY", "runtime-only-test-value")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ModelProvider != "deepseek" || cfg.Model != "deepseek-v4-flash" {
		t.Fatalf("provider defaults = %q/%q", cfg.ModelProvider, cfg.Model)
	}
	if !cfg.DeepSeekKeySet {
		t.Fatal("DeepSeek credential was not sourced from process environment")
	}
	if cfg.MaxEvalAttempts != 3 || cfg.MaxOutputTokens != 1024 {
		t.Fatalf("budget defaults = evals:%d output:%d", cfg.MaxEvalAttempts, cfg.MaxOutputTokens)
	}
	if cfg.ApprovalTTL != 15*time.Minute {
		t.Fatalf("approval TTL = %s", cfg.ApprovalTTL)
	}
}

func TestLoadCanConfigureApprovalTTL(t *testing.T) {
	t.Setenv("ERI_DATA_ROOT", t.TempDir())
	t.Setenv("ERI_WORKSPACE_ROOT", t.TempDir())
	t.Setenv("ERI_APPROVAL_TTL", "30m")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ApprovalTTL != 30*time.Minute {
		t.Fatalf("approval TTL = %s", cfg.ApprovalTTL)
	}
}

func TestLoadConfiguresUnifiedRawDebugLog(t *testing.T) {
	t.Setenv("ERI_DATA_ROOT", t.TempDir())
	t.Setenv("ERI_WORKSPACE_ROOT", t.TempDir())
	t.Setenv("ERI_DEBUG_LOG", "true")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.DebugLog {
		t.Fatal("ERI_DEBUG_LOG did not enable raw provider logging")
	}
	t.Setenv("ERI_DEBUG_LOG", "sometimes")
	if _, err := Load(); err == nil {
		t.Fatal("invalid ERI_DEBUG_LOG value unexpectedly accepted")
	}
}

func TestLoadRejectsUnsafeApprovalTTL(t *testing.T) {
	t.Setenv("ERI_DATA_ROOT", t.TempDir())
	t.Setenv("ERI_WORKSPACE_ROOT", t.TempDir())
	t.Setenv("ERI_APPROVAL_TTL", "25h")
	if _, err := Load(); err == nil {
		t.Fatal("unsafe approval TTL unexpectedly accepted")
	}
}

func TestLoadConfiguresBoundedLocalCodexDelegation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "bin", "codex")
	t.Setenv("ERI_DATA_ROOT", filepath.Join(root, "data"))
	t.Setenv("ERI_WORKSPACE_ROOT", root)
	t.Setenv("ERI_CODEX_PATH", "  "+path+"  ")
	t.Setenv("ERI_CODEX_TIMEOUT", "45m")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CodexPath != path || cfg.CodexTimeout != 45*time.Minute {
		t.Fatalf("Codex configuration = path:%q timeout:%s", cfg.CodexPath, cfg.CodexTimeout)
	}
}

func TestLoadRejectsUnsafeCodexTimeout(t *testing.T) {
	t.Setenv("ERI_DATA_ROOT", t.TempDir())
	t.Setenv("ERI_WORKSPACE_ROOT", t.TempDir())
	t.Setenv("ERI_CODEX_TIMEOUT", "7h")
	if _, err := Load(); err == nil {
		t.Fatal("unsafe Codex timeout unexpectedly accepted")
	}
}

func TestLoadEnablesLarkOnlyWithCompleteRuntimeCredentialBinding(t *testing.T) {
	t.Setenv("ERI_DATA_ROOT", t.TempDir())
	t.Setenv("ERI_WORKSPACE_ROOT", t.TempDir())
	t.Setenv("LARK_ERI_API_KEY", "cli_test")
	t.Setenv("LARK_ERI_API_SECRET", "runtime-only-test-secret")
	t.Setenv("LARK_ERI_OWNER_OPEN_ID", "ou_owner")
	t.Setenv("LARK_ERI_BRAND", "lark")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.LarkEnabled || !cfg.LarkAppSecretSet || cfg.LarkAppID != "cli_test" || cfg.LarkOwnerOpenID != "ou_owner" || cfg.LarkBrand != "lark" {
		t.Fatalf("Lark config = %+v", cfg)
	}
}

func TestLoadRejectsPartialLarkCredentialBinding(t *testing.T) {
	t.Setenv("ERI_DATA_ROOT", t.TempDir())
	t.Setenv("ERI_WORKSPACE_ROOT", t.TempDir())
	t.Setenv("LARK_ERI_API_KEY", "cli_test")
	t.Setenv("LARK_ERI_API_SECRET", "")
	if _, err := Load(); err == nil {
		t.Fatal("partial Lark binding unexpectedly accepted")
	}
}

func TestLoadAllowsExistingOwnerBindingToBeRecoveredFromLocalState(t *testing.T) {
	t.Setenv("ERI_DATA_ROOT", t.TempDir())
	t.Setenv("ERI_WORKSPACE_ROOT", t.TempDir())
	t.Setenv("LARK_ERI_API_KEY", "cli_test")
	t.Setenv("LARK_ERI_API_SECRET", "runtime-only-test-secret")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.LarkEnabled || cfg.LarkOwnerOpenID != "" {
		t.Fatalf("recoverable Lark config = %+v", cfg)
	}
}

func TestLoadRejectsInvalidEvalBudget(t *testing.T) {
	t.Setenv("ERI_DATA_ROOT", t.TempDir())
	t.Setenv("ERI_WORKSPACE_ROOT", t.TempDir())
	t.Setenv("ERI_MAX_EVAL_ATTEMPTS", "0")
	if _, err := Load(); err == nil {
		t.Fatal("zero eval-attempt budget unexpectedly accepted")
	}
}

func TestLoadRejectsWebListenersOutsideNumericLoopback(t *testing.T) {
	for _, address := range []string{"0.0.0.0:7780", "192.168.1.20:7780", "localhost:7780"} {
		t.Run(address, func(t *testing.T) {
			t.Setenv("ERI_DATA_ROOT", t.TempDir())
			t.Setenv("ERI_WORKSPACE_ROOT", t.TempDir())
			t.Setenv("ERI_CONVERSATION_ADDR", address)
			if _, err := Load(); err == nil {
				t.Fatalf("unsafe listener %q accepted", address)
			}
		})
	}
	for _, address := range []string{"127.0.0.1:0", "[::1]:7781"} {
		if err := validateLoopbackListenAddress(address); err != nil {
			t.Fatalf("loopback listener %q rejected: %v", address, err)
		}
	}
}

func TestLoadRejectsProviderURLsThatViolateLocalOrTLSBoundary(t *testing.T) {
	for name, value := range map[string]string{
		"remote Ollama":          "https://models.example.com",
		"plaintext DeepSeek":     "http://api.deepseek.com",
		"credential-bearing API": "https://user:secret@api.deepseek.com",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("ERI_DATA_ROOT", t.TempDir())
			t.Setenv("ERI_WORKSPACE_ROOT", t.TempDir())
			if name == "remote Ollama" {
				t.Setenv("ERI_OLLAMA_URL", value)
			} else {
				t.Setenv("ERI_DEEPSEEK_URL", value)
			}
			if _, err := Load(); err == nil {
				t.Fatalf("unsafe provider URL %q accepted", value)
			}
		})
	}
}
