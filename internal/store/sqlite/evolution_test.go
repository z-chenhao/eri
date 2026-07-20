package sqlite

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/z-chenhao/eri/internal/agent"
	"github.com/z-chenhao/eri/internal/content"
	"github.com/z-chenhao/eri/internal/eval"
	"github.com/z-chenhao/eri/internal/evolution"
	"github.com/z-chenhao/eri/internal/runtime"
)

func TestEvolutionFailureClusterStartsCanaryPromotesAndRollsBack(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	key := bytes.Repeat([]byte{0x75}, 32)
	contentStore, err := content.New(filepath.Join(root, "content"), key)
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(filepath.Join(root, "metadata", "eri.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := formatTime(time.Now().UTC())
	for _, statement := range []string{
		`INSERT INTO conversations(id, created_at) VALUES('evolution-conversation', '` + now + `')`,
		`INSERT INTO tasks(id, conversation_id, source_interaction_id, source_channel, status, terminal_status, version, created_at, updated_at)
		 VALUES('evolution-task', 'evolution-conversation', 'source', 'test', 'completed', 'completed', 1, '` + now + `', '` + now + `')`,
	} {
		if _, err := store.db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	service, err := evolution.NewService(store, contentStore, evolutionProposalModel{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 6; index++ {
		if err := service.Observe(ctx, agent.EvolutionSignal{
			TaskID: "evolution-task", Result: eval.Repair, Tier: "substantive", Findings: []string{"The answer lacked source comparison."},
		}); err != nil {
			t.Fatal(err)
		}
		var proposals int
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM internal_outbox WHERE kind = 'evolution.propose'`).Scan(&proposals); err != nil {
			t.Fatal(err)
		}
		if index < evolution.MinimumProposalSignals-1 && proposals != 0 {
			t.Fatalf("proposal queued with only %d signals", index+1)
		}
		if index == evolution.MinimumProposalSignals-1 && proposals != 1 {
			t.Fatalf("proposal count=%d after %d signals", proposals, index+1)
		}
	}
	if err := service.HandlePropose(ctx, runtime.OutboxItem{Attempts: 1}); err != nil {
		t.Fatal(err)
	}
	releases, err := service.Releases(ctx, 10)
	if err != nil || len(releases) != 1 || releases[0].Status != "canary" || releases[0].Version != 1 || releases[0].HoldoutSignalCount != 2 || releases[0].TrainingSignalCount != 4 {
		t.Fatalf("canary releases=%+v err=%v", releases, err)
	}
	active := releases[0]
	canaryTask := findEvolutionRoute(t, ctx, service, active.ID, true)
	routed, found, err := service.InstructionForTask(ctx, canaryTask)
	if err != nil || !found || routed.ReleaseID != active.ID {
		t.Fatalf("canary route=%+v found=%v err=%v", routed, found, err)
	}
	baselineTask := findEvolutionRoute(t, ctx, service, active.ID, false)
	if routed, found, err := service.InstructionForTask(ctx, baselineTask); err != nil || found {
		t.Fatalf("baseline task unexpectedly received a release: %+v found=%v err=%v", routed, found, err)
	}
	for index := 0; index < 8; index++ {
		if err := service.Observe(ctx, agent.EvolutionSignal{TaskID: "evolution-task", ReleaseID: active.ID, Result: eval.Pass, Tier: "routine"}); err != nil {
			t.Fatal(err)
		}
	}
	releases, err = service.Releases(ctx, 10)
	if err != nil || len(releases) != 1 || releases[0].Status != "active" || releases[0].PassCount != 8 {
		t.Fatalf("promoted releases=%+v err=%v", releases, err)
	}
	ref, err := contentStore.Put(ctx, []byte("Check exact success criteria before answering."), content.Metadata{
		MediaType: "text/plain", EncryptionDomain: "evolution-release", PrivacyClass: "private", RetentionPolicy: "user_owned", ProvenanceRef: "candidate-2",
	})
	if err != nil {
		t.Fatal(err)
	}
	reviewRef, err := contentStore.Put(ctx, []byte(`{"decision":"pass"}`), content.Metadata{
		MediaType: "application/json", EncryptionDomain: "evolution-review", PrivacyClass: "private", RetentionPolicy: "user_owned", ProvenanceRef: "candidate-2",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, created, err := store.StartEvolutionCanary(ctx, evolution.Release{
		ID: "candidate-2", InstructionRef: ref, OfflineReviewRef: reviewRef, TrainingSignalCount: 4, HoldoutSignalCount: 2,
		OfflineScore: .86, BaselineScore: .60, CreatedAt: time.Now().UTC(),
	}, "source-2")
	if err != nil || !created {
		t.Fatalf("second canary=%+v created=%v err=%v", second, created, err)
	}
	secondCanaryTask := findEvolutionRoute(t, ctx, service, second.ID, true)
	if routed, found, err := service.InstructionForTask(ctx, secondCanaryTask); err != nil || !found || routed.ReleaseID != second.ID {
		t.Fatalf("second canary route=%+v found=%v err=%v", routed, found, err)
	}
	secondBaselineTask := findEvolutionRoute(t, ctx, service, second.ID, false)
	if routed, found, err := service.InstructionForTask(ctx, secondBaselineTask); err != nil || !found || routed.ReleaseID != active.ID {
		t.Fatalf("active fallback route=%+v found=%v err=%v", routed, found, err)
	}
	if err := service.Observe(ctx, agent.EvolutionSignal{TaskID: "evolution-task", ReleaseID: second.ID, Result: eval.Repair, Tier: "routine"}); err != nil {
		t.Fatal(err)
	}
	releases, _ = service.Releases(ctx, 10)
	if releases[0].ID != second.ID || releases[0].Status != "retired" || releases[0].FailCount != 1 || releases[1].Status != "active" {
		t.Fatalf("rollback releases=%+v", releases)
	}
}

func findEvolutionRoute(t *testing.T, ctx context.Context, service *evolution.Service, releaseID string, selected bool) string {
	t.Helper()
	for index := 0; index < 1000; index++ {
		taskID := fmt.Sprintf("route-task-%s-%d", releaseID, index)
		instruction, found, err := service.InstructionForTask(ctx, taskID)
		if err != nil {
			t.Fatal(err)
		}
		isSelected := found && instruction.ReleaseID == releaseID
		if isSelected == selected {
			return taskID
		}
	}
	t.Fatalf("could not find selected=%v task for release %s", selected, releaseID)
	return ""
}

type evolutionProposalModel struct{}

func (evolutionProposalModel) Complete(_ context.Context, request agent.ModelRequest) (agent.ModelResponse, error) {
	if strings.Contains(request.System, "independent offline gate") {
		return agent.ModelResponse{Message: agent.Message{Role: "assistant", Content: `{"decision":"pass","candidate_score":0.86,"baseline_score":0.60,"regressions":[],"safety_issues":[],"review_rationale":"Improves unseen source-comparison failures without broadening authority."}`}}, nil
	}
	return agent.ModelResponse{Message: agent.Message{Role: "assistant", Content: `{"candidates":[{"instruction":"Compare independent evidence and verify the requested success criteria before finalizing the answer.","rationale":"Targets the recurring verification gap."}]}`}}, nil
}
