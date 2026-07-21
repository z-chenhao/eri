package sqlite

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/z-chenhao/eri/internal/content"
	"github.com/z-chenhao/eri/internal/episode"
	"github.com/z-chenhao/eri/internal/evolution"
	"github.com/z-chenhao/eri/internal/feedback"
	"github.com/z-chenhao/eri/internal/runtime"
)

func TestCorrectiveFeedbackLinksDeliveryInvalidatesOldDerivedDataAndCandidatesReplacement(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	contentStore, err := content.New(filepath.Join(root, "content"), bytes.Repeat([]byte{0x41}, 32))
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
		`INSERT INTO conversations(id, created_at) VALUES('conversation', '` + now + `')`,
		`INSERT INTO tasks(id, conversation_id, source_interaction_id, source_channel, status, terminal_status, version, created_at, updated_at)
		 VALUES('source-task', 'conversation', 'source-in', 'test', 'completed', 'completed', 1, '` + now + `', '` + now + `')`,
		`INSERT INTO tasks(id, conversation_id, source_interaction_id, source_channel, status, terminal_status, version, created_at, updated_at)
		 VALUES('feedback-task', 'conversation', 'feedback-in', 'test', 'running', 'completed', 1, '` + now + `', '` + now + `')`,
		`INSERT INTO runs(id, task_id, status, model_status, soul_version, target, context_manifest_json, started_at, updated_at, ended_at)
		 VALUES('source-run', 'source-task', 'succeeded', 'succeeded', 'soul', 'test:model', '{}', '` + now + `', '` + now + `', '` + now + `')`,
		`INSERT INTO artifacts(id, task_id, run_id, version, kind, content_ref_json, status, trace_ref_json, created_at)
		 VALUES('source-artifact', 'source-task', 'source-run', 1, 'reply', '{}', 'delivered', '{}', '` + now + `')`,
		`INSERT INTO deliveries(id, task_id, artifact_id, target_channel, status, receipt, idempotency_key, terminal_status, created_at, updated_at)
		 VALUES('source-delivery', 'source-task', 'source-artifact', 'test', 'sent', 'accepted', 'source-key', 'completed', '` + now + `', '` + now + `')`,
		`INSERT INTO episodes(id, task_id, manifest_ref_json, status, created_at)
		 VALUES('old-episode', 'source-task', '{}', 'ready', '` + now + `')`,
		`INSERT INTO dataset_candidates(id, episode_id, status, created_at)
		 VALUES('old-candidate', 'old-episode', 'candidate', '` + now + `')`,
	} {
		if _, err := store.db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	service, err := feedback.NewService(store, contentStore)
	if err != nil {
		t.Fatal(err)
	}
	record, err := service.Capture(ctx, "feedback-task", feedback.Correction, "", "The source answer used the wrong date.", "")
	if err != nil {
		t.Fatal(err)
	}
	if record.SourceTaskID != "source-task" || record.ArtifactID != "source-artifact" || record.DeliveryID != "source-delivery" {
		t.Fatalf("feedback link = %+v", record)
	}
	var oldEpisode, oldCandidate string
	if err := store.db.QueryRowContext(ctx, `SELECT status FROM episodes WHERE id='old-episode'`).Scan(&oldEpisode); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT status FROM dataset_candidates WHERE id='old-candidate'`).Scan(&oldCandidate); err != nil {
		t.Fatal(err)
	}
	if oldEpisode != "invalidated" || oldCandidate != "invalidated" {
		t.Fatalf("old derived data episode=%q candidate=%q", oldEpisode, oldCandidate)
	}
	var posteriorOutbox int
	if err := store.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM internal_outbox
		WHERE kind = 'evolution.feedback' AND aggregate_id = ?`, record.ID).Scan(&posteriorOutbox); err != nil || posteriorOutbox != 1 {
		t.Fatalf("posterior evolution outbox=%d err=%v", posteriorOutbox, err)
	}
	evolutionService, err := evolution.NewService(store, contentStore, evolutionProposalModel{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := evolutionService.HandleFeedback(ctx, runtime.OutboxItem{AggregateID: record.ID}); err != nil {
			t.Fatal(err)
		}
	}
	var signalCount int
	var signalResult, signalRefJSON string
	if err := store.db.QueryRowContext(ctx, `
		SELECT COUNT(*), result, findings_ref_json FROM evolution_signals WHERE id = ?`, record.ID).
		Scan(&signalCount, &signalResult, &signalRefJSON); err != nil {
		t.Fatal(err)
	}
	if signalCount != 1 || signalResult != "repair" || signalRefJSON == "" {
		t.Fatalf("posterior evolution signal count=%d result=%q ref=%q", signalCount, signalResult, signalRefJSON)
	}
	manifestRef, err := contentStore.Put(ctx, []byte(`{"task_id":"feedback-task"}`), content.Metadata{
		MediaType: "application/json", EncryptionDomain: "episode", PrivacyClass: "private", RetentionPolicy: "user_owned",
	})
	if err != nil {
		t.Fatal(err)
	}
	newEpisode, err := store.SaveEpisode(ctx, episode.Record{ID: "feedback-episode", TaskID: "feedback-task", ManifestRef: manifestRef, CreatedAt: time.Now().UTC()})
	if err != nil || newEpisode.ID != "feedback-episode" {
		t.Fatalf("episode=%+v err=%v", newEpisode, err)
	}
	var candidateStatus string
	if err := store.db.QueryRowContext(ctx, `SELECT status FROM dataset_candidates WHERE episode_id='feedback-episode'`).Scan(&candidateStatus); err != nil || candidateStatus != "candidate" {
		t.Fatalf("replacement candidate=%q err=%v", candidateStatus, err)
	}
}

