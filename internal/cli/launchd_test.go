package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/z-chenhao/eri/internal/config"
)

func TestLaunchdPlistContainsNoCredentialAndEscapesPaths(t *testing.T) {
	body := string(launchdPlist("/Applications/Eri & Me/eri", config.Config{
		DataRoot: "/Users/me/Eri & data", WorkspaceRoot: "/Users/me/work <private>",
	}))
	for _, required := range []string{"io.github.z-chenhao.eri", "Eri &amp; Me", "Eri &amp; data", "WorkingDirectory", "work &lt;private&gt;", "KeepAlive", "daemon", "Umask", "bootstrap.log", "ERI_GOOGLE_AUTH_BROKER", "google-auth-issuer.sock", "ERI_GOOGLE_AUTH_REDEMPTION_BROKER", "google-auth-redemption.sock"} {
		if !strings.Contains(body, required) {
			t.Fatalf("plist missing %q: %s", required, body)
		}
	}
	for _, forbidden := range []string{"DEEPSEEK_API_KEY", "TOKEN", "PASSWORD", "COOKIE"} {
		if strings.Contains(strings.ToUpper(body), forbidden) {
			t.Fatalf("plist contains credential field %q", forbidden)
		}
	}
}

func TestBootstrapLogKeepsOnlyLatestProtectedStartupDiagnostic(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ERI_DATA_ROOT", root)
	path := filepath.Join(root, "logs", "bootstrap.log")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("old startup failure"), 0o644); err != nil {
		t.Fatal(err)
	}
	truncateBootstrapLog()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("bootstrap log size=%d mode=%v", info.Size(), info.Mode().Perm())
	}
	if directory, err := os.Stat(filepath.Dir(path)); err != nil || directory.Mode().Perm() != 0o700 {
		t.Fatalf("bootstrap directory=%v err=%v", directory, err)
	}
}
