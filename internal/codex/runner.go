package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// DiscoverExecutable resolves an explicit path or the current user's local
// Codex installation without reading or copying Codex authentication.
func DiscoverExecutable(configured string) (string, bool, error) {
	configured = strings.TrimSpace(configured)
	if configured != "" {
		resolved, err := validateExecutable(configured)
		if err != nil {
			return "", false, err
		}
		if err := probeExecutable(resolved); err != nil {
			return "", false, err
		}
		return resolved, true, nil
	}
	candidates := make([]string, 0, 2)
	if path, err := exec.LookPath("codex"); err == nil {
		candidates = append(candidates, path)
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".codex", "plugins", ".plugin-appserver", "codex"))
	}
	seen := map[string]struct{}{}
	for _, candidate := range candidates {
		resolved, err := validateExecutable(candidate)
		if err != nil {
			continue
		}
		if _, duplicate := seen[resolved]; duplicate {
			continue
		}
		seen[resolved] = struct{}{}
		if probeExecutable(resolved) == nil {
			return resolved, true, nil
		}
	}
	return "", false, nil
}

type LocalRunner struct {
	executable string
	dataRoot   string
	timeout    time.Duration
	poll       time.Duration
}

func NewLocalRunner(executable, dataRoot string, timeout time.Duration) (*LocalRunner, error) {
	resolved, err := validateExecutable(executable)
	if err != nil {
		return nil, err
	}
	if dataRoot == "" {
		return nil, fmt.Errorf("Codex data root is required")
	}
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	return &LocalRunner{executable: resolved, dataRoot: dataRoot, timeout: timeout, poll: 250 * time.Millisecond}, nil
}

func (r *LocalRunner) Run(ctx context.Context, request RunRequest, onStarted func(int) error, canceled func(context.Context) (bool, error)) (Result, error) {
	directory, schemaPath, err := r.prepare(request.ID)
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(directory)
	sandbox := "read-only"
	if request.Mode == WorkspaceWrite {
		sandbox = "workspace-write"
	}
	stdinReader, stdinWriter, err := os.Pipe()
	if err != nil {
		return Result{}, err
	}
	command := exec.Command(r.executable,
		"--ask-for-approval", "never", "exec", "--ephemeral", "--ignore-user-config", "--skip-git-repo-check", "--json",
		"--sandbox", sandbox, "--color", "never",
		"--output-schema", schemaPath, "--cd", request.Workspace, "-",
	)
	command.Stdin = stdinReader
	stdout, err := command.StdoutPipe()
	if err != nil {
		stdinReader.Close()
		stdinWriter.Close()
		return Result{}, err
	}
	command.Stderr = io.Discard
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		stdinReader.Close()
		stdinWriter.Close()
		return Result{}, err
	}
	stdinReader.Close()
	if _, err := io.WriteString(stdinWriter, request.Prompt); err != nil {
		stdinWriter.Close()
		killProcessGroup(command.Process.Pid)
		_ = command.Wait()
		return Result{}, err
	}
	if err := stdinWriter.Close(); err != nil {
		killProcessGroup(command.Process.Pid)
		_ = command.Wait()
		return Result{}, err
	}
	if err := onStarted(command.Process.Pid); err != nil {
		killProcessGroup(command.Process.Pid)
		_ = command.Wait()
		return Result{}, err
	}
	resultLimit := request.ResultLimit
	if resultLimit <= 0 {
		resultLimit = 1024 * 1024
	}
	output := make(chan streamResult, 1)
	go func() { output <- decodeEventStream(io.LimitReader(stdout, resultLimit+1)) }()
	wait := make(chan error, 1)
	go func() { wait <- command.Wait() }()
	deadline := time.NewTimer(r.timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(r.poll)
	defer ticker.Stop()
	var processErr error
	var stream streamResult
	processDone := false
	streamDone := false
	for {
		if processDone && streamDone {
			if processErr != nil {
				return Result{}, processErr
			}
			if stream.err != nil {
				return Result{}, stream.err
			}
			return stream.result, nil
		}
		select {
		case processErr = <-wait:
			processDone = true
		case stream = <-output:
			streamDone = true
		case <-ctx.Done():
			killProcessGroup(command.Process.Pid)
			if !processDone {
				<-wait
			}
			return Result{}, ctx.Err()
		case <-deadline.C:
			killProcessGroup(command.Process.Pid)
			if !processDone {
				<-wait
			}
			return Result{}, ErrTimeout
		case <-ticker.C:
			stop, err := canceled(context.Background())
			if err == nil && stop {
				killProcessGroup(command.Process.Pid)
				if !processDone {
					<-wait
				}
				return Result{}, ErrCanceled
			}
		}
	}
}

