package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSafeErrorRedactsCredentialsAndHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	value := SafeError(errors.New(`request failed
Authorization: Bearer abcdefghijklmnopqrstuvwxyz token=super-secret-value password":"json-secret db=postgres://eri:url-secret@localhost/private path=` + filepath.Join(home, "private")))
	for _, forbidden := range []string{"abcdefghijklmnopqrstuvwxyz", "super-secret-value", "json-secret", "url-secret", home, "\n"} {
		if strings.Contains(value, forbidden) {
			t.Fatalf("unsafe log value %q contains %q", value, forbidden)
		}
	}
	if !strings.Contains(value, "[REDACTED]") || !strings.Contains(value, "$HOME") {
		t.Fatalf("safe log value = %q", value)
	}
}

func TestProcessLoggerFansOutOneRedactedRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	var terminal bytes.Buffer
	logger, closer, err := NewProcessLogger(path, &terminal)
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("provider request finished", "task_id", "task-1", "error", "token=private-value")
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for name, output := range map[string]string{"file": string(body), "terminal": terminal.String()} {
		if !strings.Contains(output, "provider request finished") || !strings.Contains(output, "task-1") {
			t.Fatalf("%s sink missing record: %s", name, output)
		}
		if strings.Contains(output, "private-value") || !strings.Contains(output, "[REDACTED]") {
			t.Fatalf("%s sink did not redact: %s", name, output)
		}
	}
	if !strings.Contains(string(body), `"level":"INFO"`) || !strings.Contains(terminal.String(), "level=INFO") {
		t.Fatalf("unexpected sink formats: file=%s terminal=%s", body, terminal.String())
	}
}

func TestProcessLoggerPreservesGroupedAttributes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	logger, closer, err := NewProcessLogger(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	logger.With(slog.Group("run", "id", "run-1")).Info("started")
	_ = closer.Close()
	body, _ := os.ReadFile(path)
	if !strings.Contains(string(body), `"run":{"id":"run-1"}`) {
		t.Fatalf("grouped attributes were lost: %s", body)
	}
}

func TestProcessLoggerSanitizesArbitraryAttributeValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	logger, closer, err := NewProcessLogger(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("request", "details", map[string]string{"token": "private-map-value", "operation": "complete"})
	_ = closer.Close()
	body, _ := os.ReadFile(path)
	if strings.Contains(string(body), "private-map-value") || !strings.Contains(string(body), "[REDACTED]") {
		t.Fatalf("arbitrary attribute was not sanitized: %s", body)
	}
}

func TestRawModelDebugBodyIsTheOnlyUnredactedProcessLogValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	logger, closer, err := NewProcessLogger(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw := `{"messages":[{"content":"token=private-raw-value ` + strings.Repeat("x", 1500) + `"}]}`
	logger.InfoContext(context.Background(), "raw model provider request",
		"component", "model", "provider", "deepseek", "body", raw)
	logger.Info("ordinary", "body", "token=ordinary-private-value")
	_ = closer.Close()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	logged := string(body)
	var record struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal([]byte(strings.Split(logged, "\n")[0]), &record); err != nil {
		t.Fatal(err)
	}
	if record.Body != raw || strings.Contains(logged, "ordinary-private-value") || !strings.Contains(logged, "[REDACTED]") {
		t.Fatalf("raw debug bypass or ordinary redaction failed: %s", logged)
	}
}

func TestRotatingFileBoundsAndRetainsBackups(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "eri.log")
	if err := os.Chmod(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	writer, err := NewRotatingFile(path, 12, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"first-line\n", "second-line\n", "third-line\n"} {
		if _, err := writer.Write([]byte(value)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{path, path + ".1", path + ".2"} {
		if info, err := os.Stat(candidate); err != nil || info.Size() > 12 {
			t.Fatalf("rotated file %s info=%v err=%v", candidate, info, err)
		} else if info.Mode().Perm() != 0o600 {
			t.Fatalf("rotated file %s mode=%v", candidate, info.Mode().Perm())
		}
	}
	info, err := os.Stat(directory)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("log directory mode=%v", info.Mode().Perm())
	}
}
