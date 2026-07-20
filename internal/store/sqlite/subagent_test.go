package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/z-chenhao/eri/internal/agent"
	"github.com/z-chenhao/eri/internal/eval"
	"github.com/z-chenhao/eri/internal/policy"
	"github.com/z-chenhao/eri/internal/subagent"
	"github.com/z-chenhao/eri/internal/tool"
)

func TestSubagentCompletionWaitsForTheProgressDeliveryBeforeResuming(t *testing.T) {
	ctx := context.Background()
	store, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	sent, err := store.CreateInbound(ctx, "cli", testRef("codex-in", "codex-in-hash"), nil)
	if err != nil {
		t.Fatal(err)
	}
	wake, ok, err := store.ClaimOutbox(ctx, "worker", time.Minute)
	if err != nil || !ok {
		t.Fatalf("wake=%+v ok=%v err=%v", wake, ok, err)
	}
	if err := store.CompleteOutbox(ctx, wake.ID); err != nil {
		t.Fatal(err)
	}
	task, claimed, err := store.ClaimTask(ctx, sent.TaskID, "worker", time.Minute, "soul", `{}`, "test:model")
	if err != nil || !claimed {
		t.Fatalf("task=%+v claimed=%v err=%v", task, claimed, err)
	}
	now := time.Now().UTC()
	intent, created, err := store.PlanIntent(ctx, tool.Intent{
		ID: "codex-delegation-1", TaskID: task.TaskID, RunID: task.RunID, InvocationID: task.InvocationID,
		ToolCallID: "delegate-call-1", ToolID: "builtin.delegate", ToolVersion: "0.4.0",
		Effect: policy.ReadOnly, Target: "subagent:codex:read_only", ParametersHash: "parameters-hash",
		IdempotencyKey: "codex-key", Control: policy.NotifyAfter, ReconciliationStrategy: "inspect_selected_subagent",
		Status: tool.IntentPlanned, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil || !created {
		t.Fatalf("intent=%+v created=%v err=%v", intent, created, err)
	}
	job, created, err := store.QueueSubagentRun(ctx, subagent.Run{
		ID: intent.ID, RoleID: "engineering_team", ProviderID: "codex", ParentTaskID: task.TaskID, ParentRunID: task.RunID, Access: subagent.ReadOnly,
		RequestRef: testRef("codex-prompt", "codex-prompt-hash"),
	})
	if err != nil || !created || job.Status != "queued" {
		t.Fatalf("job=%+v created=%v err=%v", job, created, err)
	}
	if accepted, err := store.CompleteSubagentRun(ctx, job.ID, "completed", "", testRef("codex-result", "codex-result-hash")); err != nil || !accepted {
		t.Fatal(err)
	}
	if got := countSubagentResumeOutbox(t, store, job.ID); got != 0 {
		t.Fatalf("resume outbox before continuation = %d", got)
	}
	if err := store.PauseForSubagent(ctx, agent.SubagentWaitCommit{
		TaskID: task.TaskID, RunID: task.RunID, InvocationID: task.InvocationID, DelegationID: job.ID, RoleID: "engineering_team", ProviderID: "codex",
		ArtifactID: "codex-progress-artifact", EvalID: "codex-progress-eval", DeliveryID: "codex-progress-delivery",
		ArtifactRef: testRef("codex-progress", "codex-progress-hash"), TraceRef: testRef("codex-trace", "codex-trace-hash"),
		ContinuationRef: testRef("codex-continuation", "codex-continuation-hash"),
		EvalResult:      eval.Pass, EvalFindingsRef: testRef("codex-findings", "codex-findings-hash"),
	}); err != nil {
		t.Fatal(err)
	}
	if got := countSubagentResumeOutbox(t, store, job.ID); got != 1 {
		t.Fatalf("resume outbox after continuation = %d", got)
	}
	if _, claimed, err := store.ClaimSubagentResume(ctx, job.ID, "worker", time.Minute); !errors.Is(err, agent.ErrSubagentProgressPending) || claimed {
		t.Fatalf("resume before progress delivery claimed=%v err=%v", claimed, err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE deliveries SET status = 'sent' WHERE id = ?`, "codex-progress-delivery"); err != nil {
		t.Fatal(err)
	}
	resume, claimed, err := store.ClaimSubagentResume(ctx, job.ID, "worker", time.Minute)
	if err != nil || !claimed || resume.DelegationID != job.ID || resume.RoleID != "engineering_team" || resume.ProviderID != "codex" || resume.Status != "completed" {
		t.Fatalf("resume=%+v claimed=%v err=%v", resume, claimed, err)
	}
}

func countSubagentResumeOutbox(t *testing.T, store *Store, delegationID string) int {
	t.Helper()
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM internal_outbox WHERE kind = 'subagent.resume' AND aggregate_id = ?`, delegationID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
