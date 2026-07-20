package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClientConfigMustRemainOutsideEriDataRoot(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ERI_DATA_ROOT", root)
	inside := filepath.Join(root, "oauth", "client.json")
	if err := os.MkdirAll(filepath.Dir(inside), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inside, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := requireClientConfigOutsideEriDataRoot(inside); err == nil {
		t.Fatal("OAuth client file inside EriDataRoot was accepted")
	}
	outside := filepath.Join(t.TempDir(), "client.json")
	if err := os.WriteFile(outside, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := requireClientConfigOutsideEriDataRoot(outside); err != nil {
		t.Fatalf("outside OAuth client file rejected: %v", err)
	}
}

func TestBrokerLaunchdPlistContainsOnlyPathsAndNoCredential(t *testing.T) {
	body := string(brokerLaunchdPlist("/Applications/Eri & Me/eri-google-auth-broker", brokerServiceConfig{
		ClientConfig: "/Users/me/Secrets/google & eri.json", IssuerSocket: "/Users/me/Eri/runtime/issuer.sock",
		RedemptionSocket: "/Users/me/Eri/runtime/redemption.sock", Callback: "127.0.0.1:7792", DataRoot: "/Users/me/Eri & data",
	}))
	for _, required := range []string{brokerLaunchdLabel, "Eri &amp; Me", "google &amp; eri.json", "issuer.sock", "redemption.sock", "127.0.0.1:7792", "KeepAlive", "Umask", "google-auth-broker-bootstrap.log", "ERI_DATA_ROOT", "Eri &amp; data"} {
		if !strings.Contains(body, required) {
			t.Fatalf("plist missing %q: %s", required, body)
		}
	}
	for _, forbidden := range []string{"REFRESH_TOKEN", "ACCESS_TOKEN", "CLIENT_SECRET", "PASSWORD", "COOKIE"} {
		if strings.Contains(strings.ToUpper(body), forbidden) {
			t.Fatalf("plist contains credential field %q", forbidden)
		}
	}
}

func TestBrokerBootstrapLogKeepsOnlyLatestProtectedDiagnostic(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ERI_DATA_ROOT", root)
	path := filepath.Join(root, "logs", "google-auth-broker-bootstrap.log")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	truncateBrokerBootstrapLog()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("bootstrap log size=%d mode=%v", info.Size(), info.Mode().Perm())
	}
}

func TestDefaultSocketPathsFollowEriDataRoot(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ERI_DATA_ROOT", root)
	issuer, redemption, err := defaultSocketPaths()
	if err != nil {
		t.Fatal(err)
	}
	if issuer != filepath.Join(root, "runtime", "google-auth-issuer.sock") || redemption != filepath.Join(root, "runtime", "google-auth-redemption.sock") {
		t.Fatalf("issuer=%q redemption=%q", issuer, redemption)
	}
}

func TestDefaultSocketPathsUseProjectLocalDataRoot(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("ERI_DATA_ROOT", "")
	t.Setenv("ERI_WORKSPACE_ROOT", workspace)
	issuer, redemption, err := defaultSocketPaths()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(workspace, ".eri", "runtime")
	if issuer != filepath.Join(root, "google-auth-issuer.sock") || redemption != filepath.Join(root, "google-auth-redemption.sock") {
		t.Fatalf("issuer=%q redemption=%q", issuer, redemption)
	}
}
