package sqlite

import (
	"context"
	"testing"
	"time"
)

func TestRuntimeRecoveryReclaimsSameRunAndInvocation(t *testing.T) {
	ctx := context.Background()
	store, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	sent, err := store.CreateInbound(ctx, "cli", testRef("recover-in", "recover-hash"), nil)
	if err != nil {
		t.Fatal(err)
	}
	firstOutbox, ok, err := store.ClaimOutbox(ctx, "dead-worker", 10*time.Minute)
	if err != nil || !ok {
		t.Fatalf("outbox = %+v, ok = %v, err = %v", firstOutbox, ok, err)
	}
	first, claimed, err := store.ClaimTask(ctx, sent.TaskID, "dead-worker", 10*time.Minute, "soul", `{}`, "test:model")
	if err != nil || !claimed {
		t.Fatalf("first claim = %+v, claimed = %v, err = %v", first, claimed, err)
	}
	if err := store.MarkRunDispatched(ctx, first.RunID); err != nil {
		t.Fatal(err)
	}
	checkpointRef := testRef("agent-checkpoint", "agent-checkpoint-hash")
	if err := store.SaveAgentCheckpoint(ctx, first, "model_received", checkpointRef); err != nil {
		t.Fatal(err)
	}
	stats, err := store.RecoverRuntime(ctx, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if stats.RunningTasks != 1 || stats.OutboxItems != 1 || stats.AmbiguousEffects != 0 {
		t.Fatalf("recovery stats = %+v", stats)
	}
	recoveredOutbox, ok, err := store.ClaimOutbox(ctx, "new-worker", time.Minute)
	if err != nil || !ok || recoveredOutbox.ID != firstOutbox.ID {
		t.Fatalf("recovered outbox = %+v, ok = %v, err = %v", recoveredOutbox, ok, err)
	}
	second, claimed, err := store.ClaimTask(ctx, sent.TaskID, "new-worker", time.Minute, "soul", `{"recovered":true}`, "test:model")
	if err != nil || !claimed {
		t.Fatalf("second claim = %+v, claimed = %v, err = %v", second, claimed, err)
	}
	if second.RunID != first.RunID {
		t.Fatalf("recovery created new execution identity: first=%+v second=%+v", first, second)
	}
	if second.CheckpointPhase != "model_received" || second.CheckpointRef.ObjectID != checkpointRef.ObjectID {
		t.Fatalf("recovery lost agent checkpoint: %+v", second)
	}
	if err := store.MarkRunDispatched(ctx, second.RunID); err != nil {
		t.Fatalf("idempotent dispatch marker failed: %v", err)
	}
	var runCount int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE task_id = ?`, sent.TaskID).Scan(&runCount); err != nil || runCount != 1 {
		t.Fatalf("run count = %d, err = %v", runCount, err)
	}
}
