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
)

type semanticTestEncoder struct{}

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
		`INSERT INTO runs(id, task_id, status, soul_version, started_at, ended_at)
		 VALUES('memory-delete-run', 'memory-delete-task', 'succeeded', 'soul', '` + now + `', '` + now + `')`,
		`INSERT INTO steps(id, run_id, task_id, kind, status, created_at, updated_at)
		 VALUES('memory-delete-step', 'memory-delete-run', 'memory-delete-task', 'model', 'succeeded', '` + now + `', '` + now + `')`,
		`INSERT INTO invocations(id, run_id, task_id, step_id, kind, status, target, context_manifest_json, created_at, updated_at)
		 VALUES('memory-delete-invocation', 'memory-delete-run', 'memory-delete-task', 'memory-delete-step', 'model', 'succeeded', 'test', '{"memory":["` + initial.MemoryID + `"]}', '` + now + `', '` + now + `')`,
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
		`INSERT INTO runs(id, task_id, status, soul_version, started_at) VALUES('memory-use-run', 'memory-use-task', 'active', 'soul', '` + now + `')`,
		`INSERT INTO steps(id, run_id, task_id, kind, status, created_at, updated_at)
		 VALUES('memory-use-step', 'memory-use-run', 'memory-use-task', 'model', 'active', '` + now + `', '` + now + `')`,
		`INSERT INTO invocations(id, run_id, task_id, step_id, kind, status, target, context_manifest_json, created_at, updated_at)
		 VALUES('memory-use-invocation', 'memory-use-run', 'memory-use-task', 'memory-use-step', 'model', 'dispatched', 'test', '{}', '` + now + `', '` + now + `')`,
	} {
		if _, err := store.db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	used, err := service.Recall(ctx, memory.RecallRequest{
		Query: "azimuth quartz", TaskID: "memory-use-task", InvocationID: "memory-use-invocation", Limit: 10,
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
