package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/z-chenhao/eri/internal/agent"
	"github.com/z-chenhao/eri/internal/channel"
	"github.com/z-chenhao/eri/internal/content"
	"github.com/z-chenhao/eri/internal/delivery"
	"github.com/z-chenhao/eri/internal/episode"
	"github.com/z-chenhao/eri/internal/eval"
	"github.com/z-chenhao/eri/internal/memory"
	"github.com/z-chenhao/eri/internal/policy"
	"github.com/z-chenhao/eri/internal/tool"
	"github.com/z-chenhao/eri/internal/userdata"
)

func TestUniqueExternalSenderRecoversOnlyAnUnambiguousExistingBinding(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "eri.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if sender, err := store.UniqueExternalSender(ctx, "lark"); err != nil || sender != "" {
		t.Fatalf("empty binding sender=%q err=%v", sender, err)
	}
	_, _, err = store.CreateExternalInbound(ctx, "lark", channel.ExternalInteraction{
		MessageID: "m1", ConversationID: "c1", SenderID: "ou_owner", CreatedAt: time.Now(),
	}, testRef("lark-1", "lark-hash-1"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if sender, err := store.UniqueExternalSender(ctx, "lark"); err != nil || sender != "ou_owner" {
		t.Fatalf("recovered sender=%q err=%v", sender, err)
	}
	_, _, err = store.CreateExternalInbound(ctx, "lark", channel.ExternalInteraction{
		MessageID: "m2", ConversationID: "c2", SenderID: "ou_other", CreatedAt: time.Now(),
	}, testRef("lark-2", "lark-hash-2"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if sender, err := store.UniqueExternalSender(ctx, "lark"); err == nil || sender != "" {
		t.Fatalf("ambiguous binding sender=%q err=%v", sender, err)
	}
}

func TestPersistedTimestampsPreserveChronologicalTextOrder(t *testing.T) {
	earlier := time.Date(2026, time.July, 20, 12, 0, 0, 123400000, time.UTC)
	later := time.Date(2026, time.July, 20, 12, 0, 0, 123450000, time.UTC)

	earlierText := formatTime(earlier)
	laterText := formatTime(later)
	if earlierText >= laterText {
		t.Fatalf("persisted timestamp order = %q >= %q", earlierText, laterText)
	}
	parsed, err := parseTime(laterText)
	if err != nil || !parsed.Equal(later) {
		t.Fatalf("parsed timestamp = %s, err = %v", parsed, err)
	}
}

func TestInboundJoinsDispatchedAgentLoopAndFencesStaleEffects(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "eri.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	first, err := store.CreateInbound(ctx, "web", testRef("first-input", "first-input-hash"), nil)
	if err != nil {
		t.Fatal(err)
	}
	task, claimed, err := store.ClaimTask(ctx, first.TaskID, "worker", time.Minute, "soul", `{}`, "test:model")
	if err != nil || !claimed {
		t.Fatalf("task=%+v claimed=%t err=%v", task, claimed, err)
	}
	if err := store.MarkRunDispatched(ctx, task.RunID); err != nil {
		t.Fatal(err)
	}
	second, err := store.CreateInbound(ctx, "web", testRef("second-input", "second-input-hash"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if second.TaskID != first.TaskID {
		t.Fatalf("second task = %q, want active task %q", second.TaskID, first.TaskID)
	}
	var taskCount, wakeCount int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM tasks`).Scan(&taskCount); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM internal_outbox WHERE kind = 'task.wake'`).Scan(&wakeCount); err != nil {
		t.Fatal(err)
	}
	if taskCount != 1 || wakeCount != 1 {
		t.Fatalf("tasks=%d wakes=%d, want one active Loop and its original wake", taskCount, wakeCount)
	}
	inputs, err := store.LoadTaskInputsAfter(ctx, first.TaskID, task.InputSequence)
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 1 || inputs[0].ID != second.InteractionID || inputs[0].Sequence <= task.InputSequence {
		t.Fatalf("joined inputs = %+v", inputs)
	}
	staleCommit := agent.Commit{
		TaskID: first.TaskID, RunID: task.RunID,
		ArtifactID: "stale-artifact", EvalID: "stale-eval", DeliveryID: "stale-delivery",
		ArtifactKind: "text", ArtifactRef: testRef("stale-artifact-ref", "stale-artifact-hash"),
		TraceRef: testRef("stale-trace", "stale-trace-hash"), EvalFindingsRef: testRef("stale-findings", "stale-findings-hash"),
		EvalResult: eval.Pass, TerminalStatus: "completed", BasisInputSequence: task.InputSequence,
	}
	if err := store.CommitArtifact(ctx, staleCommit); !errors.Is(err, agent.ErrStaleTaskInput) {
		t.Fatalf("stale artifact commit error = %v", err)
	}
	staleProgress := staleCommit
	staleProgress.ArtifactID = "stale-progress"
	staleProgress.EvalID = "stale-progress-eval"
	staleProgress.DeliveryID = "stale-progress-delivery"
	staleProgress.ArtifactKind = "progress"
	if _, err := store.CommitProgress(ctx, agent.ProgressCommit{Commit: staleProgress, ModelTurnID: "turn-1"}); !errors.Is(err, agent.ErrStaleTaskInput) {
		t.Fatalf("stale progress commit error = %v", err)
	}
	staleIntent := tool.Intent{
		ID: "stale-intent", TaskID: first.TaskID, RunID: task.RunID, InvocationID: task.RunID,
		ToolCallID: "call-1", BasisInputSequence: task.InputSequence, ToolID: "builtin.test", ToolVersion: "1",
		Effect: policy.ReadOnly, Target: "local", ParametersHash: "hash", IdempotencyKey: "stale-intent-key",
		Status: tool.IntentPlanned, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if _, _, err := store.PlanIntent(ctx, staleIntent); !errors.Is(err, tool.ErrStaleTaskInput) {
		t.Fatalf("stale tool intent error = %v", err)
	}
}

func TestResumedOlderTaskCannotCaptureNewConversationBranch(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "eri.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	first, err := store.CreateInbound(ctx, "web", testRef("older-input", "older-input-hash"), nil)
	if err != nil {
		t.Fatal(err)
	}
	older, claimed, err := store.ClaimTask(ctx, first.TaskID, "worker", time.Minute, "soul", `{}`, "test:model")
	if err != nil || !claimed {
		t.Fatalf("older task=%+v claimed=%t err=%v", older, claimed, err)
	}
	if err := store.MarkRunDispatched(ctx, older.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE tasks SET status = 'waiting' WHERE id = ?`, first.TaskID); err != nil {
		t.Fatal(err)
	}
	newer, err := store.CreateInbound(ctx, "web", testRef("newer-input", "newer-input-hash"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if newer.TaskID == first.TaskID {
		t.Fatal("newer branch joined a waiting task")
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE tasks SET status = 'running' WHERE id = ?`, first.TaskID); err != nil {
		t.Fatal(err)
	}
	latest, err := store.CreateInbound(ctx, "web", testRef("latest-input", "latest-input-hash"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if latest.TaskID == first.TaskID {
		t.Fatalf("latest input joined resumed older task %q", first.TaskID)
	}
	if latest.TaskID == newer.TaskID {
		t.Fatalf("latest input joined non-dispatched newer task %q", newer.TaskID)
	}
	stale := agent.Commit{
		TaskID: first.TaskID, RunID: older.RunID,
		ArtifactID: "older-artifact", EvalID: "older-eval", DeliveryID: "older-delivery",
		ArtifactKind: "text", ArtifactRef: testRef("older-artifact-ref", "older-artifact-hash"),
		TraceRef: testRef("older-trace", "older-trace-hash"), EvalFindingsRef: testRef("older-findings", "older-findings-hash"),
		EvalResult: eval.Pass, TerminalStatus: "completed", BasisInputSequence: older.InputSequence,
		BasisConversationSequence: older.ConversationSequence,
	}
	if err := store.CommitArtifact(ctx, stale); !errors.Is(err, agent.ErrStaleConversationContext) {
		t.Fatalf("older Conversation artifact commit error=%v", err)
	}
	staleIntent := tool.Intent{
		ID: "older-intent", TaskID: first.TaskID, RunID: older.RunID, InvocationID: older.RunID,
		ToolCallID: "call-older", BasisInputSequence: older.InputSequence,
		BasisConversationSequence: older.ConversationSequence, ToolID: "builtin.test", ToolVersion: "1",
		Effect: policy.ReadOnly, Target: "local", ParametersHash: "hash", IdempotencyKey: "older-intent-key",
		Status: tool.IntentPlanned, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if _, _, err := store.PlanIntent(ctx, staleIntent); !errors.Is(err, tool.ErrStaleConversationContext) {
		t.Fatalf("older Conversation tool intent error=%v", err)
	}
}

func TestExplicitReplyTargetsRunningTaskBeforeConversationFrontier(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "eri.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	first, _, err := store.CreateExternalInbound(ctx, "lark", channel.ExternalInteraction{
		MessageID: "message-old", ConversationID: "owner-chat", SenderID: "owner", CreatedAt: time.Now(),
	}, testRef("reply-old", "reply-old-hash"), nil)
	if err != nil {
		t.Fatal(err)
	}
	older, claimed, err := store.ClaimTask(ctx, first.TaskID, "worker", time.Minute, "soul", `{}`, "test:model")
	if err != nil || !claimed {
		t.Fatalf("older task=%+v claimed=%t err=%v", older, claimed, err)
	}
	if err := store.MarkRunDispatched(ctx, older.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE tasks SET status = 'waiting' WHERE id = ?`, first.TaskID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateExternalInbound(ctx, "lark", channel.ExternalInteraction{
		MessageID: "message-newer", ConversationID: "owner-chat", SenderID: "owner", CreatedAt: time.Now(),
	}, testRef("reply-newer", "reply-newer-hash"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE tasks SET status = 'running' WHERE id = ?`, first.TaskID); err != nil {
		t.Fatal(err)
	}
	reply, _, err := store.CreateExternalInbound(ctx, "lark", channel.ExternalInteraction{
		MessageID: "message-reply", ConversationID: "owner-chat", SenderID: "owner",
		ReplyToMessageID: "message-old", CreatedAt: time.Now(),
	}, testRef("reply-target", "reply-target-hash"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if reply.TaskID != first.TaskID {
		t.Fatalf("explicit reply task=%q, want %q", reply.TaskID, first.TaskID)
	}
}

func TestPlanIntentReplaysDurableEffectAfterNewerInputButRejectsNewStaleEffect(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "eri.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	first, err := store.CreateInbound(ctx, "web", testRef("first-input", "first-input-hash"), nil)
	if err != nil {
		t.Fatal(err)
	}
	task, claimed, err := store.ClaimTask(ctx, first.TaskID, "worker", time.Minute, "soul", `{}`, "test:model")
	if err != nil || !claimed {
		t.Fatalf("task=%+v claimed=%t err=%v", task, claimed, err)
	}
	if err := store.MarkRunDispatched(ctx, task.RunID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	original := tool.Intent{
		ID: "intent-original", TaskID: task.TaskID, RunID: task.RunID, InvocationID: task.RunID,
		ToolCallID: "call-1", BasisInputSequence: task.InputSequence, ToolID: "builtin.test", ToolVersion: "1",
		Effect: policy.ReadOnly, Target: "local", ParametersHash: "same-hash", IdempotencyKey: "same-key",
		Control: policy.Auto, Status: tool.IntentPlanned, CreatedAt: now, UpdatedAt: now,
	}
	persisted, created, err := store.PlanIntent(ctx, original)
	if err != nil || !created {
		t.Fatalf("initial intent=%+v created=%t err=%v", persisted, created, err)
	}
	if _, err := store.CreateInbound(ctx, "web", testRef("second-input", "second-input-hash"), nil); err != nil {
		t.Fatal(err)
	}
	replay := original
	replay.ID = "intent-replay"
	replayed, created, err := store.PlanIntent(ctx, replay)
	if err != nil || created || replayed.ID != original.ID {
		t.Fatalf("replay=%+v created=%t err=%v", replayed, created, err)
	}
	staleNew := original
	staleNew.ID = "intent-new"
	staleNew.ToolCallID = "call-2"
	staleNew.ParametersHash = "new-hash"
	staleNew.IdempotencyKey = "new-key"
	if _, _, err := store.PlanIntent(ctx, staleNew); !errors.Is(err, tool.ErrStaleTaskInput) {
		t.Fatalf("new stale intent error = %v", err)
	}
}

func TestOpenInitializesOnlyTheAuthoritativeSchema(t *testing.T) {
	store, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var version int
	if err := store.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("schema version=%d err=%v", version, err)
	}

	for table, expectation := range map[string]struct {
		required  []string
		forbidden []string
	}{
		"artifacts":                  {required: []string{"trace_ref_json"}, forbidden: []string{"decision_ref_json"}},
		"eval_records":               {required: []string{"findings_ref_json", "finding_count"}, forbidden: []string{"findings_json"}},
		"effect_intents":             {required: []string{"payload_ref_json", "parent_intent_id", "invocation_id", "tool_call_id"}},
		"conversation_introductions": {required: []string{"conversation_id", "task_id", "requested_at"}},
		"commitments":                {required: []string{"source_task_id", "task_ref_json"}},
		"memory_items":               {required: []string{"replaces_memory_id"}},
		"memory_semantic_index":      {required: []string{"memory_id", "model_id", "content_hash", "vector_ref_json"}},
	} {
		rows, err := store.db.Query(`PRAGMA table_info(` + table + `)`)
		if err != nil {
			t.Fatal(err)
		}
		columns := map[string]bool{}
		for rows.Next() {
			var position, notNull, primaryKey int
			var name, dataType string
			var defaultValue any
			if err := rows.Scan(&position, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			columns[name] = true
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		for _, column := range expectation.required {
			if !columns[column] {
				t.Errorf("%s is missing authoritative column %s", table, column)
			}
		}
		for _, column := range expectation.forbidden {
			if columns[column] {
				t.Errorf("%s still contains pre-release compatibility column %s", table, column)
			}
		}
	}
}

func TestOpenRejectsUnversionedPreReleaseSchemaInsteadOfMigratingIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pre-release.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE old_development_state(id TEXT PRIMARY KEY)`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "reset the Eri data directory") {
		t.Fatalf("Open error=%v", err)
	}
}

func TestOpenRejectsStaleVersionOneShapeInsteadOfMigratingIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stale-version-one.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE old_development_state(id TEXT PRIMARY KEY); PRAGMA user_version = 1`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "stale pre-release sqlite schema") || !strings.Contains(err.Error(), "reset the Eri data directory") {
		t.Fatalf("Open error=%v", err)
	}
}

func TestOpenRepairsSQLiteAndMetadataPermissions(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "metadata")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "eri.db")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for target, want := range map[string]os.FileMode{directory: 0o700, path: 0o600} {
		info, err := os.Stat(target)
		if err != nil {
			t.Fatalf("stat protected path %s: %v", target, err)
		}
		if info.Mode().Perm() != want {
			t.Fatalf("path %s mode=%v want=%v", target, info.Mode().Perm(), want)
		}
	}
}

func TestStoreCanReopenImmediatelyAfterWALShutdown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restart.db")
	for index := 0; index < 25; index++ {
		store, err := Open(path)
		if err != nil {
			t.Fatalf("open %d: %v", index, err)
		}
		if _, err := store.db.Exec(`INSERT INTO conversations(id, created_at) VALUES(?, ?)`, fmt.Sprintf("conversation-%d", index), formatTime(time.Now().UTC())); err != nil {
			store.Close()
			t.Fatalf("write %d: %v", index, err)
		}
		if err := store.Close(); err != nil {
			t.Fatalf("close %d: %v", index, err)
		}
	}
}

func TestListMessagesBeforeReturnsLatestPageInAscendingOrder(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for index := 0; index < 5; index++ {
		if _, err := store.CreateInbound(ctx, "web", testRef(fmt.Sprintf("message-%d", index), fmt.Sprintf("hash-%d", index)), nil); err != nil {
			t.Fatal(err)
		}
	}
	latest, err := store.ListMessagesBefore(ctx, 0, 2)
	if err != nil || len(latest) != 2 || latest[0].Sequence != 4 || latest[1].Sequence != 5 {
		t.Fatalf("latest=%+v err=%v", latest, err)
	}
	older, err := store.ListMessagesBefore(ctx, latest[0].Sequence, 2)
	if err != nil || len(older) != 2 || older[0].Sequence != 2 || older[1].Sequence != 3 {
		t.Fatalf("older=%+v err=%v", older, err)
	}
}

func TestEnsureIntroductionCommitsOneDurableAgentTaskAndEvent(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "introduction.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	first, err := store.EnsureIntroduction(ctx, "conversation_web", testRef("introduction-trigger", "introduction-hash"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.EnsureIntroduction(ctx, "cli", testRef("duplicate-trigger", "duplicate-hash"))
	if err != nil {
		t.Fatal(err)
	}
	if !first.IntroductionStarted || second.IntroductionStarted || first.TaskID == "" || second.TaskID != first.TaskID {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
	for query, want := range map[string]int{
		`SELECT COUNT(*) FROM conversation_introductions`:                                                             1,
		`SELECT COUNT(*) FROM tasks WHERE id = '` + first.TaskID + `'`:                                                1,
		`SELECT COUNT(*) FROM internal_outbox WHERE aggregate_id = '` + first.TaskID + `' AND kind = 'task.wake'`:     1,
		`SELECT COUNT(*) FROM events WHERE aggregate_id = 'primary' AND type = 'conversation.introduction.requested'`: 1,
	} {
		var count int
		if err := store.db.QueryRow(query).Scan(&count); err != nil || count != want {
			t.Fatalf("query=%q count=%d want=%d err=%v", query, count, want, err)
		}
	}
}

func TestListMessagesForTaskDoesNotDependOnGlobalPagination(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "task-messages.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	first, err := store.CreateInbound(ctx, "cli", testRef("first", "first-hash"), nil)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 550; index++ {
		if _, err := store.CreateInbound(ctx, "cli", testRef(fmt.Sprintf("noise-%d", index), fmt.Sprintf("noise-hash-%d", index)), nil); err != nil {
			t.Fatal(err)
		}
	}
	records, err := store.ListMessagesForTask(ctx, first.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].TaskID != first.TaskID {
		t.Fatalf("task records = %+v", records)
	}
}

func TestContextCheckpointReplacesOnlyOlderConversationHistory(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "eri.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	first, err := store.CreateInbound(ctx, "web", testRef("context-first", "hash-first"), nil)
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := store.ClaimTask(ctx, first.TaskID, "worker", time.Minute, "soul", `{}`, "test:model")
	if err != nil || !ok {
		t.Fatalf("claimed=%+v ok=%t err=%v", claimed, ok, err)
	}
	second, err := store.CreateInbound(ctx, "web", testRef("context-second", "hash-second"), nil)
	if err != nil {
		t.Fatal(err)
	}
	var secondSequence int64
	if err := store.db.QueryRow(`SELECT sequence FROM interactions WHERE id = ?`, second.InteractionID).Scan(&secondSequence); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveContextCheckpoint(ctx, first.TaskID, claimed.RunID, agent.ContextCheckpoint{
		ID: "checkpoint", SummaryRef: testRef("context-summary", "hash-summary"),
		FirstKeptSequence: secondSequence, SummarizedCount: 1, TokensBefore: 9000, TokensAfter: 3000,
	}); err != nil {
		t.Fatal(err)
	}
	third, err := store.CreateInbound(ctx, "web", testRef("context-third", "hash-third"), nil)
	if err != nil {
		t.Fatal(err)
	}
	loaded, ok, err := store.ClaimTask(ctx, third.TaskID, "other-worker", time.Minute, "soul", `{}`, "test:model")
	if err != nil || !ok {
		t.Fatalf("loaded=%+v ok=%t err=%v", loaded, ok, err)
	}
	if len(loaded.Messages) != 3 {
		t.Fatalf("messages=%+v", loaded.Messages)
	}
	if loaded.Messages[0].Kind != "context_checkpoint" || loaded.Messages[0].ID != "checkpoint" {
		t.Fatalf("checkpoint message=%+v", loaded.Messages[0])
	}
	if loaded.Messages[0].Sequence != secondSequence {
		t.Fatalf("checkpoint coverage boundary=%d, want %d", loaded.Messages[0].Sequence, secondSequence)
	}
	if loaded.Messages[1].ID != second.InteractionID || loaded.Messages[2].ID != third.InteractionID {
		t.Fatalf("retained messages=%+v", loaded.Messages)
	}
}

func TestClaimOlderQueuedTaskSkipsCheckpointBeyondItsSource(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "eri.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	older, err := store.CreateInbound(ctx, "web", testRef("queued-old-source", "queued-old-hash"), nil)
	if err != nil {
		t.Fatal(err)
	}
	newer, err := store.CreateInbound(ctx, "web", testRef("queued-new-source", "queued-new-hash"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if newer.TaskID == older.TaskID {
		t.Fatal("new input unexpectedly joined an undispatched queued task")
	}
	newerTask, claimed, err := store.ClaimTask(ctx, newer.TaskID, "newer-worker", time.Minute, "soul", `{}`, "test:model")
	if err != nil || !claimed {
		t.Fatalf("newer task=%+v claimed=%v err=%v", newerTask, claimed, err)
	}
	var newerSequence int64
	if err := store.db.QueryRow(`SELECT sequence FROM interactions WHERE id = ?`, newer.InteractionID).Scan(&newerSequence); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveContextCheckpoint(ctx, newer.TaskID, newerTask.RunID, agent.ContextCheckpoint{
		ID: "too-new-checkpoint", SummaryRef: testRef("too-new-summary", "too-new-summary-hash"),
		FirstKeptSequence: newerSequence, SummarizedCount: 1, TokensBefore: 9000, TokensAfter: 3000,
	}); err != nil {
		t.Fatal(err)
	}
	olderTask, claimed, err := store.ClaimTask(ctx, older.TaskID, "older-worker", time.Minute, "soul", `{}`, "test:model")
	if err != nil || !claimed {
		t.Fatalf("older task=%+v claimed=%v err=%v", olderTask, claimed, err)
	}
	for _, message := range olderTask.Messages {
		if message.Kind == "context_checkpoint" {
			t.Fatalf("older source was hidden behind newer checkpoint: %+v", olderTask.Messages)
		}
	}
	foundSource := false
	for _, message := range olderTask.Messages {
		if message.ID == older.InteractionID {
			foundSource = true
		}
	}
	if !foundSource {
		t.Fatalf("older task source missing: %+v", olderTask.Messages)
	}
}

func TestClaimTaskExcludesHistoricalRuntimeCardsFromModelContext(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "eri.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	first, err := store.CreateInbound(ctx, "web", testRef("history-user", "history-user-hash"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE tasks SET status = 'completed' WHERE id = ?`, first.TaskID); err != nil {
		t.Fatal(err)
	}
	var conversationID string
	if err := store.db.QueryRow(`SELECT conversation_id FROM tasks WHERE id = ?`, first.TaskID).Scan(&conversationID); err != nil {
		t.Fatal(err)
	}
	now := formatTime(time.Now().UTC())
	for _, record := range []struct{ id, kind string }{{"approval-card", "approval_request"}, {"runtime-card", "runtime_error"}} {
		if _, err := store.db.Exec(`
			INSERT INTO interactions(id, conversation_id, task_id, direction, role, kind, channel, content_ref_json, created_at)
			VALUES(?, ?, ?, 'outbound', 'system', ?, 'web', '{}', ?)`, record.id, conversationID, first.TaskID, record.kind, now); err != nil {
			t.Fatal(err)
		}
	}
	second, err := store.CreateInbound(ctx, "web", testRef("current-user", "current-user-hash"), nil)
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := store.ClaimTask(ctx, second.TaskID, "worker", time.Minute, "soul", `{}`, "test:model")
	if err != nil || !ok {
		t.Fatalf("claimed=%+v ok=%t err=%v", claimed, ok, err)
	}
	for _, message := range claimed.Messages {
		if message.Kind == "approval_request" || message.Kind == "runtime_error" {
			t.Fatalf("historical Runtime card entered model context: %+v", message)
		}
	}
}

func TestEffectIntentPersistsValidatedParentDelegation(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "eri.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sent, err := store.CreateInbound(ctx, "web", testRef("parent-inbound", "parent-hash"), nil)
	if err != nil {
		t.Fatal(err)
	}
	task, claimed, err := store.ClaimTask(ctx, sent.TaskID, "worker", time.Minute, "soul", `{}`, "test:model")
	if err != nil || !claimed {
		t.Fatalf("task=%+v claimed=%v err=%v", task, claimed, err)
	}
	now := time.Now().UTC()
	parent := tool.Intent{
		ID: "parent-delegation", TaskID: task.TaskID, RunID: task.RunID, ToolID: "builtin.delegate", ToolVersion: "1.0.0",
		Effect: policy.ReadOnly, Target: "subagent:native:read_only", ParametersHash: "parent-parameters", IdempotencyKey: "parent-key",
		Control: policy.Auto, ReconciliationStrategy: "replay", Status: tool.IntentPlanned, CreatedAt: now, UpdatedAt: now,
	}
	if _, created, err := store.PlanIntent(ctx, parent); err != nil || !created {
		t.Fatalf("parent created=%v err=%v", created, err)
	}
	child := tool.Intent{
		ID: "child-tool", TaskID: task.TaskID, RunID: task.RunID, ParentIntentID: parent.ID,
		ToolID: "builtin.web", ToolVersion: "1.0.0", Effect: policy.ReadOnly, Target: "research",
		ParametersHash: "child-parameters", IdempotencyKey: "child-key", Control: policy.Auto,
		ReconciliationStrategy: "replay", Status: tool.IntentPlanned, CreatedAt: now, UpdatedAt: now,
	}
	if _, created, err := store.PlanIntent(ctx, child); err != nil || !created {
		t.Fatalf("child created=%v err=%v", created, err)
	}
	loaded, found, err := store.LoadIntentByID(ctx, child.ID)
	if err != nil || !found || loaded.ParentIntentID != parent.ID {
		t.Fatalf("child=%+v found=%v err=%v", loaded, found, err)
	}
	child.ID, child.IdempotencyKey, child.ParentIntentID = "orphan-child", "orphan-key", "missing-parent"
	if _, _, err := store.PlanIntent(ctx, child); err == nil {
		t.Fatal("orphan child intent was accepted")
	}
}

func TestRenewOutboxLeaseAlsoRenewsRunningTaskLease(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "eri.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sent, err := store.CreateInbound(ctx, "web", testRef("lease-inbound", "lease-hash"), nil)
	if err != nil {
		t.Fatal(err)
	}
	wake, ok, err := store.ClaimOutbox(ctx, "worker", 250*time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("wake=%+v ok=%t err=%v", wake, ok, err)
	}
	if _, ok, err := store.ClaimTask(ctx, sent.TaskID, "worker", 250*time.Millisecond, "soul", `{}`, "test:model"); err != nil || !ok {
		t.Fatalf("claim task ok=%t err=%v", ok, err)
	}
	var before string
	if err := store.db.QueryRowContext(ctx, `SELECT lease_until FROM tasks WHERE id = ?`, sent.TaskID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := store.RenewOutboxLease(ctx, wake.ID, "worker", time.Minute); err != nil {
		t.Fatal(err)
	}
	var taskLease, outboxLease string
	if err := store.db.QueryRowContext(ctx, `SELECT lease_until FROM tasks WHERE id = ?`, sent.TaskID).Scan(&taskLease); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT lease_until FROM internal_outbox WHERE id = ?`, wake.ID).Scan(&outboxLease); err != nil {
		t.Fatal(err)
	}
	if taskLease <= before || outboxLease != taskLease {
		t.Fatalf("before=%s task=%s outbox=%s", before, taskLease, outboxLease)
	}
}

func TestUnknownEffectQueuesReconciliationWithDurablePayload(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "eri.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sent, err := store.CreateInbound(ctx, "web", testRef("reconcile-inbound", "reconcile-hash"), nil)
	if err != nil {
		t.Fatal(err)
	}
	wake, ok, err := store.ClaimOutbox(ctx, "worker", time.Minute)
	if err != nil || !ok {
		t.Fatalf("wake=%+v ok=%t err=%v", wake, ok, err)
	}
	if err := store.CompleteOutbox(ctx, wake.ID); err != nil {
		t.Fatal(err)
	}
	task, ok, err := store.ClaimTask(ctx, sent.TaskID, "worker", time.Minute, "soul", `{}`, "test:model")
	if err != nil || !ok {
		t.Fatalf("task=%+v ok=%t err=%v", task, ok, err)
	}
	payloadRef := testRef("effect-payload", "effect-payload-hash")
	intent, created, err := store.PlanIntent(ctx, tool.Intent{
		ID: "effect", TaskID: task.TaskID, RunID: task.RunID, ToolID: "mcp.calendar.create", ToolVersion: "1.0.0",
		Effect: policy.Reversible, Target: "calendar:event", ParametersHash: "parameters", PayloadRef: payloadRef,
		IdempotencyKey: "effect-key", Control: policy.Auto, ReconciliationStrategy: "provider_event_id",
		Status: tool.IntentPlanned, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	if err != nil || !created {
		t.Fatalf("intent=%+v created=%t err=%v", intent, created, err)
	}
	if err := store.TransitionIntent(ctx, intent.ID, tool.IntentPlanned, tool.IntentAuthorized, "", "", "", content.Ref{}); err != nil {
		t.Fatal(err)
	}
	if err := store.TransitionIntent(ctx, intent.ID, tool.IntentAuthorized, tool.IntentDispatched, "", "", "", content.Ref{}); err != nil {
		t.Fatal(err)
	}
	if err := store.TransitionIntent(ctx, intent.ID, tool.IntentDispatched, tool.IntentUnknown, "timeout", "", "", content.Ref{}); err != nil {
		t.Fatal(err)
	}
	loaded, found, err := store.LoadIntentByID(ctx, intent.ID)
	if err != nil || !found || loaded.PayloadRef.ObjectID != payloadRef.ObjectID {
		t.Fatalf("loaded=%+v found=%t err=%v", loaded, found, err)
	}
	reconcile, ok, err := store.ClaimOutbox(ctx, "worker", time.Minute)
	if err != nil || !ok || reconcile.Kind != "effect.reconcile" || reconcile.AggregateID != intent.ID {
		t.Fatalf("reconcile=%+v ok=%t err=%v", reconcile, ok, err)
	}
}

func TestDatasetCandidateFreezesIntoImmutableVersionedSnapshot(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := Open(filepath.Join(root, "eri.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	contents, err := content.New(filepath.Join(root, "content"), []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	now := formatTime(time.Now().UTC())
	episodeRef := testRef("episode-manifest", "episode-manifest-hash")
	encodedEpisodeRef, _ := json.Marshal(episodeRef)
	for _, statement := range []string{
		`INSERT INTO conversations(id, created_at) VALUES('primary', '` + now + `')`,
		`INSERT INTO interactions(id, conversation_id, task_id, direction, role, kind, channel, content_ref_json, created_at)
		 VALUES('source', 'primary', 'task', 'inbound', 'user', 'message', 'web', '{}', '` + now + `')`,
		`INSERT INTO tasks(id, conversation_id, source_interaction_id, source_channel, status, terminal_status, version, created_at, updated_at)
		 VALUES('task', 'primary', 'source', 'web', 'completed', 'completed', 1, '` + now + `', '` + now + `')`,
		`INSERT INTO episodes(id, task_id, manifest_ref_json, status, created_at)
		 VALUES('episode', 'task', '` + string(encodedEpisodeRef) + `', 'ready', '` + now + `')`,
		`INSERT INTO dataset_candidates(id, episode_id, status, created_at)
		 VALUES('candidate', 'episode', 'candidate', '` + now + `')`,
	} {
		if _, err := store.db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	service := episode.NewService(store, contents)
	snapshot, err := service.FreezeDataset(ctx, "regression evaluation", "stable-seed", []string{"candidate"})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Version != 1 || snapshot.Status != "frozen" || snapshot.ItemCount != 1 {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	var split string
	if err := store.db.QueryRowContext(ctx, `SELECT split FROM dataset_snapshot_items WHERE snapshot_id = ?`, snapshot.ID).Scan(&split); err != nil {
		t.Fatal(err)
	}
	if split != "development" && split != "eval" && split != "holdout" {
		t.Fatalf("split=%q", split)
	}
	listed, err := service.DatasetSnapshots(ctx, 10)
	if err != nil || len(listed) != 1 || listed[0].ID != snapshot.ID {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}
}

func TestFullUserDataErasureWaitsForFinalDeliveryThenWipesData(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "eri.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sent, err := store.CreateInbound(ctx, "web", testRef("erase-inbound", "hash-in"), nil)
	if err != nil {
		t.Fatal(err)
	}
	wake, ok, err := store.ClaimOutbox(ctx, "worker", time.Minute)
	if err != nil || !ok || wake.Kind != "task.wake" {
		t.Fatalf("wake=%+v ok=%v err=%v", wake, ok, err)
	}
	if err := store.CompleteOutbox(ctx, wake.ID); err != nil {
		t.Fatal(err)
	}
	task, claimed, err := store.ClaimTask(ctx, sent.TaskID, "worker", time.Minute, "soul", `{}`, "test:model")
	if err != nil || !claimed {
		t.Fatalf("task=%+v claimed=%v err=%v", task, claimed, err)
	}
	if err := store.MarkRunDispatched(ctx, task.RunID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	job, err := store.ScheduleUserDataErasure(ctx, userdata.ErasureJob{
		ID: "erasure-job", TaskID: sent.TaskID, Status: "awaiting_delivery", CreatedAt: now, UpdatedAt: now,
	})
	if err != nil || job.Status != "awaiting_delivery" {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	commit := testCommit(task, testRef("erase-reply", "hash-out"), eval.Pass)
	if err := store.CommitArtifact(ctx, commit); err != nil {
		t.Fatal(err)
	}
	deliveryItem, ok, err := store.ClaimOutbox(ctx, "worker", time.Minute)
	if err != nil || !ok || deliveryItem.Kind != "delivery.send" {
		t.Fatalf("delivery=%+v ok=%v err=%v", deliveryItem, ok, err)
	}
	if err := store.CommitConversationDelivery(ctx, commit.DeliveryID, "erase-confirmation", delivery.Receipt{Level: "accepted_by_channel"}, now); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteOutbox(ctx, deliveryItem.ID); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := store.db.QueryRow(`SELECT status FROM data_erasure_jobs WHERE id = ?`, job.ID).Scan(&status); err != nil || status != "ready" {
		t.Fatalf("status=%q err=%v", status, err)
	}
	refs, found, err := store.PrepareUserDataErasure(ctx, job.ID)
	if err != nil || !found || len(refs) == 0 {
		t.Fatalf("refs=%d found=%v err=%v", len(refs), found, err)
	}
	if err := store.CommitUserDataErasure(ctx, job.ID, len(refs), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"tasks", "interactions", "artifacts", "eval_records", "events", "content_objects"} {
		var count int
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s count=%d err=%v", table, count, err)
		}
	}
	var erasedObjects int
	if err := store.db.QueryRow(`SELECT content_objects FROM data_erasure_jobs WHERE id = ? AND status = 'completed'`, job.ID).Scan(&erasedObjects); err != nil || erasedObjects != len(refs) {
		t.Fatalf("erasure receipt objects=%d want=%d err=%v", erasedObjects, len(refs), err)
	}
	if _, err := store.CreateInbound(ctx, "web", testRef("clean-start", "clean-hash"), nil); err != nil {
		t.Fatalf("new clean conversation after erasure: %v", err)
	}
}

func TestReliableReplyTransactionFlowIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	inboundRef := testRef("inbound", "hash-in")
	sent, err := store.CreateInbound(ctx, "cli", inboundRef, nil)
	if err != nil {
		t.Fatal(err)
	}
	presence, err := store.Presence(ctx)
	if err != nil || presence.State != "working" {
		t.Fatalf("presence = %+v, err = %v", presence, err)
	}
	item, ok, err := store.ClaimOutbox(ctx, "worker-a", time.Minute)
	if err != nil || !ok || item.Kind != "task.wake" || item.AggregateID != sent.TaskID {
		t.Fatalf("wake item = %+v, ok = %v, err = %v", item, ok, err)
	}
	if err := store.CompleteOutbox(ctx, item.ID); err != nil {
		t.Fatal(err)
	}
	task, claimed, err := store.ClaimTask(ctx, sent.TaskID, "worker-a", time.Minute, "soul-v1", `{}`, "test:model")
	if err != nil || !claimed {
		t.Fatalf("task = %+v, claimed = %v, err = %v", task, claimed, err)
	}
	var runTarget string
	if err := store.db.QueryRowContext(ctx, `SELECT target FROM runs WHERE id = ?`, task.RunID).Scan(&runTarget); err != nil || runTarget != "test:model" {
		t.Fatalf("run target=%q err=%v", runTarget, err)
	}
	if _, claimed, err := store.ClaimTask(ctx, sent.TaskID, "worker-b", time.Minute, "soul-v1", `{}`, "test:model"); err != nil || claimed {
		t.Fatalf("second claim claimed = %v, err = %v", claimed, err)
	}
	if err := store.MarkRunDispatched(ctx, task.RunID); err != nil {
		t.Fatal(err)
	}
	commit := testCommit(task, testRef("reply", "hash-out"), eval.Pass)
	commit.Usage = agent.Usage{
		Provider: "deepseek", Model: "deepseek-v4-flash", ModelCalls: 2,
		InputTokens: 300, OutputTokens: 40, CacheHitTokens: 240, CacheMissTokens: 60,
	}
	if err := store.CommitArtifact(ctx, commit); err != nil {
		t.Fatal(err)
	}
	presence, err = store.Presence(ctx)
	if err != nil || presence.State != "available" {
		t.Fatalf("waiting delivery must not show Working: %+v, err = %v", presence, err)
	}
	deliveryItem, ok, err := store.ClaimOutbox(ctx, "worker-a", time.Minute)
	if err != nil || !ok || deliveryItem.Kind != "delivery.send" {
		t.Fatalf("delivery item = %+v, ok = %v, err = %v", deliveryItem, ok, err)
	}
	if err := store.CommitConversationDelivery(ctx, commit.DeliveryID, "outbound-interaction", delivery.Receipt{Level: "accepted_by_channel"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := store.CommitConversationDelivery(ctx, commit.DeliveryID, "duplicate-interaction", delivery.Receipt{Level: "accepted_by_channel"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	messages, err := store.ListMessages(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 {
		t.Fatalf("message count = %d, want 2", len(messages))
	}
	status, err := store.TaskStatus(ctx, sent.TaskID)
	if err != nil || status.Status != "completed" {
		t.Fatalf("status = %+v, err = %v", status, err)
	}
	events, err := store.ListEvents(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < 8 {
		t.Fatalf("event count = %d, want causal lifecycle", len(events))
	}
	var runUsage map[string]any
	for _, event := range events {
		if event.Type == "run.succeeded" {
			runUsage = event.Data
		}
	}
	if runUsage["cache_hit_tokens"] != float64(240) || runUsage["cache_miss_tokens"] != float64(60) {
		t.Fatalf("cache telemetry missing from run event: %+v", runUsage)
	}
}

func TestClaimTaskDoesNotCarryProviderTranscriptAcrossModelTargets(t *testing.T) {
	ctx := context.Background()
	store, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	first, err := store.CreateInbound(ctx, "cli", testRef("first-inbound", "first-inbound-hash"), nil)
	if err != nil {
		t.Fatal(err)
	}
	wake, ok, err := store.ClaimOutbox(ctx, "worker", time.Minute)
	if err != nil || !ok || wake.Kind != "task.wake" {
		t.Fatalf("first wake=%+v ok=%t err=%v", wake, ok, err)
	}
	if err := store.CompleteOutbox(ctx, wake.ID); err != nil {
		t.Fatal(err)
	}
	firstTask, claimed, err := store.ClaimTask(ctx, first.TaskID, "worker", time.Minute, "soul", `{}`, "ollama:model-a")
	if err != nil || !claimed {
		t.Fatalf("first task=%+v claimed=%t err=%v", firstTask, claimed, err)
	}
	if err := store.MarkRunDispatched(ctx, firstTask.RunID); err != nil {
		t.Fatal(err)
	}
	commit := testCommit(firstTask, testRef("first-reply", "first-reply-hash"), eval.Pass)
	if err := store.CommitArtifact(ctx, commit); err != nil {
		t.Fatal(err)
	}
	deliveryItem, ok, err := store.ClaimOutbox(ctx, "worker", time.Minute)
	if err != nil || !ok || deliveryItem.Kind != "delivery.send" {
		t.Fatalf("first delivery=%+v ok=%t err=%v", deliveryItem, ok, err)
	}
	if err := store.CommitConversationDelivery(ctx, commit.DeliveryID, "first-outbound", delivery.Receipt{Level: "accepted_by_channel"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteOutbox(ctx, deliveryItem.ID); err != nil {
		t.Fatal(err)
	}

	second, err := store.CreateInbound(ctx, "cli", testRef("second-inbound", "second-inbound-hash"), nil)
	if err != nil {
		t.Fatal(err)
	}
	for {
		secondWake, ok, err := store.ClaimOutbox(ctx, "worker", time.Minute)
		if err != nil || !ok {
			t.Fatalf("second wake=%+v ok=%t err=%v", secondWake, ok, err)
		}
		if err := store.CompleteOutbox(ctx, secondWake.ID); err != nil {
			t.Fatal(err)
		}
		if secondWake.Kind == "task.wake" {
			break
		}
	}
	secondTask, claimed, err := store.ClaimTask(ctx, second.TaskID, "worker", time.Minute, "soul", `{}`, "deepseek:model-b")
	if err != nil || !claimed {
		t.Fatalf("second task=%+v claimed=%t err=%v", secondTask, claimed, err)
	}
	if secondTask.PriorTranscriptRef.ObjectID != "" || secondTask.PriorTranscriptSequence != 0 {
		t.Fatalf("cross-target provider transcript was carried: ref=%+v sequence=%d", secondTask.PriorTranscriptRef, secondTask.PriorTranscriptSequence)
	}
	if err := store.MarkRunDispatched(ctx, secondTask.RunID); err != nil {
		t.Fatal(err)
	}
	secondCommit := testCommit(secondTask, testRef("second-reply", "second-reply-hash"), eval.Pass)
	if err := store.CommitArtifact(ctx, secondCommit); err != nil {
		t.Fatal(err)
	}
	secondDelivery, ok, err := store.ClaimOutbox(ctx, "worker", time.Minute)
	if err != nil || !ok || secondDelivery.Kind != "delivery.send" {
		t.Fatalf("second delivery=%+v ok=%t err=%v", secondDelivery, ok, err)
	}
	if err := store.CommitConversationDelivery(ctx, secondCommit.DeliveryID, "second-outbound", delivery.Receipt{Level: "accepted_by_channel"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteOutbox(ctx, secondDelivery.ID); err != nil {
		t.Fatal(err)
	}

	third, err := store.CreateInbound(ctx, "cli", testRef("third-inbound", "third-inbound-hash"), nil)
	if err != nil {
		t.Fatal(err)
	}
	thirdTask, claimed, err := store.ClaimTask(ctx, third.TaskID, "worker", time.Minute, "soul", `{}`, "ollama:model-a")
	if err != nil || !claimed {
		t.Fatalf("third task=%+v claimed=%t err=%v", thirdTask, claimed, err)
	}
	if thirdTask.PriorTranscriptRef.ObjectID != "" || thirdTask.PriorTranscriptSequence != 0 {
		t.Fatalf("provider carry skipped the intervening DeepSeek turn and resurrected older Ollama context: ref=%+v sequence=%d", thirdTask.PriorTranscriptRef, thirdTask.PriorTranscriptSequence)
	}
}

func TestClaimTaskDoesNotCarryAcrossInterveningFailedTurn(t *testing.T) {
	ctx := context.Background()
	store, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	createAndClaim := func(name string) (channel.SendResult, agent.TaskContext) {
		t.Helper()
		inbound, err := store.CreateInbound(ctx, "cli", testRef(name+"-inbound", name+"-inbound-hash"), nil)
		if err != nil {
			t.Fatal(err)
		}
		for {
			wake, ok, err := store.ClaimOutbox(ctx, "worker", time.Minute)
			if err != nil || !ok {
				t.Fatalf("%s wake=%+v ok=%t err=%v", name, wake, ok, err)
			}
			if err := store.CompleteOutbox(ctx, wake.ID); err != nil {
				t.Fatal(err)
			}
			if wake.Kind == "task.wake" {
				break
			}
		}
		task, claimed, err := store.ClaimTask(ctx, inbound.TaskID, "worker", time.Minute, "soul", `{}`, "deepseek:model")
		if err != nil || !claimed {
			t.Fatalf("%s task=%+v claimed=%t err=%v", name, task, claimed, err)
		}
		return inbound, task
	}
	deliver := func(name string, task agent.TaskContext, terminal, kind string) {
		t.Helper()
		if err := store.MarkRunDispatched(ctx, task.RunID); err != nil {
			t.Fatal(err)
		}
		commit := testCommit(task, testRef(name+"-reply", name+"-reply-hash"), eval.Pass)
		commit.TerminalStatus = terminal
		commit.ArtifactKind = kind
		if terminal == "failed" {
			commit.FailureCode = "simulated_failure"
		}
		if err := store.CommitArtifact(ctx, commit); err != nil {
			t.Fatal(err)
		}
		item, ok, err := store.ClaimOutbox(ctx, "worker", time.Minute)
		if err != nil || !ok || item.Kind != "delivery.send" {
			t.Fatalf("%s delivery=%+v ok=%t err=%v", name, item, ok, err)
		}
		if err := store.CommitConversationDelivery(ctx, commit.DeliveryID, name+"-outbound", delivery.Receipt{Level: "accepted_by_channel"}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		if err := store.CompleteOutbox(ctx, item.ID); err != nil {
			t.Fatal(err)
		}
	}

	_, firstTask := createAndClaim("first")
	deliver("first", firstTask, "completed", "text")
	_, failedTask := createAndClaim("failed")
	deliver("failed", failedTask, "failed", "runtime_error")
	_, currentTask := createAndClaim("current")
	if currentTask.PriorTranscriptRef.ObjectID != "" || currentTask.PriorTranscriptSequence != 0 {
		t.Fatalf("provider carry skipped an intervening failed turn: ref=%+v sequence=%d", currentTask.PriorTranscriptRef, currentTask.PriorTranscriptSequence)
	}
}

func TestEvalAndDeliveryOutboxCommitAtomically(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	sent, err := store.CreateInbound(ctx, "web", testRef("atomic-in", "hash-in"), nil)
	if err != nil {
		t.Fatal(err)
	}
	wake, _, err := store.ClaimOutbox(ctx, "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteOutbox(ctx, wake.ID); err != nil {
		t.Fatal(err)
	}
	task, claimed, err := store.ClaimTask(ctx, sent.TaskID, "worker", time.Minute, "soul", `{}`, "test:model")
	if err != nil || !claimed {
		t.Fatalf("claim err = %v", err)
	}
	if err := store.MarkRunDispatched(ctx, task.RunID); err != nil {
		t.Fatal(err)
	}
	invalid := testCommit(task, testRef("atomic-out", "hash-out"), eval.Result("not-valid"))
	if err := store.CommitArtifact(ctx, invalid); err == nil {
		t.Fatal("invalid Eval result unexpectedly committed")
	}
	if _, found, err := store.LoadDelivery(ctx, invalid.DeliveryID); err != nil || found {
		t.Fatalf("delivery found after rolled-back Eval: found=%v err=%v", found, err)
	}
	invalid.EvalResult = eval.Pass
	if err := store.CommitArtifact(ctx, invalid); err != nil {
		t.Fatalf("valid retry after rollback: %v", err)
	}
	if _, found, err := store.LoadDelivery(ctx, invalid.DeliveryID); err != nil || !found {
		t.Fatalf("delivery missing after valid commit: found=%v err=%v", found, err)
	}
}

func TestAppliedMemoryUseCommitsAtomicallyWithAcceptedArtifact(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(map[bool]string{false: "accepted", true: "stale"}[stale], func(t *testing.T) {
			ctx := context.Background()
			store, err := Open(filepath.Join(t.TempDir(), "eri.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			contentStore, err := content.New(t.TempDir(), []byte("0123456789abcdef0123456789abcdef"))
			if err != nil {
				t.Fatal(err)
			}
			memoryService, err := memory.NewService(store, contentStore, []byte("0123456789abcdef0123456789abcdef"))
			if err != nil {
				t.Fatal(err)
			}
			entry, err := memoryService.Capture(ctx, memory.CaptureRequest{
				Statement: "Keep reports concise.", Kind: "preference", Scope: "communication",
				Relation: memory.Supports, SourceType: "user", SourceRef: "interaction:memory-source",
				IndependenceGroup: "user:self", DirectUserStatement: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			sent, err := store.CreateInbound(ctx, "web", testRef("memory-atomic-in", "memory-atomic-in-hash"), nil)
			if err != nil {
				t.Fatal(err)
			}
			task, claimed, err := store.ClaimTask(ctx, sent.TaskID, "worker", time.Minute, "soul", `{}`, "test:model")
			if err != nil || !claimed {
				t.Fatalf("task=%+v claimed=%v err=%v", task, claimed, err)
			}
			if err := store.MarkRunDispatched(ctx, task.RunID); err != nil {
				t.Fatal(err)
			}
			bundle, err := memoryService.Recall(ctx, memory.RecallRequest{
				Query: "concise report", RunID: task.RunID, SourceInteractionID: sent.InteractionID, Limit: 5,
			})
			if err != nil || len(bundle.Entries) != 1 {
				t.Fatalf("bundle=%+v err=%v", bundle, err)
			}
			commit := testCommit(task, testRef("memory-atomic-out", "memory-atomic-out-hash"), eval.Pass)
			commit.AppliedMemoryUses = []agent.MemoryUse{{RetrievalID: bundle.RetrievalID, MemoryIDs: []string{entry.MemoryID}}}
			if stale {
				commit.BasisInputSequence = task.InputSequence
				if _, err := store.CreateInbound(ctx, "web", testRef("memory-atomic-newer", "memory-atomic-newer-hash"), nil); err != nil {
					t.Fatal(err)
				}
			}
			err = store.CommitArtifact(ctx, commit)
			if stale && !errors.Is(err, agent.ErrStaleTaskInput) {
				t.Fatalf("stale commit error=%v", err)
			}
			if !stale && err != nil {
				t.Fatal(err)
			}
			var used, accessCount int
			if err := store.db.QueryRowContext(ctx, `
				SELECT retrieval.used, item.access_count
				FROM memory_retrieval_items retrieval
				JOIN memory_items item ON item.id = retrieval.memory_id
				WHERE retrieval.retrieval_id = ? AND retrieval.memory_id = ?`, bundle.RetrievalID, entry.MemoryID).Scan(&used, &accessCount); err != nil {
				t.Fatal(err)
			}
			want := 1
			if stale {
				want = 0
			}
			if used != want || accessCount != want {
				t.Fatalf("used=%d access_count=%d want=%d", used, accessCount, want)
			}
		})
	}
}

func TestProgressDeliveryKeepsRunningTaskOpenAndDeduplicatesContent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	sent, err := store.CreateInbound(ctx, "web", testRef("progress-in", "progress-in-hash"), nil)
	if err != nil {
		t.Fatal(err)
	}
	wake, ok, err := store.ClaimOutbox(ctx, "worker", time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim wake ok=%v err=%v", ok, err)
	}
	if err := store.CompleteOutbox(ctx, wake.ID); err != nil {
		t.Fatal(err)
	}
	task, claimed, err := store.ClaimTask(ctx, sent.TaskID, "worker", time.Minute, "soul", `{}`, "test:model")
	if err != nil || !claimed {
		t.Fatalf("claim task=%+v claimed=%v err=%v", task, claimed, err)
	}
	commit := testCommit(task, testRef("progress-out", "progress-content-hash"), eval.Pass)
	commit.ArtifactID = "progress-artifact"
	commit.EvalID = "progress-eval"
	commit.DeliveryID = "progress-delivery"
	commit.ArtifactKind = "progress"
	commit.EvalTier = "routine"
	commit.EvalEvaluator = "llm_judge_progress"
	created, err := store.CommitProgress(ctx, agent.ProgressCommit{Commit: commit, ModelTurnID: task.RunID + ":turn:1"})
	if err != nil || !created {
		t.Fatalf("commit progress created=%v err=%v", created, err)
	}
	var taskStatus, deliveryStatus string
	var continueTask int
	if err := store.db.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id = ?`, task.TaskID).Scan(&taskStatus); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT status, continue_task FROM deliveries WHERE id = ?`, commit.DeliveryID).Scan(&deliveryStatus, &continueTask); err != nil {
		t.Fatal(err)
	}
	if taskStatus != "running" || deliveryStatus != "queued" || continueTask != 1 {
		t.Fatalf("task=%q delivery=%q continue_task=%d", taskStatus, deliveryStatus, continueTask)
	}
	deliveryItem, ok, err := store.ClaimOutbox(ctx, "delivery-worker", time.Minute)
	if err != nil || !ok || deliveryItem.Kind != "delivery.send" || deliveryItem.AggregateID != commit.DeliveryID {
		t.Fatalf("progress delivery item=%+v ok=%v err=%v", deliveryItem, ok, err)
	}
	if err := store.CommitConversationDelivery(ctx, commit.DeliveryID, "progress-interaction", delivery.Receipt{Level: "accepted_by_channel"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteOutbox(ctx, deliveryItem.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id = ?`, task.TaskID).Scan(&taskStatus); err != nil {
		t.Fatal(err)
	}
	if taskStatus != "running" {
		t.Fatalf("progress delivery ended task with status %q", taskStatus)
	}
	duplicate := commit
	duplicate.ArtifactID, duplicate.EvalID, duplicate.DeliveryID = "duplicate-artifact", "duplicate-eval", "duplicate-delivery"
	created, err = store.CommitProgress(ctx, agent.ProgressCommit{Commit: duplicate, ModelTurnID: task.RunID + ":turn:1"})
	if err != nil || created {
		t.Fatalf("duplicate progress created=%v err=%v", created, err)
	}
}

func testRef(id, hash string) content.Ref {
	return content.Ref{
		ObjectID: id, Version: 1, ContentHash: hash,
		MediaType: "text/plain", SizeBytes: 4,
		EncryptionDomain: "test", PrivacyClass: "private", RetentionPolicy: "test",
	}
}

func testCommit(task agent.TaskContext, ref content.Ref, result eval.Result) agent.Commit {
	return agent.Commit{
		TaskID: task.TaskID, RunID: task.RunID,
		ArtifactID: "artifact-" + task.TaskID, EvalID: "eval-" + task.TaskID,
		DeliveryID: "delivery-" + task.TaskID, ArtifactKind: "text", ArtifactRef: ref,
		TraceRef:        testRef("trace-"+task.TaskID, "hash-trace"),
		EvalFindingsRef: testRef("eval-findings-"+task.TaskID, "hash-eval-findings"),
		EvalResult:      result, TerminalStatus: "completed",
	}
}
