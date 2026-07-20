package builtin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/z-chenhao/eri/internal/policy"
	"github.com/z-chenhao/eri/internal/tool"
)

const maxTerminalOutputBytes = 1024 * 1024

type Terminal struct {
	root string
}

type terminalInput struct {
	Executable     string   `json:"executable"`
	Arguments      []string `json:"arguments,omitempty"`
	WorkingDir     string   `json:"working_dir,omitempty"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
}

func NewTerminal(root string) (*Terminal, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	real, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, fmt.Errorf("resolve terminal workspace: %w", err)
	}
	return &Terminal{root: real}, nil
}

func (t *Terminal) Descriptor() tool.Descriptor {
	return tool.Descriptor{
		ID: "builtin.terminal", Version: "0.1.0",
		Purpose: "Run one bounded local process without a shell inside the configured workspace. Use executable plus argument array; output and exit status are capped and recorded. Arbitrary programs require strong approval.",
		InputSchema: map[string]any{
			"type": "object", "properties": map[string]any{
				"executable":      map[string]any{"type": "string"},
				"arguments":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"working_dir":     map[string]any{"type": "string"},
				"timeout_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 120},
			}, "required": []string{"executable"},
		},
		OutputSchema:           map[string]any{"type": "object"},
		AllowedEffects:         []policy.EffectClass{policy.ReadOnly, policy.Privileged},
		PermissionRequirements: []string{"configured_workspace", "sanitized_process_environment"}, Timeout: 125 * time.Second,
		CostPolicy: "local_compute", Idempotency: "gateway_key", Reconciliation: "exit_status_and_process_timeout",
		Source: tool.BuiltIn,
	}
}

func (t *Terminal) Prepare(_ context.Context, raw json.RawMessage) (tool.Prepared, error) {
	var input terminalInput
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return tool.Prepared{}, err
	}
	input.Executable = strings.TrimSpace(input.Executable)
	if input.Executable == "" || strings.ContainsAny(input.Executable, "\x00\r\n") {
		return tool.Prepared{}, fmt.Errorf("executable is required")
	}
	if input.TimeoutSeconds == 0 {
		input.TimeoutSeconds = 30
	}
	if input.TimeoutSeconds < 1 || input.TimeoutSeconds > 120 {
		return tool.Prepared{}, fmt.Errorf("timeout_seconds must be between 1 and 120")
	}
	workingDir, relative, err := t.resolveWorkingDir(input.WorkingDir)
	if err != nil {
		return tool.Prepared{}, err
	}
	input.WorkingDir = relative
	for _, argument := range input.Arguments {
		if strings.ContainsRune(argument, 0) {
			return tool.Prepared{}, fmt.Errorf("arguments cannot contain NUL")
		}
	}
	effect := policy.Privileged
	if safeReadCommand(input.Executable, input.Arguments) {
		effect = policy.ReadOnly
	}
	normalized, err := json.Marshal(input)
	if err != nil {
		return tool.Prepared{}, err
	}
	return tool.Prepared{Input: normalized, Action: policy.Action{
		Effect: effect, Target: input.Executable + " in " + workingDir,
	}}, nil
}

func (t *Terminal) Execute(ctx context.Context, prepared tool.Prepared) (tool.Result, error) {
	var input terminalInput
	if err := json.Unmarshal(prepared.Input, &input); err != nil {
		return tool.Result{}, err
	}
	workingDir, _, err := t.resolveWorkingDir(input.WorkingDir)
	if err != nil {
		return tool.Result{}, err
	}
	commandContext, cancel := context.WithTimeout(ctx, time.Duration(input.TimeoutSeconds)*time.Second)
	defer cancel()
	command := exec.CommandContext(commandContext, input.Executable, input.Arguments...)
	command.Dir = workingDir
	command.Env = sanitizedProcessEnvironment()
	stdout := &boundedBuffer{limit: maxTerminalOutputBytes / 2}
	stderr := &boundedBuffer{limit: maxTerminalOutputBytes / 2}
	command.Stdout = stdout
	command.Stderr = stderr
	started := time.Now()
	runErr := command.Run()
	exitCode := 0
	if runErr != nil {
		if exit, ok := runErr.(*exec.ExitError); ok {
			exitCode = exit.ExitCode()
		} else if commandContext.Err() != nil {
			exitCode = -1
		} else {
			return tool.Result{}, runErr
		}
	}
	output := map[string]any{
		"executable": input.Executable, "arguments": input.Arguments, "working_dir": input.WorkingDir,
		"exit_code": exitCode, "stdout": stdout.String(), "stderr": stderr.String(),
		"truncated": stdout.truncated || stderr.truncated, "timed_out": commandContext.Err() == context.DeadlineExceeded,
		"duration_ms": time.Since(started).Milliseconds(),
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		return tool.Result{}, err
	}
	digest := sha256.Sum256(encoded)
	return tool.Result{Output: encoded, Receipt: "sha256:" + hex.EncodeToString(digest[:]), FreshAt: time.Now().UTC()}, nil
}

func (t *Terminal) resolveWorkingDir(relative string) (string, string, error) {
	if relative == "" {
		relative = "."
	}
	clean := filepath.Clean(relative)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("working_dir must stay inside the configured workspace")
	}
	candidate, err := filepath.EvalSymlinks(filepath.Join(t.root, clean))
	if err != nil {
		return "", "", fmt.Errorf("resolve working_dir: %w", err)
	}
	rel, err := filepath.Rel(t.root, candidate)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("working_dir escapes the configured workspace")
	}
	info, err := os.Stat(candidate)
	if err != nil || !info.IsDir() {
		return "", "", fmt.Errorf("working_dir is not a directory")
	}
	return candidate, filepath.ToSlash(rel), nil
}

func safeReadCommand(executable string, arguments []string) bool {
	// Paths are never treated as intrinsically safe: ./pwd and /tmp/ls can be
	// arbitrary workspace executables whose basename merely resembles a
	// system inspection command.
	if filepath.Base(executable) != executable {
		return false
	}
	switch executable {
	case "pwd":
		return len(arguments) == 0
	case "ls":
		for _, argument := range arguments {
			if !safeLSFlag(argument) {
				return false
			}
		}
		return true
	}
	return false
}

func safeLSFlag(argument string) bool {
	if !strings.HasPrefix(argument, "-") || argument == "-" || strings.HasPrefix(argument, "--") {
		return false
	}
	for _, flag := range strings.TrimPrefix(argument, "-") {
		if !strings.ContainsRune("1AaCcdFfghilmnpqRrSstux", flag) {
			return false
		}
	}
	return true
}

func sanitizedProcessEnvironment() []string {
	environment := []string{"NO_COLOR=1"}
	for _, name := range []string{"PATH", "LANG", "LC_ALL", "TMPDIR", "TERM"} {
		if value := os.Getenv(name); value != "" {
			environment = append(environment, name+"="+value)
		}
	}
	return environment
}

type boundedBuffer struct {
	mu        sync.Mutex
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func (b *boundedBuffer) Write(payload []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	original := len(payload)
	remaining := b.limit - b.buffer.Len()
	if remaining <= 0 {
		b.truncated = true
		return original, nil
	}
	if len(payload) > remaining {
		payload = payload[:remaining]
		b.truncated = true
	}
	_, _ = b.buffer.Write(payload)
	return original, nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}
