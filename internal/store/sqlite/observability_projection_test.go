package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/z-chenhao/eri/internal/policy"
	"github.com/z-chenhao/eri/internal/tool"
)

func TestLoadRunProjectsExplicitEffectParent(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "eri.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sent, err := store.CreateInbound(ctx, "web", testRef("observability-parent", "observability-parent-hash"), nil)
	if err != nil {
		t.Fatal(err)
	}
	task, claimed, err := store.ClaimTask(ctx, sent.TaskID, "worker", time.Minute, "soul", `{}`, "test:model")
	if err != nil || !claimed {
		t.Fatalf("task=%+v claimed=%v err=%v", task, claimed, err)
	}
	now := time.Now().UTC()
	parent := tool.Intent{
		ID: "delegate-intent", TaskID: task.TaskID, RunID: task.RunID, ToolID: "builtin.delegate", ToolVersion: "1.0.0",
		Effect: policy.ReadOnly, Target: "subagent:native:read_only", ParametersHash: "delegate-parameters", IdempotencyKey: "delegate-key",
		Control: policy.Auto, ReconciliationStrategy: "replay", Status: tool.IntentPlanned, CreatedAt: now, UpdatedAt: now,
	}
	if _, created, err := store.PlanIntent(ctx, parent); err != nil || !created {
		t.Fatalf("parent created=%v err=%v", created, err)
	}
	child := tool.Intent{
		ID: "child-intent", TaskID: task.TaskID, RunID: task.RunID, ParentIntentID: parent.ID, ToolID: "builtin.web", ToolVersion: "1.0.0",
		Effect: policy.ReadOnly, Target: "research", ParametersHash: "child-parameters", IdempotencyKey: "child-key",
		Control: policy.Auto, ReconciliationStrategy: "replay", Status: tool.IntentPlanned, CreatedAt: now, UpdatedAt: now,
	}
	if _, created, err := store.PlanIntent(ctx, child); err != nil || !created {
		t.Fatalf("child created=%v err=%v", created, err)
	}
	detail, found, err := store.LoadRun(ctx, task.RunID)
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	for _, effect := range detail.Effects {
		if effect.ID == child.ID {
			if effect.ParentIntentID != parent.ID {
				t.Fatalf("parent_intent_id=%q", effect.ParentIntentID)
			}
			return
		}
	}
	t.Fatal("Child Tool effect was not projected")
}