func TestCorrectiveFeedbackBeforeEpisodeBuildKeepsSourceEpisodeInvalidated(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	contentStore, err := content.New(filepath.Join(root, "content"), bytes.Repeat([]byte{0x44}, 32))
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
		`INSERT INTO conversations(id, created_at) VALUES('conversation', '` + now + `')`,
		`INSERT INTO tasks(id, conversation_id, source_interaction_id, source_channel, status, terminal_status, version, created_at, updated_at)
		 VALUES('source-task', 'conversation', 'source-in', 'test', 'completed', 'completed', 1, '` + now + `', '` + now + `'),
		       ('feedback-task', 'conversation', 'feedback-in', 'test', 'running', 'completed', 1, '` + now + `', '` + now + `')`,
		`INSERT INTO runs(id, task_id, status, model_status, soul_version, target, context_manifest_json, started_at, updated_at, ended_at)
		 VALUES('source-run', 'source-task', 'succeeded', 'succeeded', 'soul', 'test:model', '{}', '` + now + `', '` + now + `', '` + now + `')`,
		`INSERT INTO artifacts(id, task_id, run_id, version, kind, content_ref_json, status, trace_ref_json, created_at)
		 VALUES('source-artifact', 'source-task', 'source-run', 1, 'reply', '{}', 'delivered', '{}', '` + now + `')`,
		`INSERT INTO deliveries(id, task_id, artifact_id, target_channel, status, receipt, idempotency_key, terminal_status, created_at, updated_at)
		 VALUES('source-delivery', 'source-task', 'source-artifact', 'test', 'sent', 'accepted', 'source-key', 'completed', '` + now + `', '` + now + `')`,
	} {
		if _, err := store.db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	service, err := feedback.NewService(store, contentStore)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Capture(ctx, "feedback-task", feedback.Correction, "", "The source answer needs correction.", ""); err != nil {
		t.Fatal(err)
	}
	manifestRef, err := contentStore.Put(ctx, []byte(`{"task_id":"source-task"}`), content.Metadata{
		MediaType: "application/json", EncryptionDomain: "episode", PrivacyClass: "private", RetentionPolicy: "user_owned",
	})
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.SaveEpisode(ctx, episode.Record{ID: "source-episode", TaskID: "source-task", ManifestRef: manifestRef, CreatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != "invalidated" {
		t.Fatalf("episode status=%q", record.Status)
	}
	var storedStatus string
	if err := store.db.QueryRowContext(ctx, `SELECT status FROM episodes WHERE id='source-episode'`).Scan(&storedStatus); err != nil {
		t.Fatal(err)
	}
	if storedStatus != "invalidated" {
		t.Fatalf("stored episode status=%q", storedStatus)
	}
	var candidates int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM dataset_candidates WHERE episode_id='source-episode'`).Scan(&candidates); err != nil {
		t.Fatal(err)
	}
	if candidates != 0 {
		t.Fatalf("invalidated source dataset candidates=%d", candidates)
	}
}

func TestFeedbackCannotReferenceDeliveryOutsideItsConversation(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	contentStore, err := content.New(filepath.Join(root, "content"), bytes.Repeat([]byte{0x42}, 32))
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
		`INSERT INTO conversations(id, created_at) VALUES('one', '` + now + `'), ('two', '` + now + `')`,
		`INSERT INTO tasks(id, conversation_id, source_interaction_id, source_channel, status, terminal_status, version, created_at, updated_at)
		 VALUES('feedback-task', 'one', 'in-one', 'test', 'running', 'completed', 1, '` + now + `', '` + now + `'),
		       ('foreign-task', 'two', 'in-two', 'test', 'completed', 'completed', 1, '` + now + `', '` + now + `')`,
		`INSERT INTO runs(id, task_id, status, model_status, soul_version, target, context_manifest_json, started_at, updated_at, ended_at)
		 VALUES('foreign-run', 'foreign-task', 'succeeded', 'succeeded', 'soul', 'test:model', '{}', '` + now + `', '` + now + `', '` + now + `')`,
		`INSERT INTO artifacts(id, task_id, run_id, version, kind, content_ref_json, status, trace_ref_json, created_at)
		 VALUES('foreign-artifact', 'foreign-task', 'foreign-run', 1, 'reply', '{}', 'delivered', '{}', '` + now + `')`,
		`INSERT INTO deliveries(id, task_id, artifact_id, target_channel, status, receipt, idempotency_key, terminal_status, created_at, updated_at)
		 VALUES('foreign-delivery', 'foreign-task', 'foreign-artifact', 'test', 'sent', 'accepted', 'foreign-key', 'completed', '` + now + `', '` + now + `')`,
	} {
		if _, err := store.db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	service, _ := feedback.NewService(store, contentStore)
	if _, err := service.Capture(ctx, "feedback-task", feedback.Accepted, "", "Looks good.", "foreign-delivery"); err == nil {
		t.Fatal("cross-conversation feedback reference was accepted")
	}
	var objects int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM content_objects`).Scan(&objects); err != nil || objects != 0 {
		t.Fatalf("orphan feedback content objects=%d err=%v", objects, err)
	}
}
