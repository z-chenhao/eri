package sqlite

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/z-chenhao/eri/internal/channel"
	"github.com/z-chenhao/eri/internal/content"
	"github.com/z-chenhao/eri/internal/delivery"
	"github.com/z-chenhao/eri/internal/scheduler"
)

func TestCommitmentFirePreservesCreatingLarkTarget(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := Open(filepath.Join(root, "metadata", "eri.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	contentStore, err := content.New(filepath.Join(root, "content"), bytes.Repeat([]byte{0x51}, 32))
	if err != nil {
		t.Fatal(err)
	}
	inbound, _, err := store.CreateExternalInbound(ctx, "lark", channel.ExternalInteraction{
		MessageID: "om_create_reminder", ConversationID: "oc_owner_chat", SenderID: "ou_owner", CreatedAt: time.Now().UTC(),
	}, testRef("reminder-input", "reminder-input-hash"), nil)
	if err != nil {
		t.Fatal(err)
	}
	service := scheduler.NewService(store, contentStore)
	commitment, err := service.Create(ctx, inbound.TaskID, scheduler.CreateRequest{
		Task: "Remind the user to go to the bathroom", Schedule: scheduler.Schedule{Type: "once", At: time.Now().UTC().Add(time.Minute)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if commitment.Target.Channel != "lark" || commitment.Target.ConversationID != "oc_owner_chat" || commitment.Target.ReplyToMessageID != "om_create_reminder" {
		t.Fatalf("commitment target = %+v", commitment.Target)
	}
	if commitment.Target.RoutingMode != scheduler.DeliveryRouteOrigin || commitment.DeliveryRoute != scheduler.DeliveryRouteOrigin {
		t.Fatalf("commitment routing mode target=%q model=%q", commitment.Target.RoutingMode, commitment.DeliveryRoute)
	}
	if commitment.Task != "Remind the user to go to the bathroom" {
		t.Fatalf("model-facing commitment task = %q", commitment.Task)
	}
	commitTerminalSourceTask(t, store, inbound.TaskID, "lark")
	triggered, err := store.TriggerDueCommitments(ctx, commitment.NextRunAt.Add(time.Second), 10)
	if err != nil || triggered != 1 {
		t.Fatalf("triggered=%d err=%v", triggered, err)
	}
	var fireTaskID, sourceChannel, fireChannel, conversationID, replyToMessageID, routingMode string
	if err := store.db.QueryRowContext(ctx, `
		SELECT f.task_id, t.source_channel, f.target_channel, f.target_conversation_id,
			f.reply_to_message_id, f.routing_mode FROM commitment_fires f
		JOIN tasks t ON t.id = f.task_id WHERE f.commitment_id = ?`, commitment.ID).
		Scan(&fireTaskID, &sourceChannel, &fireChannel, &conversationID, &replyToMessageID, &routingMode); err != nil {
		t.Fatal(err)
	}
	if sourceChannel != "lark" || fireChannel != "lark" || conversationID != "oc_owner_chat" || replyToMessageID != "om_create_reminder" || routingMode != scheduler.DeliveryRouteOrigin {
		t.Fatalf("fire route source=%q target=%q conversation=%q reply=%q mode=%q", sourceChannel, fireChannel, conversationID, replyToMessageID, routingMode)
	}
	claimedTask, claimed, err := store.ClaimTask(ctx, fireTaskID, "test-worker", time.Minute, "soul", `{}`, "test:model")
	if err != nil || !claimed {
		t.Fatalf("claim scheduled task claimed=%t err=%v", claimed, err)
	}
	if claimedTask.CurrentTask.TaskID != fireTaskID || claimedTask.CurrentTask.CommitmentID != commitment.ID ||
		claimedTask.CurrentTask.SourceKind != "internal_trigger" || claimedTask.CurrentTask.SourceRole != "user" ||
		claimedTask.CurrentTask.TriggerChannel != "scheduler" || claimedTask.CurrentTask.TriggerEvent != "commitment.due" ||
		claimedTask.CurrentTask.TriggerState != "occurred" || claimedTask.CurrentTask.ExecutionPhase != "fulfillment" ||
		!claimedTask.CurrentTask.ScheduledFor.Equal(commitment.NextRunAt) {
		t.Fatalf("scheduled task capsule = %+v", claimedTask.CurrentTask)
	}
	if len(claimedTask.Messages) < 2 || claimedTask.Messages[len(claimedTask.Messages)-1].ID != claimedTask.CurrentTask.SourceInteractionID ||
		claimedTask.Messages[len(claimedTask.Messages)-1].Kind != "internal_trigger" {
		t.Fatalf("fulfillment context did not append to the canonical conversation: %+v", claimedTask.Messages)
	}
	objective, err := contentStore.Get(ctx, claimedTask.ObjectiveRef)
	if err != nil || !bytes.Contains(objective, []byte("<system_reminder>")) || !bytes.Contains(objective, []byte("Remind the user to go to the bathroom")) {
		t.Fatalf("scheduled task objective=%q err=%v", objective, err)
	}
	now := formatTime(time.Now().UTC())
	for _, statement := range []string{
		`INSERT INTO runs(id, task_id, status, model_status, soul_version, target, context_manifest_json, started_at, updated_at) VALUES('fire-run', '` + fireTaskID + `', 'active', 'dispatched', 'soul', 'test:model', '{}', '` + now + `', '` + now + `')`,
		`UPDATE tasks SET status = 'waiting', terminal_status = 'completed', wait_reason = 'delivery' WHERE id = '` + fireTaskID + `'`,
		`INSERT INTO artifacts(id, task_id, run_id, version, kind, content_ref_json, status, trace_ref_json, created_at)
		 VALUES('fire-artifact', '` + fireTaskID + `', 'fire-run', 1, 'text', '{}', 'approved', '{}', '` + now + `')`,
		`INSERT INTO deliveries(id, task_id, artifact_id, target_channel, status, receipt, idempotency_key, terminal_status, created_at, updated_at)
		 VALUES('fire-delivery', '` + fireTaskID + `', 'fire-artifact', 'lark', 'queued', '', 'fire-key', 'completed', '` + now + `', '` + now + `')`,
	} {
		if _, err := store.db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	deliveryRecord, found, err := store.LoadDelivery(ctx, "fire-delivery")
	if err != nil || !found {
		t.Fatalf("delivery found=%t err=%v", found, err)
	}
	if deliveryRecord.ExternalTarget.ConversationID != "oc_owner_chat" || deliveryRecord.ExternalTarget.ReplyToMessageID != "om_create_reminder" {
		t.Fatalf("delivery target = %+v", deliveryRecord.ExternalTarget)
	}
	deliveredAt := time.Now().UTC()
	if err := store.CommitConversationDelivery(ctx, "fire-delivery", "fire-outbound", delivery.Receipt{
		Level: "accepted_by_channel", ExternalMessageID: "om_fire_outbound",
	}, deliveredAt); err != nil {
		t.Fatalf("commit scheduled Lark delivery: %v", err)
	}
	var deliveryStatus, taskStatus, outboundConversationID, outboundReplyID string
	if err := store.db.QueryRowContext(ctx, `
		SELECT d.status, t.status, cm.external_conversation_id, cm.reply_to_external_message_id
		FROM deliveries d
		JOIN tasks t ON t.id = d.task_id
		JOIN interactions i ON i.delivery_id = d.id
		JOIN channel_messages cm ON cm.interaction_id = i.id
		WHERE d.id = ? AND cm.external_message_id = ?`, "fire-delivery", "om_fire_outbound").
		Scan(&deliveryStatus, &taskStatus, &outboundConversationID, &outboundReplyID); err != nil {
		t.Fatal(err)
	}
	if deliveryStatus != "sent" || taskStatus != "completed" || outboundConversationID != "oc_owner_chat" || outboundReplyID != "om_create_reminder" {
		t.Fatalf("committed delivery status=%q task=%q conversation=%q reply=%q", deliveryStatus, taskStatus, outboundConversationID, outboundReplyID)
	}
	var eventPayload string
	if err := store.db.QueryRowContext(ctx, `
		SELECT payload_json FROM events WHERE aggregate_id = ? AND type = 'commitment.triggered'`, commitment.ID).
		Scan(&eventPayload); err != nil {
		t.Fatal(err)
	}
	if eventPayload != `{"routing_mode":"origin_channel","scheduled_for":"`+formatTime(commitment.NextRunAt)+`","target_channel":"lark","task_id":"`+fireTaskID+`"}` {
		t.Fatalf("commitment trigger event = %s", eventPayload)
	}
}

func TestDueCommitmentWaitsForSourceDeliveryAcrossRestart(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	databasePath := filepath.Join(root, "metadata", "eri.db")
	store, err := Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	contentStore, err := content.New(filepath.Join(root, "content"), bytes.Repeat([]byte{0x55}, 32))
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	inbound, err := store.CreateInbound(ctx, "conversation_web", testRef("short-reminder-input", "short-reminder-input-hash"), nil)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	service := scheduler.NewService(store, contentStore)
	commitment, err := service.Create(ctx, inbound.TaskID, scheduler.CreateRequest{
		Task: "Remind the user to read the book", Schedule: scheduler.Schedule{Type: "once", At: time.Now().UTC().Add(10 * time.Millisecond)},
	})
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	due := commitment.NextRunAt.Add(time.Second)
	if triggered, err := store.TriggerDueCommitments(ctx, due, 10); err != nil || triggered != 0 {
		store.Close()
		t.Fatalf("commitment fired before source delivery: triggered=%d err=%v", triggered, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	commitTerminalSourceTask(t, store, inbound.TaskID, "conversation_web")
	if triggered, err := store.TriggerDueCommitments(ctx, due, 10); err != nil || triggered != 1 {
		t.Fatalf("due commitment did not fire after source delivery: triggered=%d err=%v", triggered, err)
	}
	if triggered, err := store.TriggerDueCommitments(ctx, due, 10); err != nil || triggered != 0 {
		t.Fatalf("commitment fire was not idempotent: triggered=%d err=%v", triggered, err)
	}
	var sourceTaskID, scheduledFor string
	if err := store.db.QueryRowContext(ctx, `
		SELECT c.source_task_id, f.scheduled_for
		FROM commitments c JOIN commitment_fires f ON f.commitment_id = c.id
		WHERE c.id = ?`, commitment.ID).Scan(&sourceTaskID, &scheduledFor); err != nil {
		t.Fatal(err)
	}
	if sourceTaskID != inbound.TaskID || scheduledFor != formatTime(commitment.NextRunAt) {
		t.Fatalf("fire lineage source_task_id=%q scheduled_for=%q", sourceTaskID, scheduledFor)
	}
}

func TestCommitmentUpdateReplacesScheduleWithoutOverlap(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := Open(filepath.Join(root, "metadata", "eri.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	contentStore, err := content.New(filepath.Join(root, "content"), bytes.Repeat([]byte{0x54}, 32))
	if err != nil {
		t.Fatal(err)
	}
	inbound, _, err := store.CreateExternalInbound(ctx, "lark", channel.ExternalInteraction{
		MessageID: "om_create_monitor", ConversationID: "oc_owner_chat", SenderID: "ou_owner", CreatedAt: time.Now().UTC(),
	}, testRef("monitor-input", "monitor-input-hash"), nil)
	if err != nil {
		t.Fatal(err)
	}
	service := scheduler.NewService(store, contentStore)
	original, err := service.Create(ctx, inbound.TaskID, scheduler.CreateRequest{
		Task: "Check every minute", Schedule: scheduler.Schedule{Type: "interval", IntervalSeconds: 60},
	})
	if err != nil {
		t.Fatal(err)
	}
	commitTerminalSourceTask(t, store, inbound.TaskID, "lark")
	clarification, err := store.CreateInbound(ctx, "conversation_web", testRef("monitor-clarification", "monitor-clarification-hash"), nil)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := service.Update(ctx, clarification.TaskID, original.ID, scheduler.CreateRequest{
		Task: "Check every hour with corrected scope", Schedule: scheduler.Schedule{Type: "interval", IntervalSeconds: 3600},
		Importance: "normal", DeliveryRoute: scheduler.DeliveryRouteOrigin,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID != original.ID || updated.SourceTaskID != clarification.TaskID || updated.Version != 2 ||
		updated.Schedule.IntervalSeconds != 3600 || !updated.NextRunAt.After(original.NextRunAt) || updated.Task != "Check every hour with corrected scope" || updated.DeliveryRoute != scheduler.DeliveryRouteOrigin {
		t.Fatalf("updated commitment = %+v, original = %+v", updated, original)
	}
	if updated.Target.Channel != "lark" || updated.Target.ConversationID != "oc_owner_chat" || updated.Target.ReplyToMessageID != "om_create_monitor" {
		t.Fatalf("clarification moved frozen origin target: %+v", updated.Target)
	}
	listed, err := service.List(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != original.ID || listed[0].Version != 2 || listed[0].Schedule.IntervalSeconds != 3600 || listed[0].Task != "Check every hour with corrected scope" || listed[0].DeliveryRoute != scheduler.DeliveryRouteOrigin {
		t.Fatalf("listed commitments = %+v", listed)
	}
	if triggered, err := store.TriggerDueCommitments(ctx, original.NextRunAt.Add(time.Second), 10); err != nil || triggered != 0 {
		t.Fatalf("old schedule still fired: triggered=%d err=%v", triggered, err)
	}
	prompt, err := contentStore.Get(ctx, updated.TaskRef)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(prompt, []byte("corrected scope")) || bytes.Contains(prompt, []byte("unrelated earlier conversation")) {
		t.Fatalf("updated prompt = %q", prompt)
	}
	if triggered, err := store.TriggerDueCommitments(ctx, updated.NextRunAt.Add(time.Second), 10); err != nil || triggered != 0 {
		t.Fatalf("updated schedule fired before updating Task delivery: triggered=%d err=%v", triggered, err)
	}
	commitTerminalSourceTask(t, store, clarification.TaskID, "conversation_web")
	if triggered, err := store.TriggerDueCommitments(ctx, updated.NextRunAt.Add(time.Second), 10); err != nil || triggered != 1 {
		t.Fatalf("updated schedule did not fire after updating Task delivery: triggered=%d err=%v", triggered, err)
	}
}

func TestEriProposedCommitmentFireUsesLatestTrustedUserChannel(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := Open(filepath.Join(root, "metadata", "eri.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	contentStore, err := content.New(filepath.Join(root, "content"), bytes.Repeat([]byte{0x52}, 32))
	if err != nil {
		t.Fatal(err)
	}
	createdAt := time.Now().UTC()
	origin, err := store.CreateInbound(ctx, "conversation_web", testRef("proactive-origin", "proactive-origin-hash"), nil)
	if err != nil {
		t.Fatal(err)
	}
	service := scheduler.NewService(store, contentStore)
	commitment, err := service.Create(ctx, origin.TaskID, scheduler.CreateRequest{
		Task: "Review material AI developments", Schedule: scheduler.Schedule{Type: "once", At: createdAt.Add(time.Minute)},
		DeliveryRoute: scheduler.DeliveryRouteRecent,
	})
	if err != nil {
		t.Fatal(err)
	}
	if commitment.Target.Channel != "conversation_web" || commitment.Target.RoutingMode != scheduler.DeliveryRouteRecent {
		t.Fatalf("initial commitment target = %+v", commitment.Target)
	}
	if _, _, err := store.CreateExternalInbound(ctx, "lark", channel.ExternalInteraction{
		MessageID: "om_latest", ConversationID: "oc_latest", SenderID: "ou_owner", CreatedAt: createdAt.Add(time.Second),
	}, testRef("latest-lark", "latest-lark-hash"), nil); err != nil {
		t.Fatal(err)
	}
	commitTerminalSourceTask(t, store, origin.TaskID, "conversation_web")
	triggered, err := store.TriggerDueCommitments(ctx, commitment.NextRunAt.Add(time.Second), 10)
	if err != nil || triggered != 1 {
		t.Fatalf("triggered=%d err=%v", triggered, err)
	}
	var fireTaskID, sourceChannel, fireChannel, conversationID, replyToMessageID, routingMode string
	if err := store.db.QueryRowContext(ctx, `
		SELECT f.task_id, t.source_channel, f.target_channel, f.target_conversation_id,
			f.reply_to_message_id, f.routing_mode FROM commitment_fires f
		JOIN tasks t ON t.id = f.task_id WHERE f.commitment_id = ?`, commitment.ID).
		Scan(&fireTaskID, &sourceChannel, &fireChannel, &conversationID, &replyToMessageID, &routingMode); err != nil {
		t.Fatal(err)
	}
	if sourceChannel != "lark" || fireChannel != "lark" || conversationID != "oc_latest" || replyToMessageID != "" || routingMode != scheduler.DeliveryRouteRecent {
		t.Fatalf("recent fire route source=%q target=%q conversation=%q reply=%q mode=%q", sourceChannel, fireChannel, conversationID, replyToMessageID, routingMode)
	}
	now := formatTime(time.Now().UTC())
	for _, statement := range []string{
		`INSERT INTO runs(id, task_id, status, model_status, soul_version, target, context_manifest_json, started_at, updated_at) VALUES('recent-run', '` + fireTaskID + `', 'active', 'dispatched', 'soul', 'test:model', '{}', '` + now + `', '` + now + `')`,
		`INSERT INTO artifacts(id, task_id, run_id, version, kind, content_ref_json, status, trace_ref_json, created_at)
		 VALUES('recent-artifact', '` + fireTaskID + `', 'recent-run', 1, 'text', '{}', 'approved', '{}', '` + now + `')`,
		`INSERT INTO deliveries(id, task_id, artifact_id, target_channel, status, receipt, idempotency_key, terminal_status, created_at, updated_at)
		 VALUES('recent-delivery', '` + fireTaskID + `', 'recent-artifact', 'lark', 'queued', '', 'recent-key', 'completed', '` + now + `', '` + now + `')`,
	} {
		if _, err := store.db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	deliveryRecord, found, err := store.LoadDelivery(ctx, "recent-delivery")
	if err != nil || !found {
		t.Fatalf("delivery found=%t err=%v", found, err)
	}
	if deliveryRecord.ExternalTarget.ConversationID != "oc_latest" || deliveryRecord.ExternalTarget.ReplyToMessageID != "" {
		t.Fatalf("recent delivery target = %+v", deliveryRecord.ExternalTarget)
	}
}

func TestEriProposedCommitmentCanFollowLatestUserBackToWeb(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := Open(filepath.Join(root, "metadata", "eri.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	contentStore, err := content.New(filepath.Join(root, "content"), bytes.Repeat([]byte{0x53}, 32))
	if err != nil {
		t.Fatal(err)
	}
	createdAt := time.Now().UTC()
	origin, _, err := store.CreateExternalInbound(ctx, "lark", channel.ExternalInteraction{
		MessageID: "om_origin", ConversationID: "oc_origin", SenderID: "ou_owner", CreatedAt: createdAt,
	}, testRef("proactive-lark-origin", "proactive-lark-origin-hash"), nil)
	if err != nil {
		t.Fatal(err)
	}
	service := scheduler.NewService(store, contentStore)
	commitment, err := service.Create(ctx, origin.TaskID, scheduler.CreateRequest{
		Task: "Review material AI developments", Schedule: scheduler.Schedule{Type: "once", At: createdAt.Add(time.Minute)},
		DeliveryRoute: scheduler.DeliveryRouteRecent,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateInbound(ctx, "conversation_web", testRef("latest-web", "latest-web-hash"), nil); err != nil {
		t.Fatal(err)
	}
	commitTerminalSourceTask(t, store, origin.TaskID, "lark")
	if triggered, err := store.TriggerDueCommitments(ctx, commitment.NextRunAt.Add(time.Second), 10); err != nil || triggered != 1 {
		t.Fatalf("triggered=%d err=%v", triggered, err)
	}
	var sourceChannel, fireChannel, conversationID, replyToMessageID, routingMode string
	if err := store.db.QueryRowContext(ctx, `
		SELECT t.source_channel, f.target_channel, f.target_conversation_id,
			f.reply_to_message_id, f.routing_mode FROM commitment_fires f
		JOIN tasks t ON t.id = f.task_id WHERE f.commitment_id = ?`, commitment.ID).
		Scan(&sourceChannel, &fireChannel, &conversationID, &replyToMessageID, &routingMode); err != nil {
		t.Fatal(err)
	}
	if sourceChannel != "conversation_web" || fireChannel != "conversation_web" || conversationID != "" || replyToMessageID != "" || routingMode != scheduler.DeliveryRouteRecent {
		t.Fatalf("web fire route source=%q target=%q conversation=%q reply=%q mode=%q", sourceChannel, fireChannel, conversationID, replyToMessageID, routingMode)
	}
}

func commitTerminalSourceTask(t *testing.T, store *Store, taskID, targetChannel string) {
	t.Helper()
	ctx := context.Background()
	now := formatTime(time.Now().UTC())
	runID := taskID + "-terminal-run"
	artifactID := taskID + "-terminal-artifact"
	deliveryID := taskID + "-terminal-delivery"
	for _, statement := range []string{
		`INSERT INTO runs(id, task_id, status, model_status, soul_version, target, context_manifest_json, started_at, updated_at)
		 VALUES('` + runID + `', '` + taskID + `', 'active', 'succeeded', 'soul', 'test:model', '{}', '` + now + `', '` + now + `')`,
		`UPDATE tasks SET status = 'waiting', terminal_status = 'completed', wait_reason = 'delivery' WHERE id = '` + taskID + `'`,
		`INSERT INTO artifacts(id, task_id, run_id, version, kind, content_ref_json, status, trace_ref_json, created_at)
		 VALUES('` + artifactID + `', '` + taskID + `', '` + runID + `', 1, 'text', '{}', 'approved', '{}', '` + now + `')`,
		`INSERT INTO deliveries(id, task_id, artifact_id, target_channel, status, receipt, idempotency_key, terminal_status, created_at, updated_at)
		 VALUES('` + deliveryID + `', '` + taskID + `', '` + artifactID + `', '` + targetChannel + `', 'queued', '', '` + artifactID + `:` + targetChannel + `', 'completed', '` + now + `', '` + now + `')`,
	} {
		if _, err := store.db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.CommitConversationDelivery(ctx, deliveryID, taskID+"-terminal-interaction", delivery.Receipt{
		Level: "accepted_by_test",
	}, time.Now().UTC()); err != nil {
		t.Fatalf("commit source delivery: %v", err)
	}
}
