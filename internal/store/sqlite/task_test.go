package sqlite

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/z-chenhao/eri/internal/channel"
	"github.com/z-chenhao/eri/internal/content"
	assistanttask "github.com/z-chenhao/eri/internal/task"
)

func TestQueuedTaskCanBeInspectedAndCanceledWithoutRunning(t *testing.T) {
	root := t.TempDir()
	store, err := Open(filepath.Join(root, "eri.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	contentStore, err := content.New(filepath.Join(root, "content"), bytes.Repeat([]byte{0x41}, 32))
	if err != nil {
		t.Fatal(err)
	}
	conversation := channel.NewService(store, contentStore)
	sent, err := conversation.Send(context.Background(), "cli", "prepare a private cancellation target")
	if err != nil {
		t.Fatal(err)
	}
	tasks := assistanttask.NewService(store, contentStore)
	record, err := tasks.Inspect(context.Background(), sent.TaskID)
	if err != nil || record.Objective != "prepare a private cancellation target" || record.Status != "queued" {
		t.Fatalf("record=%+v err=%v", record, err)
	}
	result, err := tasks.Cancel(context.Background(), sent.TaskID)
	if err != nil || result.Status != "canceled" || result.Effect != "canceled_before_next_step" {
		t.Fatalf("cancel=%+v err=%v", result, err)
	}
	status, err := store.TaskStatus(context.Background(), sent.TaskID)
	if err != nil || status.Status != "canceled" {
		t.Fatalf("status=%+v err=%v", status, err)
	}
}

func TestFailedOrCanceledTaskRetriesFromStartOnlyWithoutDispatchedEffects(t *testing.T) {
	root := t.TempDir()
	store, err := Open(filepath.Join(root, "eri.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	contentStore, err := content.New(filepath.Join(root, "content"), bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	conversation := channel.NewService(store, contentStore)
	sent, err := conversation.Send(context.Background(), "cli", "retry this safe task")
	if err != nil {
		t.Fatal(err)
	}
	tasks := assistanttask.NewService(store, contentStore)
	if _, err := tasks.Cancel(context.Background(), sent.TaskID); err != nil {
		t.Fatal(err)
	}
	retry, err := tasks.Retry(context.Background(), sent.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if retry.SourceTaskID != sent.TaskID || retry.TaskID == sent.TaskID || retry.Status != "queued" || retry.Checkpoint != "task_start" {
		t.Fatalf("retry=%+v", retry)
	}
	retried, err := tasks.Inspect(context.Background(), retry.TaskID)
	if err != nil || retried.Objective != "retry this safe task" || retried.Status != "queued" {
		t.Fatalf("retried=%+v err=%v", retried, err)
	}
	events, err := store.ListEvents(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		found = found || (event.Type == "task.retry_requested" && event.AggregateID == sent.TaskID)
	}
	if !found {
		t.Fatal("retry audit event missing")
	}
}

func TestTaskRetryRejectsPreviouslyDispatchedOrUnknownEffects(t *testing.T) {
	ctx := context.Background()
	store, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := formatTime(time.Now().UTC())
	for _, statement := range []string{
		`INSERT INTO conversations(id, created_at) VALUES('conversation', '` + now + `')`,
		`INSERT INTO interactions(id, conversation_id, task_id, direction, role, kind, channel, content_ref_json, created_at)
		 VALUES('interaction', 'conversation', 'unsafe-task', 'inbound', 'user', 'message', 'cli', '{}', '` + now + `')`,
		`INSERT INTO tasks(id, conversation_id, source_interaction_id, source_channel, status, terminal_status, version, created_at, updated_at)
		 VALUES('unsafe-task', 'conversation', 'interaction', 'cli', 'failed', 'failed', 1, '` + now + `', '` + now + `')`,
		`INSERT INTO runs(id, task_id, status, model_status, soul_version, target, context_manifest_json, started_at, updated_at, ended_at)
		 VALUES('run', 'unsafe-task', 'failed', 'failed', 'soul', 'test:model', '{}', '` + now + `', '` + now + `', '` + now + `')`,
		`INSERT INTO effect_intents(id, task_id, run_id, invocation_id, tool_call_id, tool_id, tool_version, effect_class, target, parameters_hash, payload_ref_json, idempotency_key, control_level, reconciliation_strategy, status, created_at, updated_at)
		 VALUES('effect', 'unsafe-task', 'run', 'invocation', 'call', 'mcp.email.send', '1', 'communication', 'alice@example.com', 'hash', '{}', 'key', 'ordinary_confirm', 'provider_receipt', 'unknown', '` + now + `', '` + now + `')`,
	} {
		if _, err := store.db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.RetryTask(ctx, "unsafe-task"); err == nil {
		t.Fatal("unsafe task retry was accepted")
	}
	var tasks int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks`).Scan(&tasks); err != nil || tasks != 1 {
		t.Fatalf("tasks=%d err=%v", tasks, err)
	}
}
