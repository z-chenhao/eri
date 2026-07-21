package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/z-chenhao/eri/internal/agent"
	"github.com/z-chenhao/eri/internal/eval"
	"github.com/z-chenhao/eri/internal/policy"
	"github.com/z-chenhao/eri/internal/tool"
)

func TestApprovalExpiryQueuesDurableContinuation(t *testing.T) {
	ctx := context.Background()
	store, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	sent, err := store.CreateInbound(ctx, "cli", testRef("expiry-in", "hash-in"), nil)
	if err != nil {
		t.Fatal(err)
	}
	wake, ok, err := store.ClaimOutbox(ctx, "worker", time.Minute)
	if err != nil || !ok {
		t.Fatalf("wake = %+v, ok = %v, err = %v", wake, ok, err)
	}
	if err := store.CompleteOutbox(ctx, wake.ID); err != nil {
		t.Fatal(err)
	}
	task, claimed, err := store.ClaimTask(ctx, sent.TaskID, "worker", time.Minute, "soul", `{}`, "test:model")
	if err != nil || !claimed {
		t.Fatalf("claim = %v, err = %v", claimed, err)
	}
	now := time.Now().UTC()
	intent, created, err := store.PlanIntent(ctx, tool.Intent{
		ID: "expiry-intent", TaskID: task.TaskID, RunID: task.RunID, ToolID: "builtin.files", ToolVersion: "1",
		Effect: policy.Destructive, Target: "file", ParametersHash: "hash", IdempotencyKey: "expiry-key",
		Control: policy.StrongApproval, ReconciliationStrategy: "inspect", Status: tool.IntentPlanned, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil || !created {
		t.Fatalf("intent = %+v, created = %v, err = %v", intent, created, err)
	}
	expires := now.Add(-time.Minute)
	if err := store.PauseForApproval(ctx, agent.ApprovalCommit{
		TaskID: task.TaskID, RunID: task.RunID,
		ApprovalID: "expiry-approval", ArtifactID: "expiry-artifact", EvalID: "expiry-eval", DeliveryID: "expiry-delivery",
		Intent: intent, ArtifactRef: testRef("expiry-artifact-ref", "hash-artifact"),
		ContinuationRef: testRef("expiry-continuation", "hash-continuation"),
		EvalFindingsRef: testRef("expiry-findings", "hash-findings"), EvalResult: eval.Pass, ExpiresAt: expires,
	}); err != nil {
		t.Fatal(err)
	}
	count, err := store.ExpireApprovals(ctx, now, 10)
	if err != nil || count != 1 {
		t.Fatalf("expired = %d, err = %v", count, err)
	}
	status, err := store.ApprovalStatus(ctx, "expiry-approval")
	if err != nil || status != "expired" {
		t.Fatalf("approval status = %q, err = %v", status, err)
	}
	foundResume := false
	for range 3 {
		item, ok, err := store.ClaimOutbox(ctx, "worker", time.Minute)
		if err != nil || !ok {
			break
		}
		if item.Kind == "approval.resume" && item.AggregateID == "expiry-approval" {
			foundResume = true
			break
		}
		_ = store.CompleteOutbox(ctx, item.ID)
	}
	if !foundResume {
		t.Fatal("expired approval did not queue its durable continuation")
	}
	resume, claimed, err := store.ClaimApprovalResume(ctx, "expiry-approval", "worker", time.Minute)
	if err != nil || !claimed || resume.Decision != "expired" || resume.Grant != nil {
		t.Fatalf("resume = %+v, claimed = %v, err = %v", resume, claimed, err)
	}
}
