package codex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLocalRunnerConsumesCodexJSONLAndKeepsPromptOffArgv(t *testing.T) {
	root := t.TempDir()
	secretMarker := "private-objective-must-stay-on-stdin"
	executable := writeFakeCodex(t, root, `#!/bin/sh
for arg in "$@"; do
  if [ "$arg" = "private-objective-must-stay-on-stdin" ]; then
    exit 41
  fi
done
/bin/cat >/dev/null
printf '%s\n' '{"type":"thread.started","thread_id":"thread-1"}'
printf '%s\n' '{"type":"turn.started"}'
printf '%s\n' '{"type":"item.completed","item":{"id":"item-1","type":"agent_message","text":"{\"status\":\"completed\",\"summary\":\"inspection complete\",\"evidence\":[\"one fact\"],\"changes\":[],\"tests\":[],\"remaining_risks\":[]}"}}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":1,"cached_input_tokens":0,"output_tokens":1}}'
`)
	// The fake process is immediate, but the full repository suite runs many
	// subprocess and race-sensitive integration packages concurrently on CI.
	runner, err := NewLocalRunner(executable, filepath.Join(root, "data"), 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	result, err := runner.Run(context.Background(), RunRequest{
		ID: "job-1", Prompt: secretMarker, Mode: ReadOnly, Workspace: root,
	}, func(startedPID int) error {
		pid = startedPID
		return nil
	}, func(context.Context) (bool, error) { return false, nil })
	if err != nil {
		t.Fatal(err)
	}
	if pid <= 0 || result.Status != "completed" || result.Summary != "inspection complete" || len(result.Evidence) != 1 {
		t.Fatalf("pid=%d result=%+v", pid, result)
	}
	if _, err := os.Stat(filepath.Join(root, "data", "codex", "jobs", "job-1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("runtime job directory was not removed: %v", err)
	}
}

func TestDecodeEventStreamRejectsIncompleteOrUnstructuredResults(t *testing.T) {
	for name, stream := range map[string]string{
		"no terminal event": `{"type":"item.completed","item":{"type":"agent_message","text":"{}"}}`,
		"not structured":    "{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"ordinary prose\"}}\n{\"type\":\"turn.completed\"}\n",
	} {
		t.Run(name, func(t *testing.T) {
			if got := decodeEventStream(strings.NewReader(stream)); got.err == nil {
				t.Fatalf("stream unexpectedly accepted: %+v", got.result)
			}
		})
	}
}

func TestLocalRunnerRealCodex(t *testing.T) {
	if os.Getenv("ERI_E2E_CODEX") != "1" {
		t.Skip("set ERI_E2E_CODEX=1 to exercise the authenticated local Codex installation")
	}
	executable, found, err := DiscoverExecutable(os.Getenv("ERI_CODEX_PATH"))
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("local Codex executable was not found")
	}
	root := t.TempDir()
	runner, err := NewLocalRunner(executable, filepath.Join(root, "data"), 3*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(context.Background(), RunRequest{
		ID: "real-codex-smoke", Mode: ReadOnly, Workspace: root,
		Prompt: "Inspect this empty temporary workspace without changing it. Return a completed structured result whose summary says the workspace is empty.",
	}, func(int) error { return nil }, func(context.Context) (bool, error) { return false, nil })
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "completed" || strings.TrimSpace(result.Summary) == "" {
		t.Fatalf("real Codex result = %+v", result)
	}
}

func writeFakeCodex(t *testing.T, directory, body string) string {
	t.Helper()
	path := filepath.Join(directory, "fake-codex")
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
