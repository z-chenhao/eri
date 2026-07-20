package builtin

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/z-chenhao/eri/internal/policy"
	"github.com/z-chenhao/eri/internal/tool"
)

const maxFileBytes = 1024 * 1024

type Files struct {
	root string
	mu   sync.Mutex
}

type fileInput struct {
	Operation    string `json:"operation"`
	Path         string `json:"path"`
	Query        string `json:"query,omitempty"`
	Content      string `json:"content,omitempty"`
	Old          string `json:"old,omitempty"`
	New          string `json:"new,omitempty"`
	ExpectedHash string `json:"expected_sha256,omitempty"`
	ResultHash   string `json:"result_sha256,omitempty"`
}

func NewFiles(root string) (*Files, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("file tool root is required")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	real, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, fmt.Errorf("resolve file tool root: %w", err)
	}
	info, err := os.Stat(real)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("file tool root is not a directory")
	}
	return &Files{root: filepath.Clean(real)}, nil
}

func (f *Files) Descriptor() tool.Descriptor {
	return tool.Descriptor{
		ID: "builtin.files", Version: "0.1.0",
		Purpose: "List, search, read, create, replace or exact-patch files inside the configured local workspace.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"operation":       map[string]any{"type": "string", "enum": []string{"list", "search", "read", "create", "write", "patch"}},
				"path":            map[string]any{"type": "string"},
				"query":           map[string]any{"type": "string"},
				"content":         map[string]any{"type": "string"},
				"old":             map[string]any{"type": "string"},
				"new":             map[string]any{"type": "string"},
				"expected_sha256": map[string]any{"type": "string"},
			},
			"required": []string{"operation", "path"},
		},
		OutputSchema:           map[string]any{"type": "object"},
		AllowedEffects:         []policy.EffectClass{policy.ReadOnly, policy.Reversible},
		PermissionRequirements: []string{"configured_workspace"},
		Timeout:                20 * time.Second, CostPolicy: "local_only",
		Idempotency: "gateway_key_and_precondition", Reconciliation: "verify_path_and_sha256",
		Source: tool.BuiltIn,
	}
}

func (f *Files) Prepare(_ context.Context, raw json.RawMessage) (tool.Prepared, error) {
	var input fileInput
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return tool.Prepared{}, err
	}
	input.Operation = strings.TrimSpace(input.Operation)
	rawPath := strings.TrimSpace(input.Path)
	if rawPath == "" {
		return tool.Prepared{}, fmt.Errorf("path is required")
	}
	input.Path = filepath.Clean(rawPath)
	if filepath.IsAbs(input.Path) || input.Path == ".." || strings.HasPrefix(input.Path, ".."+string(filepath.Separator)) {
		return tool.Prepared{}, fmt.Errorf("path must stay inside the configured workspace")
	}
	target := filepath.Join(f.root, input.Path)
	action := policy.Action{Target: target}
	switch input.Operation {
	case "list":
		action.Effect = policy.ReadOnly
		if _, err := f.resolveExisting(input.Path); err != nil {
			return tool.Prepared{}, err
		}
	case "search":
		action.Effect = policy.ReadOnly
		if input.Query == "" {
			return tool.Prepared{}, fmt.Errorf("query is required for search")
		}
		if _, err := f.resolveExisting(input.Path); err != nil {
			return tool.Prepared{}, err
		}
	case "read":
		action.Effect = policy.ReadOnly
		if _, err := f.resolveExisting(input.Path); err != nil {
			return tool.Prepared{}, err
		}
	case "create":
		action.Effect = policy.Reversible
		if _, err := f.resolveParent(input.Path); err != nil {
			return tool.Prepared{}, err
		}
		if _, err := os.Lstat(target); err == nil {
			return tool.Prepared{}, fmt.Errorf("target already exists")
		} else if !errors.Is(err, os.ErrNotExist) {
			return tool.Prepared{}, err
		}
		input.ResultHash = digest([]byte(input.Content))
	case "write":
		action.Effect = policy.Reversible
		action.OverwritesExisting = true
		if input.ExpectedHash == "" {
			return tool.Prepared{}, fmt.Errorf("expected_sha256 is required for write")
		}
		if _, err := f.resolveExisting(input.Path); err != nil {
			return tool.Prepared{}, err
		}
		input.ResultHash = digest([]byte(input.Content))
	case "patch":
		action.Effect = policy.Reversible
		action.OverwritesExisting = true
		if input.ExpectedHash == "" || input.Old == "" {
			return tool.Prepared{}, fmt.Errorf("old and expected_sha256 are required for patch")
		}
		resolved, err := f.resolveExisting(input.Path)
		if err != nil {
			return tool.Prepared{}, err
		}
		current, err := os.ReadFile(resolved)
		if err == nil && digest(current) == input.ExpectedHash && bytes.Count(current, []byte(input.Old)) == 1 {
			input.ResultHash = digest(bytes.Replace(current, []byte(input.Old), []byte(input.New), 1))
		}
	default:
		return tool.Prepared{}, fmt.Errorf("unsupported operation %q", input.Operation)
	}
	normalized, err := json.Marshal(input)
	if err != nil {
		return tool.Prepared{}, err
	}
	return tool.Prepared{Input: normalized, Action: action}, nil
}