func (r *LocalRunner) Recover(_ context.Context, request RunRequest, _ func(context.Context) (bool, error)) (Result, error) {
	// The JSONL stream is deliberately never persisted because it can contain
	// private reasoning and raw tool detail. After a daemon crash there is no
	// trustworthy result channel to reattach, so stop any orphan and surface an
	// Unknown outcome instead of replaying potentially mutating work.
	if processAlive(request.PID) {
		killProcessGroup(request.PID)
	}
	return Result{}, ErrUnknown
}

func (r *LocalRunner) EraseArtifacts(context.Context) error {
	return os.RemoveAll(filepath.Join(r.dataRoot, "codex"))
}

type streamResult struct {
	result Result
	err    error
}

func decodeEventStream(reader io.Reader) streamResult {
	decoder := json.NewDecoder(reader)
	var final string
	completed := false
	failed := false
	for {
		var event struct {
			Type string `json:"type"`
			Item struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
		}
		if err := decoder.Decode(&event); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return streamResult{err: fmt.Errorf("decode Codex JSONL event: %w", err)}
		}
		switch event.Type {
		case "item.completed":
			if event.Item.Type == "agent_message" && strings.TrimSpace(event.Item.Text) != "" {
				final = event.Item.Text
			}
		case "turn.completed":
			completed = true
		case "turn.failed", "error":
			failed = true
		}
	}
	if failed || !completed || final == "" {
		return streamResult{err: fmt.Errorf("Codex stream ended without a completed final message")}
	}
	var result Result
	decoder = json.NewDecoder(strings.NewReader(final))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return streamResult{err: fmt.Errorf("decode Codex structured result: %w", err)}
	}
	if result.Status != "completed" && result.Status != "blocked" || strings.TrimSpace(result.Summary) == "" {
		return streamResult{err: fmt.Errorf("Codex result does not satisfy its terminal contract")}
	}
	return streamResult{result: result}
}

func (r *LocalRunner) prepare(id string) (string, string, error) {
	directory := filepath.Join(r.dataRoot, "codex", "jobs", id)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", "", err
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return "", "", err
	}
	schema, err := json.Marshal(resultSchema())
	if err != nil {
		return "", "", err
	}
	schemaPath := filepath.Join(directory, "result.schema.json")
	if err := os.WriteFile(schemaPath, schema, 0o600); err != nil {
		return "", "", err
	}
	return directory, schemaPath, nil
}

func resultSchema() map[string]any {
	stringArray := map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"status":   map[string]any{"type": "string", "enum": []string{"completed", "blocked"}},
			"summary":  map[string]any{"type": "string"},
			"evidence": stringArray, "changes": stringArray, "tests": stringArray, "remaining_risks": stringArray,
		},
		"required":             []string{"status", "summary", "evidence", "changes", "tests", "remaining_risks"},
		"additionalProperties": false,
	}
}

func validateExecutable(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("Codex executable is required")
	}
	resolved, err := exec.LookPath(path)
	if err != nil {
		return "", fmt.Errorf("find Codex executable: %w", err)
	}
	resolved, err = filepath.EvalSymlinks(resolved)
	if err != nil {
		return "", fmt.Errorf("resolve Codex executable: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("Codex executable must be an executable regular file")
	}
	return resolved, nil
}

func probeExecutable(path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, path, "--version")
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return fmt.Errorf("probe Codex executable: %w", err)
	}
	return nil
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func killProcessGroup(pid int) {
	if pid <= 0 {
		return
	}
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}
