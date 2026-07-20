package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/z-chenhao/eri/internal/policy"
	"github.com/z-chenhao/eri/internal/tool"
)

func TestFilesReadCreateAndGuardedPatch(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "notes.md"), []byte("hello world\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	files, err := NewFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	read, err := files.Prepare(context.Background(), json.RawMessage(`{"operation":"read","path":"notes.md"}`))
	if err != nil {
		t.Fatal(err)
	}
	if read.Action.Effect != policy.ReadOnly {
		t.Fatalf("read effect = %q", read.Action.Effect)
	}
	readResult, err := files.Execute(context.Background(), read)
	if err != nil || readResult.Receipt == "" {
		t.Fatalf("read result = %+v, err = %v", readResult, err)
	}
	created, err := files.Prepare(context.Background(), json.RawMessage(`{"operation":"create","path":"draft.md","content":"draft"}`))
	if err != nil {
		t.Fatal(err)
	}
	if created.Action.Effect != policy.Reversible || created.Action.OverwritesExisting {
		t.Fatalf("create action = %+v", created.Action)
	}
	if _, err := files.Execute(context.Background(), created); err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(filepath.Join(root, "notes.md"))
	if err != nil {
		t.Fatal(err)
	}
	patchInput := map[string]string{"operation": "patch", "path": "notes.md", "old": "world", "new": "Eri", "expected_sha256": digest(current)}
	encoded, _ := json.Marshal(patchInput)
	patch, err := files.Prepare(context.Background(), encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !patch.Action.OverwritesExisting {
		t.Fatal("patch did not declare overwrite risk")
	}
	if _, err := files.Execute(context.Background(), patch); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(root, "notes.md"))
	if string(got) != "hello Eri\n" {
		t.Fatalf("patched body = %q", got)
	}
}

func TestFilesReconcilesAmbiguousCreateByDesiredHash(t *testing.T) {
	root := t.TempDir()
	files, err := NewFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := files.Prepare(context.Background(), json.RawMessage(`{"operation":"create","path":"trip.md","content":"confirmed itinerary"}`))
	if err != nil {
		t.Fatal(err)
	}
	before, err := files.Reconcile(context.Background(), tool.ReconcileRequest{Payload: prepared.Input})
	if err != nil || before.Status != tool.IntentFailed {
		t.Fatalf("before=%+v err=%v", before, err)
	}
	if err := os.WriteFile(filepath.Join(root, "trip.md"), []byte("confirmed itinerary"), 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := files.Reconcile(context.Background(), tool.ReconcileRequest{Payload: prepared.Input})
	if err != nil || after.Status != tool.IntentConfirmed || after.Result.Receipt == "" {
		t.Fatalf("after=%+v err=%v", after, err)
	}
}

func TestFilesRejectTraversalAndSymlinkEscape(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outside := t.TempDir()
	files, err := NewFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := files.Prepare(context.Background(), json.RawMessage(`{"operation":"read","path":"../secret"}`)); err == nil {
		t.Fatal("parent traversal was accepted")
	}
	if runtime.GOOS == "windows" {
		return
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := files.Prepare(context.Background(), json.RawMessage(`{"operation":"list","path":"escape"}`)); err == nil {
		t.Fatal("symlink escape was accepted")
	}
}

func TestFilesExecutionCannotEscapeAfterPreparedDirectoryIsSwapped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory symlink race fixture is Unix-specific")
	}
	root := t.TempDir()
	outside := t.TempDir()
	inside := filepath.Join(root, "inside")
	if err := os.Mkdir(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inside, "note.txt"), []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "note.txt"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	files, err := NewFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	read, err := files.Prepare(context.Background(), json.RawMessage(`{"operation":"read","path":"inside/note.txt"}`))
	if err != nil {
		t.Fatal(err)
	}
	create, err := files.Prepare(context.Background(), json.RawMessage(`{"operation":"create","path":"inside/new.txt","content":"must stay inside"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(inside, filepath.Join(root, "moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, inside); err != nil {
		t.Fatal(err)
	}
	if _, err := files.Execute(context.Background(), read); err == nil {
		t.Fatal("prepared read followed a swapped symlink outside the workspace")
	}
	if _, err := files.Execute(context.Background(), create); err == nil {
		t.Fatal("prepared create followed a swapped symlink outside the workspace")
	}
	if _, err := os.Stat(filepath.Join(outside, "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("file appeared outside workspace: %v", err)
	}
}