// Reconcile inspects the workspace state using the precomputed desired hash.
// It never repeats a write after an ambiguous dispatch.
func (f *Files) Reconcile(ctx context.Context, request tool.ReconcileRequest) (tool.ReconcileResult, error) {
	var input fileInput
	if err := json.Unmarshal(request.Payload, &input); err != nil {
		return tool.ReconcileResult{}, err
	}
	switch input.Operation {
	case "list", "search", "read":
		prepared, err := f.Prepare(ctx, request.Payload)
		if err != nil {
			return tool.ReconcileResult{Status: tool.IntentUnknown, ErrorCode: "read_reconciliation_failed"}, nil
		}
		result, err := f.Execute(ctx, prepared)
		if err != nil {
			return tool.ReconcileResult{Status: tool.IntentUnknown, ErrorCode: "read_reconciliation_failed", Retry: true}, err
		}
		return tool.ReconcileResult{Status: tool.IntentConfirmed, Result: result}, nil
	case "create":
		return f.reconcileFileState(input.Path, input, true)
	case "write", "patch":
		_, _, err := f.readRootFile(input.Path)
		if errors.Is(err, os.ErrNotExist) {
			return tool.ReconcileResult{Status: tool.IntentUnknown, ErrorCode: "target_missing"}, nil
		}
		if err != nil {
			return tool.ReconcileResult{Status: tool.IntentUnknown, ErrorCode: "path_unavailable"}, nil
		}
		return f.reconcileFileState(input.Path, input, false)
	default:
		return tool.ReconcileResult{Status: tool.IntentUnknown, ErrorCode: "unsupported_file_operation"}, nil
	}
}

func (f *Files) reconcileFileState(path string, input fileInput, created bool) (tool.ReconcileResult, error) {
	body, _, err := f.readRootFile(path)
	if errors.Is(err, os.ErrNotExist) && created {
		return tool.ReconcileResult{Status: tool.IntentFailed, ErrorCode: "reconciled_not_executed"}, nil
	}
	if err != nil {
		return tool.ReconcileResult{Status: tool.IntentUnknown, ErrorCode: "target_unreadable"}, nil
	}
	actual := digest(body)
	if input.ResultHash != "" && actual == input.ResultHash {
		output, _ := json.Marshal(map[string]any{
			"operation": input.Operation, "path": filepath.ToSlash(input.Path), "sha256": actual, "reconciled": true,
		})
		return tool.ReconcileResult{Status: tool.IntentConfirmed, Result: tool.Result{
			Output: output, Receipt: "sha256:" + actual, FreshAt: time.Now().UTC(),
		}}, nil
	}
	if !created && input.ExpectedHash != "" && actual == input.ExpectedHash {
		return tool.ReconcileResult{Status: tool.IntentFailed, ErrorCode: "reconciled_not_executed"}, nil
	}
	return tool.ReconcileResult{Status: tool.IntentUnknown, ErrorCode: "file_state_ambiguous"}, nil
}

