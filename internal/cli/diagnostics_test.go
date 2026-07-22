package cli

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/z-chenhao/eri/internal/config"
)

func TestLogsFiltersByTaskAndRedactsAgainAtReadBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	body := strings.Join([]string{
		`{"msg":"first","task_id":"task-other"}`,
		`{"msg":"selected","task_id":"task-1","token":"private-value"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if _, err := printLogTail(path, 20, "task-1", &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "selected") || strings.Contains(output.String(), "task-other") || strings.Contains(output.String(), "private-value") {
		t.Fatalf("log output=%s", output.String())
	}
}

func TestDiagnoseCreatesReviewableRedactedArchive(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	logBody := `{"msg":"task failed","error":"token=private-diagnostic-value"}` + "\n" +
		`{"msg":"raw model provider response","body":"private-raw-provider-body"}` + "\n"
	if err := os.WriteFile(filepath.Join(root, "logs", "daemon.log"), []byte(logBody), 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "bundle.zip")
	cfg := config.Config{
		DataRoot: root, ModelProvider: "ollama", Model: "test-model", ConversationAddr: "127.0.0.1:7780", ObservatoryAddr: "127.0.0.1:7781",
		LarkEnabled: true, LarkBrand: "feishu", LarkAppID: "cli_private", LarkOwnerOpenID: "ou_private", LarkAppSecretSet: true,
		TavilyKeySet: true, TavilySearchDepth: "basic", TavilyExtractDepth: "advanced",
	}
	if err := writeDiagnosticBundle(context.Background(), cfg, output); err != nil {
		t.Fatal(err)
	}
	archive, err := zip.OpenReader(output)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	entries := map[string]string{}
	for _, file := range archive.File {
		reader, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(reader)
		reader.Close()
		if err != nil {
			t.Fatal(err)
		}
		entries[file.Name] = string(body)
	}
	for _, required := range []string{"manifest.json", "configuration.json", "doctor.txt", "logs/daemon.log"} {
		if _, exists := entries[required]; !exists {
			t.Fatalf("diagnostic archive is missing %s: %v", required, entries)
		}
	}
	if strings.Contains(entries["logs/daemon.log"], "private-diagnostic-value") || strings.Contains(entries["logs/daemon.log"], "private-raw-provider-body") || !strings.Contains(entries["logs/daemon.log"], "[REDACTED]") {
		t.Fatalf("diagnostic log was not redacted: %s", entries["logs/daemon.log"])
	}
	if !strings.Contains(entries["configuration.json"], `"lark_enabled": true`) || !strings.Contains(entries["configuration.json"], `"lark_brand": "feishu"`) {
		t.Fatalf("diagnostic configuration is missing safe Lark state: %s", entries["configuration.json"])
	}
	if !strings.Contains(entries["configuration.json"], `"web_enabled": true`) || !strings.Contains(entries["configuration.json"], `"web_provider": "tavily"`) {
		t.Fatalf("diagnostic configuration is missing safe Web state: %s", entries["configuration.json"])
	}
	if !strings.Contains(entries["configuration.json"], `"raw_model_debug": false`) {
		t.Fatalf("diagnostic configuration is missing raw debug state: %s", entries["configuration.json"])
	}
	for _, sensitive := range []string{"cli_private", "ou_private", "app_secret"} {
		if strings.Contains(entries["configuration.json"], sensitive) {
			t.Fatalf("diagnostic configuration leaked %q: %s", sensitive, entries["configuration.json"])
		}
	}
}
