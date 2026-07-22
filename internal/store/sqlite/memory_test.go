package sqlite

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/z-chenhao/eri/internal/content"
	"github.com/z-chenhao/eri/internal/memory"
	"github.com/z-chenhao/eri/internal/policy"
	"github.com/z-chenhao/eri/internal/tool"
	"github.com/z-chenhao/eri/internal/tool/builtin"
)

type semanticTestEncoder struct{}

type failOnceDeleteContentStore struct {
	*content.Store
	fail bool
}

func (s *failOnceDeleteContentStore) Delete(ctx context.Context, ref content.Ref) error {
	if s.fail {
		s.fail = false
		return errors.New("simulated physical cleanup failure")
	}
	return s.Store.Delete(ctx, ref)
}

func (semanticTestEncoder) ID() string { return "test:semantic-v1" }

func (semanticTestEncoder) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	result := make([][]float32, 0, len(inputs))
	for _, input := range inputs {
		if strings.Contains(strings.ToLower(input), "dawn") || strings.Contains(strings.ToLower(input), "comfortable departure") {
			result = append(result, []float32{1, 0})
		} else {
			result = append(result, []float32{0, 1})
		}
	}
	return result, nil
}

func TestMemoryHybridRecallFindsSemanticParaphraseAndDeletesEncryptedIndex(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	key := bytes.Repeat([]byte{0x61}, 32)
	contentStore, err := content.New(filepath.Join(root, "content"), key)
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(filepath.Join(root, "metadata", "eri.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := memory.NewService(store, contentStore, key, memory.Options{SemanticEncoder: semanticTestEncoder{}, SemanticIndex: store})
	if err != nil {
		t.Fatal(err)
	}
	entry, err := service.Capture(ctx, memory.CaptureRequest{
		Statement: "The user avoids dawn flights", Kind: "preference", Scope: "travel",
		Relation: memory.Supports, SourceType: "user", SourceRef: "interaction:flight",
		IndependenceGroup: "user:self", DirectUserStatement: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var encodedRef string
	if err := store.db.QueryRow(`SELECT vector_ref_json FROM memory_semantic_index WHERE memory_id = ?`, entry.MemoryID).Scan(&encodedRef); err != nil {
		t.Fatal(err)
	}
	var vectorRef content.Ref
	if err := json.Unmarshal([]byte(encodedRef), &vectorRef); err != nil {
		t.Fatal(err)
	}
	recalled, err := service.Retrieve(ctx, "comfortable departure time", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(recalled.Entries) != 1 || recalled.Entries[0].MemoryID != entry.MemoryID || recalled.Entries[0].SemanticScore < .99 || !slices.Contains(recalled.Entries[0].RecallReasons, "semantic_match") {
		t.Fatalf("semantic recall=%+v", recalled)
	}
	if _, err := service.Delete(ctx, entry.MemoryID); err != nil {
		t.Fatal(err)
	}
	if _, err := contentStore.Get(ctx, vectorRef); !errors.Is(err, content.ErrDeleted) {
		t.Fatalf("semantic vector survived governed deletion: %v", err)
	}
}

func TestMemoryForgetDeletesOnlyGatewayResultsThatDisclosedTheMemory(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	key := bytes.Repeat([]byte{0x6f}, 32)
	contentStore, err := content.New(filepath.Join(root, "content"), key)
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(filepath.Join(root, "metadata", "eri.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := memory.NewService(store, contentStore, key)
	if err != nil {
		t.Fatal(err)
	}
	memoryTool, err := builtin.NewMemory(service, contentStore)
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := tool.NewGateway(store, contentStore, memoryTool)
	if err != nil {
		t.Fatal(err)
	}

	forgotten, err := service.Capture(ctx, memory.CaptureRequest{
		Statement: "I prefer the unique azimuth reading chair.", Kind: "preference", Scope: "reading",
		Relation: memory.Supports, SourceType: "user", SourceRef: "interaction:memory-source",
		IndependenceGroup: "user:self:chair", DirectUserStatement: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	retained, err := service.Capture(ctx, memory.CaptureRequest{
		Statement: "I prefer the unique quartz writing desk.", Kind: "preference", Scope: "writing",
		Relation: memory.Supports, SourceType: "user", SourceRef: "interaction:memory-source",
		IndependenceGroup: "user:self:desk", DirectUserStatement: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := formatTime(time.Now().UTC())
	for _, statement := range []string{
		`INSERT INTO conversations(id, created_at) VALUES('gateway-memory-conversation', '` + now + `')`,
		`INSERT INTO tasks(id, conversation_id, source_interaction_id, source_channel, status, terminal_status, version, created_at, updated_at)
		 VALUES('gateway-memory-task', 'gateway-memory-conversation', 'gateway-memory-source', 'test', 'running', '', 1, '` + now + `', '` + now + `')`,
		`INSERT INTO interactions(id, conversation_id, task_id, direction, role, kind, channel, content_ref_json, created_at)
		 VALUES('gateway-memory-source', 'gateway-memory-conversation', 'gateway-memory-task', 'inbound', 'user', 'text', 'test', '{}', '` + now + `')`,
		`INSERT INTO runs(id, task_id, status, model_status, soul_version, target, context_manifest_json, started_at, updated_at)
		 VALUES('gateway-memory-run', 'gateway-memory-task', 'active', 'dispatched', 'soul', 'test', '{}', '` + now + `', '` + now + `')`,
	} {
		if _, err := store.db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}

	invokeRead := func(name, input string) (tool.Outcome, memory.Bundle) {
		t.Helper()
		outcome, err := gateway.Invoke(ctx, tool.Request{
			TaskID: "gateway-memory-task", RunID: "gateway-memory-run", InvocationID: "model-" + name,
			SourceInteractionID: "gateway-memory-source", SourceInteractionRole: "user", SourceInteractionKind: "text",
			ToolCallID: "call-" + name, ToolID: "builtin.memory", Input: json.RawMessage(input),
		})
		if err != nil {
			t.Fatal(err)
		}
		if outcome.ApprovalRequired || outcome.Intent.Status != tool.IntentConfirmed {
			t.Fatalf("%s outcome=%+v", name, outcome)
		}
		var bundle memory.Bundle
		if err := json.Unmarshal(outcome.Result.Output, &bundle); err != nil {
			t.Fatal(err)
		}
		if bundle.RetrievalID != outcome.Intent.ID {
			t.Fatalf("%s retrieval=%q intent=%q", name, bundle.RetrievalID, outcome.Intent.ID)
		}
		return outcome, bundle
	}
	forgottenSearch, _ := invokeRead("forgotten-search", `{"operation":"search","query":"azimuth","limit":1}`)
	list, _ := invokeRead("list", `{"operation":"list","limit":10}`)
	retainedSearch, _ := invokeRead("retained-search", `{"operation":"search","query":"quartz","limit":1}`)
	for name, outcome := range map[string]tool.Outcome{"forgotten search": forgottenSearch, "list": list} {
		encoded, err := contentStore.Get(ctx, outcome.Intent.ResultRef)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(encoded, []byte(forgotten.Statement)) {
			t.Fatalf("%s did not contain the Memory statement", name)
		}
	}
	retainedEncoded, err := contentStore.Get(ctx, retainedSearch.Intent.ResultRef)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(retainedEncoded, []byte(retained.Statement)) || bytes.Contains(retainedEncoded, []byte(forgotten.Statement)) {
		t.Fatalf("unrelated search result=%s", retainedEncoded)
	}

	forgetInput := json.RawMessage(`{"operation":"forget","memory_id":"` + forgotten.MemoryID + `"}`)
	forgetRequest := tool.Request{
		TaskID: "gateway-memory-task", RunID: "gateway-memory-run", InvocationID: "model-forget",
		SourceInteractionID: "gateway-memory-source", SourceInteractionRole: "user", SourceInteractionKind: "text",
		ToolCallID: "call-forget", ToolID: "builtin.memory", Input: forgetInput,
	}
	planned, err := gateway.Invoke(ctx, forgetRequest)
	if err != nil {
		t.Fatal(err)
	}
	if !planned.ApprovalRequired || planned.Intent.Status != tool.IntentPlanned {
		t.Fatalf("forget approval outcome=%+v", planned)
	}
	forgetRequest.Grant = &tool.Grant{
		ID: "gateway-memory-grant", ApprovalID: "gateway-memory-approval", TaskID: planned.Intent.TaskID,
		ToolID: planned.Intent.ToolID, ToolVersion: planned.Intent.ToolVersion, Effect: planned.Intent.Effect,
		Target: planned.Intent.Target, ParametersHash: planned.Intent.ParametersHash,
		Control: policy.StrongApproval, ExpiresAt: time.Now().UTC().Add(time.Minute),
	}
	forgot, err := gateway.Invoke(ctx, forgetRequest)
	if err != nil {
		t.Fatal(err)
	}
	if forgot.Intent.Status != tool.IntentConfirmed {
		t.Fatalf("forget outcome=%+v", forgot)
	}
	var deletion memory.DeletePlan
	if err := json.Unmarshal(forgot.Result.Output, &deletion); err != nil {
		t.Fatal(err)
	}
	if deletion.Affected["tool_results"] != 2 {
		t.Fatalf("deleted tool results=%d, want search and list", deletion.Affected["tool_results"])
	}

	for name, outcome := range map[string]tool.Outcome{"forgotten search": forgottenSearch, "list": list} {
		loaded, found, err := store.LoadIntentByID(ctx, outcome.Intent.ID)
		if err != nil || !found {
			t.Fatalf("load %s intent found=%v err=%v", name, found, err)
		}
		if loaded.ResultRef.ObjectID != "" {
			t.Fatalf("%s result_ref still points at forgotten ciphertext: %+v", name, loaded.ResultRef)
		}
		if _, err := contentStore.Get(ctx, outcome.Intent.ResultRef); !errors.Is(err, content.ErrDeleted) {
			t.Fatalf("%s ciphertext survived forget: %v", name, err)
		}
	}
	loadedRetained, found, err := store.LoadIntentByID(ctx, retainedSearch.Intent.ID)
	if err != nil || !found {
		t.Fatalf("load retained intent found=%v err=%v", found, err)
	}
	if loadedRetained.ResultRef.ObjectID == "" {
		t.Fatal("unrelated Memory result_ref was cleared")
	}
	if _, err := contentStore.Get(ctx, retainedSearch.Intent.ResultRef); err != nil {
		t.Fatalf("unrelated Memory ciphertext was deleted: %v", err)
	}
}

func TestMemoryForgetWaitsForDispatchedGatewayRead(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	key := bytes.Repeat([]byte{0x6e}, 32)
	contentStore, err := content.New(filepath.Join(root, "content"), key)
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(filepath.Join(root, "metadata", "eri.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := memory.NewService(store, contentStore, key)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := service.Capture(ctx, memory.CaptureRequest{
		Statement: "I prefer the in-flight result to remain governed.", Kind: "preference",
		Relation: memory.Supports, SourceType: "user", SourceRef: "interaction:race-source",
		IndependenceGroup: "user:self:race", DirectUserStatement: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	encodedNow := formatTime(now)
	for _, statement := range []string{
		`INSERT INTO conversations(id, created_at) VALUES('race-memory-conversation', '` + encodedNow + `')`,
		`INSERT INTO tasks(id, conversation_id, source_interaction_id, source_channel, status, terminal_status, version, created_at, updated_at)
		 VALUES('race-memory-task', 'race-memory-conversation', 'race-memory-source', 'test', 'running', '', 1, '` + encodedNow + `', '` + encodedNow + `')`,
		`INSERT INTO interactions(id, conversation_id, task_id, direction, role, kind, channel, content_ref_json, created_at)
		 VALUES('race-memory-source', 'race-memory-conversation', 'race-memory-task', 'inbound', 'user', 'text', 'test', '{}', '` + encodedNow + `')`,
		`INSERT INTO runs(id, task_id, status, model_status, soul_version, target, context_manifest_json, started_at, updated_at)
		 VALUES('race-memory-run', 'race-memory-task', 'active', 'dispatched', 'soul', 'test', '{}', '` + encodedNow + `', '` + encodedNow + `')`,
	} {
		if _, err := store.db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	intent, created, err := store.PlanIntent(ctx, tool.Intent{
		ID: "race-memory-intent", TaskID: "race-memory-task", RunID: "race-memory-run",
		InvocationID: "race-model-invocation", ToolCallID: "race-tool-call", ToolID: "builtin.memory", ToolVersion: "0.2.0",
		Effect: policy.ReadOnly, Target: "memory", ParametersHash: "race-parameters", IdempotencyKey: "race-idempotency",
		Control: policy.Auto, ReconciliationStrategy: "inspect_memory_state", Status: tool.IntentPlanned,
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil || !created {
		t.Fatalf("plan active read created=%v err=%v", created, err)
	}
	if err := store.TransitionIntent(ctx, intent.ID, tool.IntentPlanned, tool.IntentAuthorized, "", "", "", content.Ref{}); err != nil {
		t.Fatal(err)
	}
	if err := store.TransitionIntent(ctx, intent.ID, tool.IntentAuthorized, tool.IntentDispatched, "", "", "", content.Ref{}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordMemoryRetrieval(ctx, memory.RetrievalRecord{
		ID: intent.ID, RunID: intent.RunID, SourceInteractionID: "race-memory-source", QueryKey: "race-query", CreatedAt: now,
		Items: []memory.RetrievalItem{{MemoryID: entry.MemoryID, Rank: 1, Injected: true}},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := service.Delete(ctx, entry.MemoryID); err == nil || !strings.Contains(err.Error(), "still in progress") {
		t.Fatalf("forget did not wait for dispatched read: %v", err)
	}
	inspected, err := service.Inspect(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(inspected.Entries) != 1 || inspected.Entries[0].MemoryID != entry.MemoryID {
		t.Fatalf("aborted forget mutated Memory: %+v", inspected)
	}
	if _, err := contentStore.Get(ctx, entry.StatementRef); err != nil {
		t.Fatalf("aborted forget deleted statement ciphertext: %v", err)
	}
	var jobs int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM memory_delete_jobs WHERE memory_id = ?`, entry.MemoryID).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 0 {
		t.Fatalf("aborted forget persisted %d delete jobs", jobs)
	}
}

func TestMemoryConflictSourceIndependenceRecoveryAndDelete(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	key := bytes.Repeat([]byte{0x71}, 32)
	contentStore, err := content.New(filepath.Join(root, "content"), key)
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(filepath.Join(root, "metadata", "eri.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := memory.NewService(store, contentStore, key)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := service.Capture(ctx, memory.CaptureRequest{
		Statement: "The user prefers hotel rooms with a window", Kind: "preference", Scope: "travel",
		Relation: memory.Supports, SourceType: "user", SourceRef: "interaction:user-1",
		IndependenceGroup: "user:self", ExplicitUserMemory: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if initial.Status != memory.Supported {
		t.Fatalf("initial belief = %+v", initial)
	}
	for _, source := range []string{"article-copy-1", "article-copy-2"} {
		if _, err := service.Capture(ctx, memory.CaptureRequest{
			ClaimID: initial.ClaimID, Statement: initial.Statement, Evidence: "One travel record shows the user selected an interior room",
			Kind: "preference", Scope: "travel", Relation: memory.Contradicts,
			SourceType: "web", SourceRef: source, IndependenceGroup: "syndicated:one-origin",
			Reliability: .9, Directness: .8, Verifiability: .9,
		}); err != nil {
			t.Fatal(err)
		}
	}
	inspected, err := service.Inspect(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(inspected.Entries) != 1 || inspected.Entries[0].IndependentGroups != 2 {
		t.Fatalf("same-origin evidence was counted more than once: %+v", inspected)
	}
	if _, err := service.Capture(ctx, memory.CaptureRequest{
		ClaimID: initial.ClaimID, Statement: initial.Statement, Evidence: "The user explicitly confirmed that future rooms should still have a window",
		Kind: "preference", Scope: "travel", Relation: memory.Supports,
		SourceType: "user", SourceRef: "interaction:user-2", IndependenceGroup: "user:confirmation-2",
		Reliability: 1, Directness: 1, Verifiability: 1,
	}); err != nil {
		t.Fatal(err)
	}
	retrieved, err := service.Retrieve(ctx, "travel hotel window", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(retrieved.Entries) != 1 || retrieved.Entries[0].Status != memory.Supported || retrieved.Entries[0].ContradictWeight == 0 {
		t.Fatalf("strong fact did not recover while preserving conflict: %+v", retrieved)
	}
	if err := service.SetUsagePolicy(ctx, initial.MemoryID, "do_not_use"); err != nil {
		t.Fatal(err)
	}
	retrieved, err = service.Retrieve(ctx, "hotel", 10)
	if err != nil || len(retrieved.Entries) != 0 {
		t.Fatalf("do_not_use memory was retrieved: %+v err=%v", retrieved, err)
	}
	now := formatTime(time.Now().UTC())
	for _, statement := range []string{
		`INSERT INTO conversations(id, created_at) VALUES('memory-delete-conversation', '` + now + `')`,
		`INSERT INTO tasks(id, conversation_id, source_interaction_id, source_channel, status, terminal_status, version, created_at, updated_at)
		 VALUES('memory-delete-task', 'memory-delete-conversation', 'source', 'test', 'completed', 'completed', 1, '` + now + `', '` + now + `')`,
		`INSERT INTO runs(id, task_id, status, model_status, soul_version, target, context_manifest_json, started_at, updated_at, ended_at)
		 VALUES('memory-delete-run', 'memory-delete-task', 'succeeded', 'succeeded', 'soul', 'test', '{"memory":["` + initial.MemoryID + `"]}', '` + now + `', '` + now + `', '` + now + `')`,
		`INSERT INTO episodes(id, task_id, manifest_ref_json, status, created_at)
		 VALUES('memory-delete-episode', 'memory-delete-task', '{}', 'ready', '` + now + `')`,
		`INSERT INTO dataset_candidates(id, episode_id, status, created_at)
		 VALUES('memory-delete-dataset', 'memory-delete-episode', 'candidate', '` + now + `')`,
		`INSERT INTO dataset_snapshots(id, version, purpose, manifest_ref_json, status, item_count, created_at)
		 VALUES('memory-delete-snapshot', 1, 'eval', '{}', 'frozen', 1, '` + now + `')`,
		`INSERT INTO dataset_snapshot_items(snapshot_id, candidate_id, episode_id, task_id, split)
		 VALUES('memory-delete-snapshot', 'memory-delete-dataset', 'memory-delete-episode', 'memory-delete-task', 'eval')`,
	} {
		if _, err := store.db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := service.Delete(ctx, initial.MemoryID)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Affected["episodes_invalidated"] != 1 || plan.Affected["dataset_candidates_invalidated"] != 1 || plan.Affected["dataset_snapshots_invalidated"] != 1 {
		t.Fatalf("derived deletion plan = %+v", plan.Affected)
	}
	var episodeStatus, datasetStatus, snapshotStatus string
	if err := store.db.QueryRow(`SELECT status FROM episodes WHERE id = 'memory-delete-episode'`).Scan(&episodeStatus); err != nil || episodeStatus != "invalidated" {
		t.Fatalf("episode status=%q err=%v", episodeStatus, err)
	}
	if err := store.db.QueryRow(`SELECT status FROM dataset_candidates WHERE id = 'memory-delete-dataset'`).Scan(&datasetStatus); err != nil || datasetStatus != "invalidated" {
		t.Fatalf("dataset status=%q err=%v", datasetStatus, err)
	}
	if err := store.db.QueryRow(`SELECT status FROM dataset_snapshots WHERE id = 'memory-delete-snapshot'`).Scan(&snapshotStatus); err != nil || snapshotStatus != "invalidated" {
		t.Fatalf("snapshot status=%q err=%v", snapshotStatus, err)
	}
	inspected, err = service.Inspect(ctx, 10)
	if err != nil || len(inspected.Entries) != 0 {
		t.Fatalf("deleted memory remains: %+v err=%v", inspected, err)
	}
	needle := []byte(initial.Statement)
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(body, needle) {
			t.Fatalf("memory plaintext found in %s", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryReplacementCreatesNewCurrentFactAndPreservesRevisionLineage(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	key := bytes.Repeat([]byte{0x76}, 32)
	contentStore, err := content.New(filepath.Join(root, "content"), key)
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(filepath.Join(root, "metadata", "eri.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := memory.NewService(store, contentStore, key)
	if err != nil {
		t.Fatal(err)
	}

	initial, err := service.Capture(ctx, memory.CaptureRequest{
		Statement: "I prefer hotel rooms with a window.", Kind: "preference", Scope: "travel",
		Relation: memory.Supports, SourceType: "user", SourceRef: "interaction:preference-original",
		IndependenceGroup: "user:self", DirectUserStatement: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Capture(ctx, memory.CaptureRequest{
		ReplacesMemoryID: "missing-memory", Statement: "I prefer a higher floor.", Kind: "preference", Scope: "travel",
		Relation: memory.Supports, SourceType: "user", SourceRef: "interaction:missing-replacement",
		IndependenceGroup: "user:self", DirectUserStatement: true,
	}); err == nil {
		t.Fatal("replacement with an unknown predecessor was accepted")
	}
	replacementRequest := memory.CaptureRequest{
		ReplacesMemoryID: initial.MemoryID, Statement: "I now prefer rooms near the lift.", Kind: "preference", Scope: "travel",
		Relation: memory.Supports, SourceType: "user", SourceRef: "interaction:preference-correction",
		IndependenceGroup: "user:self", DirectUserStatement: true,
	}
	replacement, err := service.Capture(ctx, replacementRequest)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.ClaimID == initial.ClaimID || replacement.MemoryID == initial.MemoryID || replacement.ReplacesMemoryID != initial.MemoryID {
		t.Fatalf("replacement did not create a new immutable claim: initial=%+v replacement=%+v", initial, replacement)
	}

	recalled, err := service.Retrieve(ctx, "lift", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recalled.Entries) != 1 || recalled.Entries[0].MemoryID != replacement.MemoryID || recalled.Entries[0].Statement != replacement.Statement || recalled.Entries[0].Status != memory.Supported {
		t.Fatalf("replacement was not the current recalled fact: %+v", recalled)
	}
	if stale, err := service.Retrieve(ctx, "window", 10); err != nil || len(stale.Entries) != 0 {
		t.Fatalf("archived fact remained recallable: %+v err=%v", stale, err)
	}

	var oldStatus, parentID string
	if err := store.db.QueryRow(`SELECT status FROM memory_items WHERE id = ?`, initial.MemoryID).Scan(&oldStatus); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT replaces_memory_id FROM memory_items WHERE id = ?`, replacement.MemoryID).Scan(&parentID); err != nil {
		t.Fatal(err)
	}
	if oldStatus != "archived" || parentID != initial.MemoryID {
		t.Fatalf("revision lineage old_status=%q parent=%q", oldStatus, parentID)
	}
	var salienceBefore float64
	var updatedBefore string
	var beliefVersionBefore, eventsBefore int
	if err := store.db.QueryRow(`
		SELECT m.salience, m.updated_at, b.version
		FROM memory_items m JOIN memory_beliefs b ON b.claim_id = m.claim_id
		WHERE m.id = ?`, replacement.MemoryID).Scan(&salienceBefore, &updatedBefore, &beliefVersionBefore); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`
		SELECT COUNT(*) FROM events
		WHERE aggregate_type = 'memory' AND aggregate_id IN (?, ?)`, initial.MemoryID, replacement.MemoryID).Scan(&eventsBefore); err != nil {
		t.Fatal(err)
	}
	replayed, err := service.Capture(ctx, replacementRequest)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.MemoryID != replacement.MemoryID || replayed.ClaimID != replacement.ClaimID {
		t.Fatalf("idempotent replacement replay created a new revision: first=%+v replay=%+v", replacement, replayed)
	}
	var salienceAfter float64
	var updatedAfter string
	var beliefVersionAfter, eventsAfter int
	if err := store.db.QueryRow(`
		SELECT m.salience, m.updated_at, b.version
		FROM memory_items m JOIN memory_beliefs b ON b.claim_id = m.claim_id
		WHERE m.id = ?`, replacement.MemoryID).Scan(&salienceAfter, &updatedAfter, &beliefVersionAfter); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`
		SELECT COUNT(*) FROM events
		WHERE aggregate_type = 'memory' AND aggregate_id IN (?, ?)`, initial.MemoryID, replacement.MemoryID).Scan(&eventsAfter); err != nil {
		t.Fatal(err)
	}
	if salienceAfter != salienceBefore || updatedAfter != updatedBefore || beliefVersionAfter != beliefVersionBefore || eventsAfter != eventsBefore {
		t.Fatalf("replacement retry mutated state: salience %f->%f updated %q->%q belief_version %d->%d events %d->%d",
			salienceBefore, salienceAfter, updatedBefore, updatedAfter, beliefVersionBefore, beliefVersionAfter, eventsBefore, eventsAfter)
	}
	var revisionEvents int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM events WHERE type = 'memory.revision_created' AND aggregate_id = ?`, replacement.MemoryID).Scan(&revisionEvents); err != nil {
		t.Fatal(err)
	}
	if revisionEvents != 1 {
		t.Fatalf("revision event count=%d, want 1", revisionEvents)
	}
	if _, err := service.Capture(ctx, memory.CaptureRequest{
		ReplacesMemoryID: initial.MemoryID, Statement: "I prefer a room near the stairs.", Kind: "preference", Scope: "travel",
		Relation: memory.Supports, SourceType: "user", SourceRef: "interaction:forked-replacement",
		IndependenceGroup: "user:self", DirectUserStatement: true,
	}); err == nil {
		t.Fatal("archived memory was allowed to acquire a second replacement")
	}
	var correctionRefJSON string
	if err := store.db.QueryRow(`
		SELECT evidence_ref_json FROM memory_evidence
		WHERE claim_id = ? AND relation = 'supports' AND source_ref = 'interaction:preference-correction'`, replacement.ClaimID).Scan(&correctionRefJSON); err != nil {
		t.Fatal(err)
	}
	var correctionRef content.Ref
	if err := json.Unmarshal([]byte(correctionRefJSON), &correctionRef); err != nil {
		t.Fatal(err)
	}
	correctionBody, err := contentStore.Get(ctx, correctionRef)
	if err != nil {
		t.Fatal(err)
	}
	if string(correctionBody) != "I now prefer rooms near the lift." {
		t.Fatalf("correction evidence=%q", correctionBody)
	}

	plan, err := service.Delete(ctx, replacement.MemoryID)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Affected["evidence"] != 1 || len(plan.ContentRefs) != 2 {
		t.Fatalf("replacement deletion lineage=%+v refs=%d, want its evidence and claim", plan.Affected, len(plan.ContentRefs))
	}
	for _, ref := range plan.ContentRefs {
		if _, err := contentStore.Get(ctx, ref); !errors.Is(err, content.ErrDeleted) {
			t.Fatalf("memory lineage content survived deletion: ref=%+v err=%v", ref, err)
		}
	}
	if _, err := contentStore.Get(ctx, initial.StatementRef); err != nil {
		t.Fatalf("deleting replacement removed the archived source claim: %v", err)
	}
	if err := store.db.QueryRow(`SELECT status FROM memory_items WHERE id = ?`, initial.MemoryID).Scan(&oldStatus); err != nil {
		t.Fatal(err)
	}
	if oldStatus != "archived" {
		t.Fatalf("deleting replacement revived old memory with status %q", oldStatus)
	}
	if stale, err := service.Retrieve(ctx, "window", 10); err != nil || len(stale.Entries) != 0 {
		t.Fatalf("deleting replacement revived archived fact: %+v err=%v", stale, err)
	}
	if _, err := service.Capture(ctx, memory.CaptureRequest{
		ReplacesMemoryID: replacement.MemoryID, Statement: "I prefer a room by the stairs.", Kind: "preference", Scope: "travel",
		Relation: memory.Supports, SourceType: "user", SourceRef: "interaction:replace-deleted",
		IndependenceGroup: "user:self", DirectUserStatement: true,
	}); err == nil {
		t.Fatal("deleted memory was allowed to become a replacement source")
	}
}

func TestMemoryReplacementSupportsAtoBtoAWithoutReusingArchivedClaim(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	key := bytes.Repeat([]byte{0x7a}, 32)
	contentStore, err := content.New(filepath.Join(root, "content"), key)
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(filepath.Join(root, "metadata", "eri.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := memory.NewService(store, contentStore, key)
	if err != nil {
		t.Fatal(err)
	}

	first, err := service.Capture(ctx, memory.CaptureRequest{
		Statement: "I prefer a window seat.", Kind: "preference", Scope: "travel",
		Relation: memory.Supports, SourceType: "user", SourceRef: "interaction:seat-a1",
		IndependenceGroup: "user:self", DirectUserStatement: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Capture(ctx, memory.CaptureRequest{
		ReplacesMemoryID: first.MemoryID, Statement: "I prefer an aisle seat.", Kind: "preference", Scope: "travel",
		Relation: memory.Supports, SourceType: "user", SourceRef: "interaction:seat-b",
		IndependenceGroup: "user:self", DirectUserStatement: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Capture(ctx, memory.CaptureRequest{
		Statement: first.Statement, Kind: "preference", Scope: "travel",
		Relation: memory.Supports, SourceType: "user", SourceRef: "interaction:seat-a-plain-after-b",
		IndependenceGroup: "user:self", DirectUserStatement: true,
	}); err == nil {
		t.Fatal("plain evidence reactivated A after A was replaced by B")
	}
	third, err := service.Capture(ctx, memory.CaptureRequest{
		ReplacesMemoryID: second.MemoryID, Statement: first.Statement, Kind: "preference", Scope: "travel",
		Relation: memory.Supports, SourceType: "user", SourceRef: "interaction:seat-a2",
		IndependenceGroup: "user:self", DirectUserStatement: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if third.MemoryID == first.MemoryID || third.ClaimID == first.ClaimID || third.MemoryID == second.MemoryID || third.ClaimID == second.ClaimID {
		t.Fatalf("A->B->A reused an earlier immutable revision: first=%+v second=%+v third=%+v", first, second, third)
	}
	if third.ReplacesMemoryID != second.MemoryID {
		t.Fatalf("A->B->A parent=%q, want %q", third.ReplacesMemoryID, second.MemoryID)
	}

	var firstStatus, secondStatus, thirdStatus, thirdParent string
	if err := store.db.QueryRow(`SELECT status FROM memory_items WHERE id = ?`, first.MemoryID).Scan(&firstStatus); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT status FROM memory_items WHERE id = ?`, second.MemoryID).Scan(&secondStatus); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT status, replaces_memory_id FROM memory_items WHERE id = ?`, third.MemoryID).Scan(&thirdStatus, &thirdParent); err != nil {
		t.Fatal(err)
	}
	if firstStatus != "archived" || secondStatus != "archived" || thirdStatus != "active" || thirdParent != second.MemoryID {
		t.Fatalf("A->B->A lifecycle first=%q second=%q third=%q parent=%q", firstStatus, secondStatus, thirdStatus, thirdParent)
	}
	if _, err := service.Capture(ctx, memory.CaptureRequest{
		ReplacesMemoryID: first.MemoryID, Statement: second.Statement, Kind: "preference", Scope: "travel",
		Relation: memory.Supports, SourceType: "user", SourceRef: "interaction:seat-b-replayed-after-a2",
		IndependenceGroup: "user:self", DirectUserStatement: true,
	}); err == nil {
		t.Fatal("a superseded B revision was reactivated after A->B->A")
	}
	if err := store.db.QueryRow(`SELECT status FROM memory_items WHERE id = ?`, second.MemoryID).Scan(&secondStatus); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT status FROM memory_items WHERE id = ?`, third.MemoryID).Scan(&thirdStatus); err != nil {
		t.Fatal(err)
	}
	if secondStatus != "archived" || thirdStatus != "active" {
		t.Fatalf("replayed old revision changed lifecycle second=%q third=%q", secondStatus, thirdStatus)
	}
	recalled, err := service.Retrieve(ctx, "window seat", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recalled.Entries) != 1 || recalled.Entries[0].MemoryID != third.MemoryID {
		t.Fatalf("recall did not return only the current A revision: %+v", recalled)
	}
}

func TestMemoryReplacementDoesNotReuseSameTextFromAnotherScope(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	key := bytes.Repeat([]byte{0x7b}, 32)
	contentStore, err := content.New(filepath.Join(root, "content"), key)
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(filepath.Join(root, "metadata", "eri.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := memory.NewService(store, contentStore, key)
	if err != nil {
		t.Fatal(err)
	}

	existing, err := service.Capture(ctx, memory.CaptureRequest{
		Statement: "Keep the plan concise.", Kind: "preference", Scope: "travel",
		Relation: memory.Supports, SourceType: "user", SourceRef: "interaction:travel-concise",
		IndependenceGroup: "user:self", DirectUserStatement: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	predecessor, err := service.Capture(ctx, memory.CaptureRequest{
		Statement: "Keep the project plan detailed.", Kind: "preference", Scope: "work",
		Relation: memory.Supports, SourceType: "user", SourceRef: "interaction:work-detailed",
		IndependenceGroup: "user:self", DirectUserStatement: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := service.Capture(ctx, memory.CaptureRequest{
		ReplacesMemoryID: predecessor.MemoryID, Statement: existing.Statement, Kind: "preference", Scope: "work",
		Relation: memory.Supports, SourceType: "user", SourceRef: "interaction:work-concise",
		IndependenceGroup: "user:self", DirectUserStatement: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if replacement.MemoryID == existing.MemoryID || replacement.ClaimID == existing.ClaimID {
		t.Fatalf("replacement reused same-text Claim from another scope: existing=%+v replacement=%+v", existing, replacement)
	}
	if replacement.Scope != "work" || replacement.ReplacesMemoryID != predecessor.MemoryID {
		t.Fatalf("replacement scope/lineage = %+v", replacement)
	}
	recalled, err := service.Retrieve(ctx, "plan concise", 10)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, entry := range recalled.Entries {
		ids[entry.MemoryID] = true
	}
	if len(recalled.Entries) != 2 || !ids[existing.MemoryID] || !ids[replacement.MemoryID] {
		t.Fatalf("same-text scoped memories were not independently recallable: %+v", recalled)
	}
}

func TestDeletingPredecessorDetachesAndPreservesCurrentSuccessor(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	key := bytes.Repeat([]byte{0x7c}, 32)
	contentStore, err := content.New(filepath.Join(root, "content"), key)
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(filepath.Join(root, "metadata", "eri.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := memory.NewService(store, contentStore, key)
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.Capture(ctx, memory.CaptureRequest{
		Statement: "I prefer early flights.", Kind: "preference", Scope: "travel",
		Relation: memory.Supports, SourceType: "user", SourceRef: "interaction:flight-a",
		IndependenceGroup: "user:self", DirectUserStatement: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Capture(ctx, memory.CaptureRequest{
		ReplacesMemoryID: first.MemoryID, Statement: "I prefer late flights.", Kind: "preference", Scope: "travel",
		Relation: memory.Supports, SourceType: "user", SourceRef: "interaction:flight-b",
		IndependenceGroup: "user:self", DirectUserStatement: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Delete(ctx, first.MemoryID); err != nil {
		t.Fatal(err)
	}
	var parent, status string
	if err := store.db.QueryRow(`SELECT COALESCE(replaces_memory_id, ''), status FROM memory_items WHERE id = ?`, second.MemoryID).Scan(&parent, &status); err != nil {
		t.Fatal(err)
	}
	if parent != "" || status != "active" {
		t.Fatalf("successor parent=%q status=%q after predecessor deletion", parent, status)
	}
	third, err := service.Capture(ctx, memory.CaptureRequest{
		ReplacesMemoryID: second.MemoryID, Statement: "I prefer midday flights.", Kind: "preference", Scope: "travel",
		Relation: memory.Supports, SourceType: "user", SourceRef: "interaction:flight-c",
		IndependenceGroup: "user:self", DirectUserStatement: true,
	})
	if err != nil {
		t.Fatalf("current successor could not be revised after predecessor deletion: %v", err)
	}
	if third.ReplacesMemoryID != second.MemoryID {
		t.Fatalf("third revision=%+v", third)
	}
}

func TestMemoryInspectIsSideEffectFree(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	key := bytes.Repeat([]byte{0x7d}, 32)
	contentStore, err := content.New(filepath.Join(root, "content"), key)
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(filepath.Join(root, "metadata", "eri.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := memory.NewService(store, contentStore, key)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := service.Capture(ctx, memory.CaptureRequest{
		Statement: "Temporary stable note.", Relation: memory.Supports, SourceType: "user",
		SourceRef: "interaction:inspect", IndependenceGroup: "user:self", DirectUserStatement: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	old := formatTime(time.Now().UTC().Add(-400 * 24 * time.Hour))
	if _, err := store.db.Exec(`UPDATE memory_items SET status = 'low_salience', salience = 0.1, last_accessed_at = ?, last_consolidated_at = ? WHERE id = ?`, old, old, entry.MemoryID); err != nil {
		t.Fatal(err)
	}
	var eventsBefore int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&eventsBefore); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Inspect(ctx, 10); err != nil {
		t.Fatal(err)
	}
	var gotStatus string
	var gotSalience float64
	var eventsAfter int
	if err := store.db.QueryRow(`SELECT status, salience FROM memory_items WHERE id = ?`, entry.MemoryID).Scan(&gotStatus, &gotSalience); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&eventsAfter); err != nil {
		t.Fatal(err)
	}
	if gotStatus != "low_salience" || gotSalience != 0.1 || eventsAfter != eventsBefore {
		t.Fatalf("Inspect mutated Memory status=%q salience=%f events=%d->%d", gotStatus, gotSalience, eventsBefore, eventsAfter)
	}
}

func TestMemoryConsolidationPromotionExplicitUseAndAssociation(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	key := bytes.Repeat([]byte{0x73}, 32)
	contentStore, err := content.New(filepath.Join(root, "content"), key)
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(filepath.Join(root, "metadata", "eri.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := memory.NewService(store, contentStore, key)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := service.Capture(ctx, memory.CaptureRequest{
		Statement: "azimuth preference", Kind: "preference", Relation: memory.Supports,
		SourceType: "observation", SourceRef: "trip-1", IndependenceGroup: "trip:1",
		Reliability: 1, Directness: 1, Verifiability: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if candidate.LifecycleStatus != "candidate" {
		t.Fatalf("first inferred memory lifecycle=%q", candidate.LifecycleStatus)
	}
	promoted, err := service.Capture(ctx, memory.CaptureRequest{
		ClaimID: candidate.ClaimID, Statement: candidate.Statement, Kind: "preference", Relation: memory.Supports,
		SourceType: "observation", SourceRef: "trip-2", IndependenceGroup: "trip:2",
		Reliability: 1, Directness: 1, Verifiability: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if promoted.LifecycleStatus != "active" || promoted.IndependentGroups != 2 {
		t.Fatalf("independent evidence did not promote memory: %+v", promoted)
	}
	second, err := service.Capture(ctx, memory.CaptureRequest{
		Statement: "quartz travel note", Kind: "semantic", Relation: memory.Supports,
		SourceType: "user", SourceRef: "interaction:quartz", IndependenceGroup: "user:self:quartz", ExplicitUserMemory: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := formatTime(time.Now().UTC())
	for _, statement := range []string{
		`INSERT INTO conversations(id, created_at) VALUES('memory-use-conversation', '` + now + `')`,
		`INSERT INTO tasks(id, conversation_id, source_interaction_id, source_channel, status, terminal_status, version, created_at, updated_at)
		 VALUES('memory-use-task', 'memory-use-conversation', 'source', 'test', 'running', '', 1, '` + now + `', '` + now + `')`,
		`INSERT INTO interactions(id, conversation_id, task_id, direction, role, kind, channel, content_ref_json, created_at)
		 VALUES('memory-source-interaction', 'memory-use-conversation', 'memory-use-task', 'inbound', 'user', 'text', 'test', '{}', '` + now + `')`,
		`INSERT INTO runs(id, task_id, status, model_status, soul_version, target, context_manifest_json, started_at, updated_at)
		 VALUES('memory-use-run', 'memory-use-task', 'active', 'dispatched', 'soul', 'test', '{}', '` + now + `', '` + now + `')`,
	} {
		if _, err := store.db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	used, err := service.Recall(ctx, memory.RecallRequest{
		Query: "azimuth quartz", RunID: "memory-use-run", SourceInteractionID: "memory-source-interaction", Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if used.RetrievalID == "" || len(used.Entries) != 2 {
		t.Fatalf("tracked recall = %+v", used)
	}
	if used.Entries[0].RecallScore < used.Entries[1].RecallScore || len(used.Entries[0].RecallReasons) == 0 {
		t.Fatalf("recall was not locally reranked with reasons: %+v", used.Entries)
	}
	var accessesBeforeUse int
	if err := store.db.QueryRow(`SELECT access_count FROM memory_items WHERE id = ?`, candidate.MemoryID).Scan(&accessesBeforeUse); err != nil {
		t.Fatal(err)
	}
	if accessesBeforeUse != 0 {
		t.Fatalf("mere retrieval reinforced memory: access_count=%d", accessesBeforeUse)
	}
	if err := service.MarkUsed(ctx, used.RetrievalID, []string{candidate.MemoryID}); err != nil {
		t.Fatal(err)
	}
	if err := service.MarkUsed(ctx, used.RetrievalID, []string{second.MemoryID}); err != nil {
		t.Fatal(err)
	}
	var retrievedItems, usedItems int
	if err := store.db.QueryRow(`SELECT COUNT(*), SUM(used) FROM memory_retrieval_items WHERE retrieval_id = ?`, used.RetrievalID).Scan(&retrievedItems, &usedItems); err != nil {
		t.Fatal(err)
	}
	if retrievedItems != 2 || usedItems != 2 {
		t.Fatalf("retrieval stages lost: retrieved=%d used=%d", retrievedItems, usedItems)
	}
	recalled, err := service.Retrieve(ctx, "azimuth", 10)
	if err != nil {
		t.Fatal(err)
	}
	foundAssociated := false
	for _, entry := range recalled.Entries {
		if entry.MemoryID == second.MemoryID {
			foundAssociated = true
		}
	}
	if !foundAssociated {
		t.Fatalf("associated memory was not completed from cue: %+v", recalled)
	}
	var accesses int
	var salience float64
	if err := store.db.QueryRow(`SELECT access_count, salience FROM memory_items WHERE id = ?`, candidate.MemoryID).Scan(&accesses, &salience); err != nil {
		t.Fatal(err)
	}
	if accesses != 1 || salience <= 0.5 {
		t.Fatalf("explicit use did not reinforce memory: accesses=%d salience=%f", accesses, salience)
	}
}

func TestMemoryConsolidationDecaysUnpinnedButPreservesPinned(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	key := bytes.Repeat([]byte{0x74}, 32)
	contentStore, _ := content.New(filepath.Join(root, "content"), key)
	store, err := Open(filepath.Join(root, "metadata", "eri.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, _ := memory.NewService(store, contentStore, key)
	unpinned, err := service.Capture(ctx, memory.CaptureRequest{
		Statement: "temporary observation", Relation: memory.Supports, SourceType: "observation", SourceRef: "old", IndependenceGroup: "old:1",
		Reliability: 1, Directness: 1, Verifiability: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PromoteMemory(ctx, unpinned.MemoryID, false); err != nil {
		t.Fatal(err)
	}
	pinned, err := service.Capture(ctx, memory.CaptureRequest{
		Statement: "stable user preference", Relation: memory.Supports, SourceType: "user", SourceRef: "user", IndependenceGroup: "user:self", ExplicitUserMemory: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-400 * 24 * time.Hour)
	for _, id := range []string{unpinned.MemoryID, pinned.MemoryID} {
		if _, err := store.db.Exec(`UPDATE memory_items SET salience = 0.1, last_accessed_at = ?, last_consolidated_at = ? WHERE id = ?`, formatTime(old), formatTime(old), id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.ConsolidateMemory(ctx, time.Now().UTC(), 100); err != nil {
		t.Fatal(err)
	}
	var unpinnedStatus, pinnedStatus string
	var pinnedSalience float64
	_ = store.db.QueryRow(`SELECT status FROM memory_items WHERE id = ?`, unpinned.MemoryID).Scan(&unpinnedStatus)
	_ = store.db.QueryRow(`SELECT status, salience FROM memory_items WHERE id = ?`, pinned.MemoryID).Scan(&pinnedStatus, &pinnedSalience)
	if unpinnedStatus != "low_salience" || pinnedStatus != "active" || pinnedSalience < .75 {
		t.Fatalf("lifecycle unpinned=%q pinned=%q salience=%f", unpinnedStatus, pinnedStatus, pinnedSalience)
	}
}

func TestDirectUserStatementIsUsableWithoutBeingPinned(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	key := bytes.Repeat([]byte{0x79}, 32)
	contentStore, err := content.New(filepath.Join(root, "content"), key)
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(filepath.Join(root, "metadata", "eri.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := memory.NewService(store, contentStore, key)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := service.Capture(ctx, memory.CaptureRequest{
		Statement: "I do not eat cilantro", Kind: "preference", Relation: memory.Supports,
		SourceType: "user", SourceRef: "task:preference", IndependenceGroup: "user:self",
		DirectUserStatement: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if entry.LifecycleStatus != "active" || entry.Pinned || entry.Status != memory.Supported {
		t.Fatalf("direct first-party memory = %+v", entry)
	}
}

func TestExpiredMemoryIsInspectableButNotRetrieved(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	key := bytes.Repeat([]byte{0x72}, 32)
	contentStore, err := content.New(filepath.Join(root, "content"), key)
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(filepath.Join(root, "metadata", "eri.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := memory.NewService(store, contentStore, key)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := service.Capture(ctx, memory.CaptureRequest{
		Statement: "The user is temporarily staying in Shanghai this week", Kind: "episodic", Relation: memory.Supports,
		SourceType: "user", SourceRef: "interaction:expired", IndependenceGroup: "user:self",
		ExplicitUserMemory: true, ObservedAt: time.Now().Add(-2 * time.Hour), ValidUntil: time.Now().Add(-time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !entry.Expired {
		t.Fatalf("expired evidence produced active snapshot: %+v", entry)
	}
	retrieved, err := service.Retrieve(ctx, "Shanghai", 10)
	if err != nil || len(retrieved.Entries) != 0 {
		t.Fatalf("expired memory was retrieved: %+v err=%v", retrieved, err)
	}
	inspected, err := service.Inspect(ctx, 10)
	if err != nil || len(inspected.Entries) != 1 || !inspected.Entries[0].Expired {
		t.Fatalf("expired memory was not inspectable: %+v err=%v", inspected, err)
	}
}

func TestMemoryDeleteConfirmsLogicalDeletionWhenPhysicalCleanupIsDeferred(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	key := bytes.Repeat([]byte{0x7a}, 32)
	baseContent, err := content.New(filepath.Join(root, "content"), key)
	if err != nil {
		t.Fatal(err)
	}
	contentStore := &failOnceDeleteContentStore{Store: baseContent, fail: true}
	store, err := Open(filepath.Join(root, "metadata", "eri.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := memory.NewService(store, contentStore, key)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := service.Capture(ctx, memory.CaptureRequest{
		Statement: "I prefer a quiet reading room", Kind: "preference", Relation: memory.Supports,
		SourceType: "user", SourceRef: "interaction:delete-cleanup", IndependenceGroup: "user:self", DirectUserStatement: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := service.Delete(ctx, entry.MemoryID)
	if err != nil {
		t.Fatalf("logical deletion lost its confirmed outcome after cleanup failure: %v", err)
	}
	if !plan.PhysicalCleanupPending {
		t.Fatalf("deferred cleanup was not reported: %+v", plan)
	}
	if recalled, err := service.Retrieve(ctx, "quiet reading room", 10); err != nil || len(recalled.Entries) != 0 {
		t.Fatalf("logically deleted Memory remained recallable: %+v err=%v", recalled, err)
	}
	var status string
	if err := store.db.QueryRowContext(ctx, `SELECT status FROM memory_delete_jobs WHERE memory_id = ?`, entry.MemoryID).Scan(&status); err != nil || status != "pending" {
		t.Fatalf("delete job status=%q err=%v", status, err)
	}
	if err := service.RecoverDeletes(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT status FROM memory_delete_jobs WHERE memory_id = ?`, entry.MemoryID).Scan(&status); err != nil || status != "completed" {
		t.Fatalf("recovered delete job status=%q err=%v", status, err)
	}
}