func (f *Files) Execute(ctx context.Context, prepared tool.Prepared) (tool.Result, error) {
	var input fileInput
	if err := json.Unmarshal(prepared.Input, &input); err != nil {
		return tool.Result{}, err
	}
	var output any
	var err error
	switch input.Operation {
	case "list":
		output, err = f.list(ctx, input.Path)
	case "search":
		output, err = f.search(ctx, input.Path, input.Query)
	case "read":
		output, err = f.read(input.Path)
	case "create":
		f.mu.Lock()
		defer f.mu.Unlock()
		output, err = f.create(input.Path, []byte(input.Content))
	case "write":
		f.mu.Lock()
		defer f.mu.Unlock()
		output, err = f.replace(input.Path, []byte(input.Content), input.ExpectedHash)
	case "patch":
		f.mu.Lock()
		defer f.mu.Unlock()
		output, err = f.patch(input)
	default:
		err = fmt.Errorf("unsupported operation %q", input.Operation)
	}
	if err != nil {
		return tool.Result{}, err
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		return tool.Result{}, err
	}
	return tool.Result{Output: encoded, Receipt: "sha256:" + digest(encoded), FreshAt: time.Now().UTC()}, nil
}

func (f *Files) list(ctx context.Context, relative string) (map[string]any, error) {
	root, err := os.OpenRoot(f.root)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	walkRoot := filepath.ToSlash(filepath.Clean(relative))
	entries := make([]string, 0)
	err = fs.WalkDir(root.FS(), walkRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == walkRoot {
			return nil
		}
		if len(entries) >= 500 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel := path
		if entry.IsDir() {
			rel += "/"
		}
		entries = append(entries, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(entries)
	return map[string]any{"operation": "list", "path": filepath.ToSlash(relative), "entries": entries, "truncated": len(entries) >= 500}, nil
}

func (f *Files) search(ctx context.Context, relative, query string) (map[string]any, error) {
	root, err := os.OpenRoot(f.root)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	walkRoot := filepath.ToSlash(filepath.Clean(relative))
	type match struct {
		Path string `json:"path"`
		Line int    `json:"line"`
		Text string `json:"text"`
	}
	matches := make([]match, 0)
	err = fs.WalkDir(root.FS(), walkRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || len(matches) >= 200 {
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxFileBytes {
			return nil
		}
		body, err := fs.ReadFile(root.FS(), path)
		if err != nil || bytes.IndexByte(body, 0) >= 0 {
			return nil
		}
		for index, line := range strings.Split(string(body), "\n") {
			if strings.Contains(line, query) {
				matches = append(matches, match{Path: path, Line: index + 1, Text: truncate(line, 500)})
				if len(matches) >= 200 {
					break
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"operation": "search", "path": filepath.ToSlash(relative), "query": query, "matches": matches, "truncated": len(matches) >= 200}, nil
}

func (f *Files) read(relative string) (map[string]any, error) {
	body, _, err := f.readRootFile(relative)
	if err != nil {
		return nil, err
	}
	if len(body) > maxFileBytes {
		return nil, fmt.Errorf("file exceeds %d bytes", maxFileBytes)
	}
	if bytes.IndexByte(body, 0) >= 0 {
		return nil, fmt.Errorf("binary files are not supported")
	}
	return map[string]any{"operation": "read", "path": filepath.ToSlash(relative), "content": string(body), "size_bytes": len(body), "sha256": digest(body)}, nil
}

func (f *Files) create(relative string, body []byte) (map[string]any, error) {
	if len(body) > maxFileBytes {
		return nil, fmt.Errorf("content exceeds %d bytes", maxFileBytes)
	}
	root, err := os.OpenRoot(f.root)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	file, err := root.OpenFile(relative, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if _, err := file.Write(body); err != nil {
		file.Close()
		return nil, err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	return map[string]any{"operation": "create", "path": filepath.ToSlash(relative), "size_bytes": len(body), "sha256": digest(body)}, nil
}

func (f *Files) replace(relative string, body []byte, expected string) (map[string]any, error) {
	if len(body) > maxFileBytes {
		return nil, fmt.Errorf("content exceeds %d bytes", maxFileBytes)
	}
	current, mode, err := f.readRootFile(relative)
	if err != nil {
		return nil, err
	}
	if digest(current) != expected {
		return nil, fmt.Errorf("file changed since it was observed")
	}
	root, err := os.OpenRoot(f.root)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if err := atomicRootReplace(root, relative, body, mode); err != nil {
		return nil, err
	}
	return map[string]any{"operation": "write", "path": filepath.ToSlash(relative), "previous_sha256": expected, "sha256": digest(body), "size_bytes": len(body)}, nil
}

func (f *Files) patch(input fileInput) (map[string]any, error) {
	current, mode, err := f.readRootFile(input.Path)
	if err != nil {
		return nil, err
	}
	if digest(current) != input.ExpectedHash {
		return nil, fmt.Errorf("file changed since it was observed")
	}
	if bytes.Count(current, []byte(input.Old)) != 1 {
		return nil, fmt.Errorf("patch old text must occur exactly once")
	}
	next := bytes.Replace(current, []byte(input.Old), []byte(input.New), 1)
	if len(next) > maxFileBytes {
		return nil, fmt.Errorf("patched content exceeds %d bytes", maxFileBytes)
	}
	root, err := os.OpenRoot(f.root)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if err := atomicRootReplace(root, input.Path, next, mode); err != nil {
		return nil, err
	}
	return map[string]any{"operation": "patch", "path": filepath.ToSlash(input.Path), "previous_sha256": input.ExpectedHash, "sha256": digest(next), "size_bytes": len(next)}, nil
}

func (f *Files) resolveExisting(relative string) (string, error) {
	candidate := filepath.Join(f.root, filepath.Clean(relative))
	real, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", err
	}
	if !within(f.root, real) {
		return "", fmt.Errorf("resolved path leaves the configured workspace")
	}
	return real, nil
}

func (f *Files) resolveParent(relative string) (string, error) {
	parent := filepath.Dir(filepath.Join(f.root, filepath.Clean(relative)))
	real, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", err
	}
	if !within(f.root, real) {
		return "", fmt.Errorf("resolved parent leaves the configured workspace")
	}
	return real, nil
}

func within(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func (f *Files) readRootFile(relative string) ([]byte, fs.FileMode, error) {
	root, err := os.OpenRoot(f.root)
	if err != nil {
		return nil, 0, err
	}
	defer root.Close()
	file, err := root.Open(relative)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, 0, err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("path is not a regular file")
	}
	body, err := io.ReadAll(io.LimitReader(file, maxFileBytes+1))
	if err != nil {
		return nil, 0, err
	}
	if len(body) > maxFileBytes {
		return nil, 0, fmt.Errorf("file exceeds %d bytes", maxFileBytes)
	}
	return body, info.Mode(), nil
}

func atomicRootReplace(root *os.Root, path string, body []byte, mode fs.FileMode) error {
	var temporary string
	var file *os.File
	for attempt := 0; attempt < 10; attempt++ {
		random := make([]byte, 8)
		if _, err := rand.Read(random); err != nil {
			return err
		}
		temporary = filepath.Join(filepath.Dir(path), ".eri-write-"+hex.EncodeToString(random))
		candidate, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode.Perm())
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return err
		}
		file = candidate
		break
	}
	if file == nil {
		return fmt.Errorf("allocate atomic workspace write")
	}
	defer root.Remove(temporary)
	if _, err := file.Write(body); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return root.Rename(temporary, path)
}

func digest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
