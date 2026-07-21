// Package sqlite implements Eri's default operational record, event spine,
// and transactional outbox in one local SQLite database.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/z-chenhao/eri/internal/agent"
	"github.com/z-chenhao/eri/internal/approval"
	"github.com/z-chenhao/eri/internal/channel"
	"github.com/z-chenhao/eri/internal/content"
	"github.com/z-chenhao/eri/internal/delivery"
	"github.com/z-chenhao/eri/internal/eval"
	"github.com/z-chenhao/eri/internal/eventlog"
	"github.com/z-chenhao/eri/internal/execution"
	"github.com/z-chenhao/eri/internal/identifier"
	"github.com/z-chenhao/eri/internal/policy"
	"github.com/z-chenhao/eri/internal/runtime"
	"github.com/z-chenhao/eri/internal/tool"
	_ "modernc.org/sqlite"
)

const (
	// SQLite compares persisted timestamps as TEXT. A fixed-width fractional
	// second keeps lexical order identical to chronological order; RFC3339Nano
	// trims trailing zeroes and can briefly hide a newly queued outbox item.
	timestampLayout = "2006-01-02T15:04:05.000000000Z07:00"
	// Before the MVP ships, schema changes replace this authoritative shape
	// instead of accumulating migration generations.
	schemaVersion = 1
)

type schemaRequirement struct {
	table   string
	columns []string
}

var authoritativeSchemaRequirements = []schemaRequirement{
	{table: "artifacts", columns: []string{"trace_ref_json"}},
	{table: "eval_records", columns: []string{"findings_ref_json", "finding_count"}},
	{table: "effect_intents", columns: []string{"payload_ref_json", "parent_intent_id", "invocation_id", "tool_call_id"}},
	{table: "subagent_runs", columns: []string{"role_id", "provider_id", "access_mode", "request_ref_json", "runtime_state_ref_json", "runtime_id", "result_ref_json", "continuation_ref_json"}},
	{table: "conversation_introductions", columns: []string{"conversation_id", "task_id", "requested_at"}},
	{table: "channel_messages", columns: []string{"channel", "external_message_id", "interaction_id", "external_conversation_id", "external_sender_id", "reply_to_external_message_id", "direction", "external_created_at"}},
	{table: "commitment_fires", columns: []string{"target_channel", "target_conversation_id", "reply_to_message_id", "routing_mode"}},
	{table: "memory_semantic_index", columns: []string{"memory_id", "model_id", "content_hash", "vector_ref_json"}},
}

type Store struct {
	db *sql.DB
}

// UniqueExternalSender recovers an already confirmed channel binding without
// treating a new or ambiguous sender as the owner. The identifier is returned
// only to the composition root and is never logged.
func (s *Store) UniqueExternalSender(ctx context.Context, channelName string) (string, error) {
	channelName = strings.TrimSpace(channelName)
	if channelName == "" {
		return "", fmt.Errorf("channel name is required")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT external_sender_id
		FROM channel_messages
		WHERE channel = ? AND direction = 'inbound'
			AND external_sender_id IS NOT NULL AND external_sender_id <> ''
		LIMIT 2`, channelName)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var senders []string
	for rows.Next() {
		var sender string
		if err := rows.Scan(&sender); err != nil {
			return "", err
		}
		senders = append(senders, sender)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if len(senders) > 1 {
		return "", fmt.Errorf("channel has multiple historical senders; explicit owner binding is required")
	}
	if len(senders) == 1 {
		return senders[0], nil
	}
	return "", nil
}

func Open(path string) (*Store, error) {
	if path != ":memory:" {
		metadataDirectory := filepath.Dir(path)
		if err := os.MkdirAll(metadataDirectory, 0o700); err != nil {
			return nil, fmt.Errorf("create metadata directory: %w", err)
		}
		if err := os.Chmod(metadataDirectory, 0o700); err != nil {
			return nil, fmt.Errorf("protect metadata directory: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// A single writer connection makes transaction ordering explicit while WAL
	// still permits external diagnostic readers.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{db: db}
	if err := store.configure(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	if err := protectSQLiteFiles(path); err != nil {
		db.Close()
		return nil, err
	}
	if err := store.initializeSchema(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	if err := protectSQLiteFiles(path); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func protectSQLiteFiles(path string) error {
	if path == ":memory:" {
		return nil
	}
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(candidate, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("protect sqlite file %s: %w", filepath.Base(candidate), err)
		}
	}
	return nil
}

func (s *Store) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, checkpointErr := s.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
	closeErr := s.db.Close()
	return errors.Join(checkpointErr, closeErr)
}

func (s *Store) configure(ctx context.Context) error {
	for _, statement := range []string{
		"PRAGMA busy_timeout = 5000",
		"PRAGMA journal_mode = WAL",
		"PRAGMA foreign_keys = ON",
		"PRAGMA synchronous = NORMAL",
	} {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("configure sqlite with %q: %w", statement, err)
		}
	}
	return nil
}

func (s *Store) initializeSchema(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read sqlite schema version: %w", err)
	}
	if version == schemaVersion {
		return s.validateAuthoritativeSchema(ctx)
	}
	if version != 0 {
		return fmt.Errorf("unsupported sqlite schema version %d; reset the pre-release Eri data directory", version)
	}
	var existingTables int
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM sqlite_schema
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`).Scan(&existingTables); err != nil {
		return fmt.Errorf("inspect sqlite schema: %w", err)
	}
	if existingTables != 0 {
		return fmt.Errorf("unversioned pre-release sqlite schema detected; reset the Eri data directory")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin sqlite schema initialization: %w", err)
	}
	defer tx.Rollback()
	for _, statement := range schemaStatements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize sqlite schema: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
		return fmt.Errorf("record sqlite schema version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit sqlite schema initialization: %w", err)
	}
	return s.validateAuthoritativeSchema(ctx)
}

func (s *Store) validateAuthoritativeSchema(ctx context.Context) error {
	for _, requirement := range authoritativeSchemaRequirements {
		rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(`+requirement.table+`)`)
		if err != nil {
			return fmt.Errorf("inspect sqlite table %s: %w", requirement.table, err)
		}
		columns := make(map[string]struct{})
		for rows.Next() {
			var position, notNull, primaryKey int
			var name, dataType string
			var defaultValue any
			if err := rows.Scan(&position, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
				rows.Close()
				return fmt.Errorf("inspect sqlite table %s: %w", requirement.table, err)
			}
			columns[name] = struct{}{}
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("inspect sqlite table %s: %w", requirement.table, err)
		}
		for _, column := range requirement.columns {
			if _, exists := columns[column]; !exists {
				return fmt.Errorf("stale pre-release sqlite schema: %s.%s is missing; reset the Eri data directory", requirement.table, column)
			}
		}
	}
	return nil
}

func (s *Store) CreateInbound(ctx context.Context, sourceChannel string, ref content.Ref, attachments []channel.AttachmentRecord) (channel.SendResult, error) {
	result, _, err := s.createInbound(ctx, sourceChannel, channel.ExternalInteraction{}, ref, attachments)
	return result, err
}

func (s *Store) CreateExternalInbound(ctx context.Context, sourceChannel string, external channel.ExternalInteraction, ref content.Ref, attachments []channel.AttachmentRecord) (channel.SendResult, bool, error) {
	return s.createInbound(ctx, sourceChannel, external, ref, attachments)
}

func (s *Store) createInbound(ctx context.Context, sourceChannel string, external channel.ExternalInteraction, ref content.Ref, attachments []channel.AttachmentRecord) (channel.SendResult, bool, error) {
	interactionID, err := identifier.New()
	if err != nil {
		return channel.SendResult{}, false, err
	}
	taskID, err := identifier.New()
	if err != nil {
		return channel.SendResult{}, false, err
	}
	now := time.Now().UTC()
	if external.MessageID != "" && external.CreatedAt.IsZero() {
		external.CreatedAt = now
	}
	if sourceChannel == "" {
		sourceChannel = "conversation_web"
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return channel.SendResult{}, false, err
	}
	defer tx.Rollback()
	if external.MessageID != "" {
		var existing channel.SendResult
		err := tx.QueryRowContext(ctx, `
			SELECT i.id, i.task_id
			FROM channel_messages cm JOIN interactions i ON i.id = cm.interaction_id
			WHERE cm.channel = ? AND cm.external_message_id = ?`, sourceChannel, external.MessageID).
			Scan(&existing.InteractionID, &existing.TaskID)
		if err == nil {
			return existing, false, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return channel.SendResult{}, false, err
		}
	}
	var erasurePending int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM data_erasure_jobs WHERE status IN ('awaiting_delivery', 'ready')`).Scan(&erasurePending); err != nil {
		return channel.SendResult{}, false, err
	}
	if erasurePending > 0 {
		return channel.SendResult{}, false, fmt.Errorf("user data erasure is in progress; wait for Eri to return to a clean state")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO conversations(id, created_at) VALUES(?, ?) ON CONFLICT(id) DO NOTHING`, channel.ConversationID, formatTime(now)); err != nil {
		return channel.SendResult{}, false, err
	}
	joinedActiveTask := false
	var activeTaskID string
	err = tx.QueryRowContext(ctx, `
		SELECT t.id
		FROM tasks t
		WHERE t.conversation_id = ? AND t.cancel_requested = 0 AND (
			t.status = 'running' AND EXISTS (
					SELECT 1 FROM invocations i
					WHERE i.task_id = t.id AND i.kind = 'model' AND i.status = 'dispatched'
				)
		)
		ORDER BY t.updated_at DESC, t.created_at DESC
		LIMIT 1`, channel.ConversationID).Scan(&activeTaskID)
	if err == nil {
		taskID = activeTaskID
		joinedActiveTask = true
	} else if !errors.Is(err, sql.ErrNoRows) {
		return channel.SendResult{}, false, err
	}
	if err := insertContentRef(ctx, tx, ref, now); err != nil {
		return channel.SendResult{}, false, err
	}
	encodedRef, err := json.Marshal(ref)
	if err != nil {
		return channel.SendResult{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO interactions(id, conversation_id, task_id, direction, role, kind, channel, content_ref_json, created_at)
		VALUES(?, ?, ?, 'inbound', 'user', 'text', ?, ?, ?)`,
		interactionID, channel.ConversationID, taskID, sourceChannel, string(encodedRef), formatTime(now)); err != nil {
		return channel.SendResult{}, false, err
	}
	if external.MessageID != "" {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO channel_messages(
				channel, external_message_id, interaction_id, external_conversation_id,
				external_sender_id, reply_to_external_message_id, direction, external_created_at, created_at
			) VALUES(?, ?, ?, ?, ?, NULLIF(?, ''), 'inbound', ?, ?)`,
			sourceChannel, external.MessageID, interactionID, external.ConversationID, external.SenderID,
			external.ReplyToMessageID, formatTime(external.CreatedAt), formatTime(now)); err != nil {
			return channel.SendResult{}, false, err
		}
	}
	for _, attachment := range attachments {
		if err := insertContentRef(ctx, tx, attachment.ContentRef, now); err != nil {
			return channel.SendResult{}, false, err
		}
		encodedAttachmentRef, err := json.Marshal(attachment.ContentRef)
		if err != nil {
			return channel.SendResult{}, false, err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO attachments(id, interaction_id, name, media_type, size_bytes, content_ref_json, created_at)
			VALUES(?, ?, ?, ?, ?, ?, ?)`, attachment.ID, interactionID, attachment.Name,
			attachment.MediaType, attachment.SizeBytes, string(encodedAttachmentRef), formatTime(now)); err != nil {
			return channel.SendResult{}, false, err
		}
	}
	if err := appendEvent(ctx, tx, "interaction", interactionID, "interaction.received", map[string]any{"task_id": taskID, "channel": sourceChannel}, now); err != nil {
		return channel.SendResult{}, false, err
	}
	if joinedActiveTask {
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET source_channel = ?, version = version + 1, updated_at = ? WHERE id = ?`, sourceChannel, formatTime(now), taskID); err != nil {
			return channel.SendResult{}, false, err
		}
		if err := appendEvent(ctx, tx, "task", taskID, "task.input_joined", map[string]any{"interaction_id": interactionID, "channel": sourceChannel}, now); err != nil {
			return channel.SendResult{}, false, err
		}
	} else {
		outboxID, err := identifier.New()
		if err != nil {
			return channel.SendResult{}, false, err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO tasks(id, conversation_id, source_interaction_id, source_channel, status, terminal_status, version, created_at, updated_at)
			VALUES(?, ?, ?, ?, 'queued', '', 1, ?, ?)`,
			taskID, channel.ConversationID, interactionID, sourceChannel, formatTime(now), formatTime(now)); err != nil {
			return channel.SendResult{}, false, err
		}
		if err := appendEvent(ctx, tx, "task", taskID, "task.created", map[string]any{"status": "queued"}, now); err != nil {
			return channel.SendResult{}, false, err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO internal_outbox(id, kind, aggregate_id, status, attempts, available_at, created_at, updated_at)
			VALUES(?, 'task.wake', ?, 'pending', 0, ?, ?, ?)`,
			outboxID, taskID, formatTime(now), formatTime(now), formatTime(now)); err != nil {
			return channel.SendResult{}, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return channel.SendResult{}, false, err
	}
	return channel.SendResult{InteractionID: interactionID, TaskID: taskID}, true, nil
}

func (s *Store) EnsureIntroduction(ctx context.Context, sourceChannel string, ref content.Ref) (channel.ConnectResult, error) {
	if sourceChannel == "" {
		sourceChannel = "conversation_web"
	}
	interactionID, err := identifier.New()
	if err != nil {
		return channel.ConnectResult{}, err
	}
	taskID, err := identifier.New()
	if err != nil {
		return channel.ConnectResult{}, err
	}
	outboxID, err := identifier.New()
	if err != nil {
		return channel.ConnectResult{}, err
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return channel.ConnectResult{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO conversations(id, created_at) VALUES(?, ?) ON CONFLICT(id) DO NOTHING`, channel.ConversationID, formatTime(now)); err != nil {
		return channel.ConnectResult{}, err
	}
	var existingTaskID string
	if err := tx.QueryRowContext(ctx, `SELECT task_id FROM conversation_introductions WHERE conversation_id = ?`, channel.ConversationID).Scan(&existingTaskID); err == nil {
		return channel.ConnectResult{IntroductionStarted: false, TaskID: existingTaskID}, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return channel.ConnectResult{}, err
	}
	if err := insertContentRef(ctx, tx, ref, now); err != nil {
		return channel.ConnectResult{}, err
	}
	encodedRef, err := json.Marshal(ref)
	if err != nil {
		return channel.ConnectResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO interactions(id, conversation_id, task_id, direction, role, kind, channel, content_ref_json, created_at)
		VALUES(?, ?, ?, 'inbound', 'system', 'internal_trigger', 'introduction', ?, ?)`,
		interactionID, channel.ConversationID, taskID, string(encodedRef), formatTime(now)); err != nil {
		return channel.ConnectResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tasks(id, conversation_id, source_interaction_id, source_channel, status, terminal_status, version, created_at, updated_at)
		VALUES(?, ?, ?, ?, 'queued', '', 1, ?, ?)`,
		taskID, channel.ConversationID, interactionID, sourceChannel, formatTime(now), formatTime(now)); err != nil {
		return channel.ConnectResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO conversation_introductions(conversation_id, task_id, requested_at) VALUES(?, ?, ?)`, channel.ConversationID, taskID, formatTime(now)); err != nil {
		return channel.ConnectResult{}, err
	}
	if err := appendEvent(ctx, tx, "conversation", channel.ConversationID, "conversation.introduction.requested", map[string]any{
		"task_id": taskID, "source_channel": sourceChannel,
	}, now); err != nil {
		return channel.ConnectResult{}, err
	}
	if err := appendEvent(ctx, tx, "task", taskID, "task.created", map[string]any{"status": "queued", "trigger": "first_connection"}, now); err != nil {
		return channel.ConnectResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO internal_outbox(id, kind, aggregate_id, status, attempts, available_at, created_at, updated_at)
		VALUES(?, 'task.wake', ?, 'pending', 0, ?, ?, ?)`,
		outboxID, taskID, formatTime(now), formatTime(now), formatTime(now)); err != nil {
		return channel.ConnectResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return channel.ConnectResult{}, err
	}
	return channel.ConnectResult{IntroductionStarted: true, TaskID: taskID}, nil
}

func (s *Store) ListMessages(ctx context.Context, after int64, limit int) ([]channel.MessageRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT sequence, id, task_id, direction, role, kind, channel, content_ref_json,
		       COALESCE(artifact_id, ''), COALESCE(delivery_id, ''), COALESCE(receipt, ''), created_at
		FROM interactions WHERE conversation_id = ? AND sequence > ? AND kind != 'internal_trigger'
		ORDER BY sequence ASC LIMIT ?`, channel.ConversationID, after, limit)
	if err != nil {
		return nil, err
	}
	return s.scanMessages(ctx, rows, false)
}

func (s *Store) ListMessagesBefore(ctx context.Context, before int64, limit int) ([]channel.MessageRecord, error) {
	condition := ""
	arguments := []any{channel.ConversationID}
	if before > 0 {
		condition = "AND sequence < ?"
		arguments = append(arguments, before)
	}
	arguments = append(arguments, limit)
	rows, err := s.db.QueryContext(ctx, `
		SELECT sequence, id, task_id, direction, role, kind, channel, content_ref_json,
		       COALESCE(artifact_id, ''), COALESCE(delivery_id, ''), COALESCE(receipt, ''), created_at
		FROM interactions WHERE conversation_id = ? `+condition+` AND kind != 'internal_trigger'
		ORDER BY sequence DESC LIMIT ?`, arguments...)
	if err != nil {
		return nil, err
	}
	return s.scanMessages(ctx, rows, true)
}

func (s *Store) ListMessagesForTask(ctx context.Context, taskID string) ([]channel.MessageRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT sequence, id, task_id, direction, role, kind, channel, content_ref_json,
		       COALESCE(artifact_id, ''), COALESCE(delivery_id, ''), COALESCE(receipt, ''), created_at
		FROM interactions
		WHERE conversation_id = ? AND task_id = ? AND kind != 'internal_trigger'
		ORDER BY sequence ASC`, channel.ConversationID, taskID)
	if err != nil {
		return nil, err
	}
	return s.scanMessages(ctx, rows, false)
}

func (s *Store) scanMessages(ctx context.Context, rows *sql.Rows, reverse bool) ([]channel.MessageRecord, error) {
	records := make([]channel.MessageRecord, 0)
	for rows.Next() {
		var record channel.MessageRecord
		var encodedRef, created string
		if err := rows.Scan(&record.Sequence, &record.ID, &record.TaskID, &record.Direction, &record.Role, &record.Kind, &record.Channel, &encodedRef, &record.ArtifactID, &record.DeliveryID, &record.Receipt, &created); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(encodedRef), &record.ContentRef); err != nil {
			return nil, fmt.Errorf("decode message content ref: %w", err)
		}
		parsed, err := parseTime(created)
		if err != nil {
			return nil, err
		}
		record.CreatedAt = parsed
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if reverse {
		slices.Reverse(records)
	}
	for index := range records {
		attachments, err := s.listInteractionAttachments(ctx, records[index].ID)
		if err != nil {
			return nil, err
		}
		records[index].Attachments = attachments
	}
	return records, nil
}

func (s *Store) listInteractionAttachments(ctx context.Context, interactionID string) ([]channel.AttachmentRecord, error) {
	return listInteractionAttachments(ctx, s.db, interactionID)
}

func listInteractionAttachments(ctx context.Context, queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, interactionID string) ([]channel.AttachmentRecord, error) {
	rows, err := queryer.QueryContext(ctx, `
		SELECT id, name, media_type, size_bytes, content_ref_json
		FROM attachments WHERE interaction_id = ? ORDER BY created_at, id`, interactionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	attachments := make([]channel.AttachmentRecord, 0)
	for rows.Next() {
		var record channel.AttachmentRecord
		var encodedRef string
		if err := rows.Scan(&record.ID, &record.Name, &record.MediaType, &record.SizeBytes, &encodedRef); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(encodedRef), &record.ContentRef); err != nil {
			return nil, err
		}
		attachments = append(attachments, record)
	}
	return attachments, rows.Err()
}

func (s *Store) LoadAttachment(ctx context.Context, id string) (channel.AttachmentRecord, bool, error) {
	var record channel.AttachmentRecord
	var encodedRef string
	err := s.db.QueryRowContext(ctx, `SELECT id, name, media_type, size_bytes, content_ref_json FROM attachments WHERE id = ?`, id).
		Scan(&record.ID, &record.Name, &record.MediaType, &record.SizeBytes, &encodedRef)
	if errors.Is(err, sql.ErrNoRows) {
		return channel.AttachmentRecord{}, false, nil
	}
	if err != nil {
		return channel.AttachmentRecord{}, false, err
	}
	if err := json.Unmarshal([]byte(encodedRef), &record.ContentRef); err != nil {
		return channel.AttachmentRecord{}, false, err
	}
	return record, true, nil
}

func (s *Store) TaskStatus(ctx context.Context, id string) (channel.TaskStatus, error) {
	var status channel.TaskStatus
	var updated string
	err := s.db.QueryRowContext(ctx, `SELECT id, status, updated_at, COALESCE(error_code, '') FROM tasks WHERE id = ?`, id).Scan(&status.ID, &status.Status, &updated, &status.ErrorCode)
	if err != nil {
		return channel.TaskStatus{}, err
	}
	status.UpdatedAt, err = parseTime(updated)
	return status, err
}

func (s *Store) ApprovalStatus(ctx context.Context, id string) (string, error) {
	var status string
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM approvals WHERE id = ?`, id).Scan(&status); err != nil {
		return "", err
	}
	return status, nil
}

func (s *Store) Presence(ctx context.Context) (channel.Presence, error) {
	var active int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE status IN ('queued', 'running')`).Scan(&active); err != nil {
		return channel.Presence{}, err
	}
	state := "available"
	if active > 0 {
		state = "working"
	}
	return channel.Presence{State: state, ActiveTasks: active}, nil
}

func (s *Store) ListEvents(ctx context.Context, after int64, limit int) ([]eventlog.Event, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT sequence, id, aggregate_type, aggregate_id, type, payload_json, created_at
		FROM events WHERE sequence > ? ORDER BY sequence ASC LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := make([]eventlog.Event, 0)
	for rows.Next() {
		var event eventlog.Event
		var payload, created string
		if err := rows.Scan(&event.Sequence, &event.ID, &event.AggregateType, &event.AggregateID, &event.Type, &payload, &created); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(payload), &event.Data); err != nil {
			return nil, err
		}
		event.Time, err = parseTime(created)
		if err != nil {
			return nil, err
		}
		eventlog.Normalize(&event)
		events = append(events, event)
	}
	return events, rows.Err()
}

func (s *Store) ClaimTask(ctx context.Context, taskID, owner string, lease time.Duration, soulVersion, manifest, modelTarget string) (agent.TaskContext, bool, error) {
	now := time.Now().UTC()
	if modelTarget == "" {
		modelTarget = "model"
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return agent.TaskContext{}, false, err
	}
	defer tx.Rollback()
	var currentStatus, leaseUntil, sourceChannel, objectiveRefJSON, scheduledFor string
	currentTask := execution.TaskCapsule{TaskID: taskID}
	err = tx.QueryRowContext(ctx, `
		SELECT t.status, COALESCE(t.lease_until, ''), t.source_channel,
			t.source_interaction_id, source.kind, source.role, source.channel, source.content_ref_json,
			COALESCE(f.commitment_id, ''), COALESCE(f.scheduled_for, '')
		FROM tasks t
		JOIN interactions source ON source.id = t.source_interaction_id
		LEFT JOIN commitment_fires f ON f.task_id = t.id
		WHERE t.id = ?`, taskID).Scan(
		&currentStatus, &leaseUntil, &sourceChannel,
		&currentTask.SourceInteractionID, &currentTask.SourceKind, &currentTask.SourceRole,
		&currentTask.TriggerChannel, &objectiveRefJSON,
		&currentTask.CommitmentID, &scheduledFor,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return agent.TaskContext{}, false, nil
	}
	if err != nil {
		return agent.TaskContext{}, false, err
	}
	var objectiveRef content.Ref
	if err := json.Unmarshal([]byte(objectiveRefJSON), &objectiveRef); err != nil {
		return agent.TaskContext{}, false, fmt.Errorf("decode task objective ref: %w", err)
	}
	if scheduledFor != "" {
		currentTask.ScheduledFor, err = parseTime(scheduledFor)
		if err != nil {
			return agent.TaskContext{}, false, fmt.Errorf("parse task scheduled time: %w", err)
		}
	}
	if currentTask.CommitmentID != "" {
		currentTask.TriggerEvent = execution.TriggerEventCommitmentDue
		currentTask.TriggerState = execution.TriggerStateOccurred
	}
	recovering := currentStatus == "running" && leaseUntil != "" && leaseUntil < formatTime(now)
	if currentStatus != "queued" && !recovering {
		return agent.TaskContext{}, false, nil
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE tasks SET status = 'running', version = version + 1, lease_owner = ?, lease_until = ?, updated_at = ?
		WHERE id = ? AND (status = 'queued' OR (status = 'running' AND lease_until < ?))`,
		owner, formatTime(now.Add(lease)), formatTime(now), taskID, formatTime(now))
	if err != nil {
		return agent.TaskContext{}, false, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return agent.TaskContext{}, false, err
	}
	if count == 0 {
		return agent.TaskContext{}, false, nil
	}
	var runID, invocationID string
	if recovering {
		err = tx.QueryRowContext(ctx, `
			SELECT r.id, i.id FROM runs r JOIN invocations i ON i.run_id = r.id AND i.kind = 'model'
			WHERE r.task_id = ? AND r.status = 'active' ORDER BY i.created_at DESC LIMIT 1`, taskID).Scan(&runID, &invocationID)
		if err != nil {
			return agent.TaskContext{}, false, fmt.Errorf("recover active run: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE steps SET status = 'running', updated_at = ? WHERE run_id = ? AND status IN ('running', 'waiting')`, formatTime(now), runID); err != nil {
			return agent.TaskContext{}, false, err
		}
		if err := appendEvent(ctx, tx, "task", taskID, "task.recovered", map[string]any{"run_id": runID, "invocation_id": invocationID}, now); err != nil {
			return agent.TaskContext{}, false, err
		}
	} else {
		runID, err = identifier.New()
		if err != nil {
			return agent.TaskContext{}, false, err
		}
		stepID, err := identifier.New()
		if err != nil {
			return agent.TaskContext{}, false, err
		}
		invocationID, err = identifier.New()
		if err != nil {
			return agent.TaskContext{}, false, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO runs(id, task_id, status, soul_version, started_at) VALUES(?, ?, 'active', ?, ?)`, runID, taskID, soulVersion, formatTime(now)); err != nil {
			return agent.TaskContext{}, false, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO steps(id, run_id, task_id, kind, status, created_at, updated_at) VALUES(?, ?, ?, 'model', 'running', ?, ?)`, stepID, runID, taskID, formatTime(now), formatTime(now)); err != nil {
			return agent.TaskContext{}, false, err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO invocations(id, run_id, task_id, step_id, kind, status, target, context_manifest_json, created_at, updated_at)
			VALUES(?, ?, ?, ?, 'model', 'planned', ?, ?, ?, ?)`, invocationID, runID, taskID, stepID, modelTarget, manifest, formatTime(now), formatTime(now)); err != nil {
			return agent.TaskContext{}, false, err
		}
		if err := appendEvent(ctx, tx, "task", taskID, "task.started", map[string]any{"run_id": runID}, now); err != nil {
			return agent.TaskContext{}, false, err
		}
		if err := appendEvent(ctx, tx, "invocation", invocationID, "invocation.planned", map[string]any{"kind": "model", "run_id": runID}, now); err != nil {
			return agent.TaskContext{}, false, err
		}
	}
	firstSequence := int64(0)
	var checkpointID, checkpointRefJSON string
	err = tx.QueryRowContext(ctx, `
		SELECT id, summary_ref_json, first_kept_sequence
		FROM context_checkpoints ORDER BY created_at DESC LIMIT 1`).Scan(&checkpointID, &checkpointRefJSON, &firstSequence)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return agent.TaskContext{}, false, err
	}
	var reversed []agent.ContextRecord
	if err == nil {
		var checkpointRef content.Ref
		if err := json.Unmarshal([]byte(checkpointRefJSON), &checkpointRef); err != nil {
			return agent.TaskContext{}, false, err
		}
		reversed = append(reversed, agent.ContextRecord{
			ID: checkpointID, Kind: "context_checkpoint", Role: "system", ContentRef: checkpointRef,
		})
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT sequence, id, kind, role, content_ref_json FROM interactions
		WHERE conversation_id = ? AND sequence >= ?
			AND (kind != 'internal_trigger' OR id = (SELECT source_interaction_id FROM tasks WHERE id = ?))
		ORDER BY sequence DESC`, channel.ConversationID, firstSequence, taskID)
	if err != nil {
		return agent.TaskContext{}, false, err
	}
	for rows.Next() {
		var record agent.ContextRecord
		var encodedRef string
		if err := rows.Scan(&record.Sequence, &record.ID, &record.Kind, &record.Role, &encodedRef); err != nil {
			rows.Close()
			return agent.TaskContext{}, false, err
		}
		if err := json.Unmarshal([]byte(encodedRef), &record.ContentRef); err != nil {
			rows.Close()
			return agent.TaskContext{}, false, err
		}
		reversed = append(reversed, record)
	}
	if err := rows.Close(); err != nil {
		return agent.TaskContext{}, false, err
	}
	messages := make([]agent.ContextRecord, 0, len(reversed))
	if checkpointID != "" {
		messages = append(messages, reversed[0])
		reversed = reversed[1:]
	}
	for index := len(reversed) - 1; index >= 0; index-- {
		messages = append(messages, reversed[index])
	}
	for index := range messages {
		attachments, err := listInteractionAttachments(ctx, tx, messages[index].ID)
		if err != nil {
			return agent.TaskContext{}, false, err
		}
		for _, attachment := range attachments {
			messages[index].Attachments = append(messages[index].Attachments, agent.ContextAttachment{
				ID: attachment.ID, Name: attachment.Name, MediaType: attachment.MediaType,
				SizeBytes: attachment.SizeBytes, ContentRef: attachment.ContentRef,
			})
		}
	}
	var checkpointPhase, agentCheckpointRefJSON string
	err = tx.QueryRowContext(ctx, `
		SELECT phase, state_ref_json FROM agent_checkpoints
		WHERE task_id = ? AND run_id = ? AND status = 'active'
		ORDER BY updated_at DESC LIMIT 1`, taskID, runID).Scan(&checkpointPhase, &agentCheckpointRefJSON)
	var agentCheckpointRef content.Ref
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return agent.TaskContext{}, false, err
	}
	if err == nil {
		if err := json.Unmarshal([]byte(agentCheckpointRefJSON), &agentCheckpointRef); err != nil {
			return agent.TaskContext{}, false, err
		}
	}
	var inputSequence int64
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(sequence), 0) FROM interactions
		WHERE task_id = ? AND direction = 'inbound'`, taskID).Scan(&inputSequence); err != nil {
		return agent.TaskContext{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return agent.TaskContext{}, false, err
	}
	return agent.TaskContext{
		TaskID: taskID, RunID: runID, InvocationID: invocationID, SourceChannel: sourceChannel,
		InputSequence: inputSequence, Messages: messages,
		CheckpointRef: agentCheckpointRef, CheckpointPhase: checkpointPhase,
		CurrentTask: currentTask, ObjectiveRef: objectiveRef,
	}, true, nil
}

func (s *Store) LoadTaskInputsAfter(ctx context.Context, taskID string, afterSequence int64) ([]agent.ContextRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT sequence, id, kind, role, content_ref_json
		FROM interactions
		WHERE task_id = ? AND direction = 'inbound' AND sequence > ?
		ORDER BY sequence`, taskID, afterSequence)
	if err != nil {
		return nil, err
	}
	records := make([]agent.ContextRecord, 0)
	for rows.Next() {
		var record agent.ContextRecord
		var encodedRef string
		if err := rows.Scan(&record.Sequence, &record.ID, &record.Kind, &record.Role, &encodedRef); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(encodedRef), &record.ContentRef); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for index := range records {
		attachments, err := listInteractionAttachments(ctx, s.db, records[index].ID)
		if err != nil {
			return nil, err
		}
		for _, attachment := range attachments {
			records[index].Attachments = append(records[index].Attachments, agent.ContextAttachment{
				ID: attachment.ID, Name: attachment.Name, MediaType: attachment.MediaType,
				SizeBytes: attachment.SizeBytes, ContentRef: attachment.ContentRef,
			})
		}
	}
	return records, nil
}

func (s *Store) SaveContextCheckpoint(ctx context.Context, taskID, invocationID string, checkpoint agent.ContextCheckpoint) error {
	now := time.Now().UTC()
	encodedRef, err := json.Marshal(checkpoint.SummaryRef)
	if err != nil {
		return fmt.Errorf("encode context checkpoint ref: %w", err)
	}
	encodedSourceIDs, err := json.Marshal(checkpoint.SourceIDs)
	if err != nil {
		return fmt.Errorf("encode context checkpoint sources: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := insertContentRef(ctx, tx, checkpoint.SummaryRef, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO context_checkpoints(
			id, task_id, invocation_id, summary_ref_json, source_ids_json, first_kept_sequence,
			summarized_count, tokens_before, tokens_after, created_at
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		checkpoint.ID, taskID, invocationID, string(encodedRef), string(encodedSourceIDs), checkpoint.FirstKeptSequence,
		checkpoint.SummarizedCount, checkpoint.TokensBefore, checkpoint.TokensAfter, formatTime(now)); err != nil {
		return err
	}
	if err := appendEvent(ctx, tx, "invocation", invocationID, "context.compacted", map[string]any{
		"checkpoint_id": checkpoint.ID, "summarized_count": checkpoint.SummarizedCount,
		"first_kept_sequence": checkpoint.FirstKeptSequence,
		"tokens_before":       checkpoint.TokensBefore, "tokens_after": checkpoint.TokensAfter,
	}, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SaveAgentCheckpoint(ctx context.Context, task agent.TaskContext, phase string, stateRef content.Ref) error {
	if phase != "ready_for_model" && phase != "model_received" && phase != "candidate_received" {
		return fmt.Errorf("unsupported agent checkpoint phase %q", phase)
	}
	checkpointID, err := identifier.New()
	if err != nil {
		return err
	}
	encodedRef, err := json.Marshal(stateRef)
	if err != nil {
		return fmt.Errorf("encode agent checkpoint ref: %w", err)
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := insertContentRef(ctx, tx, stateRef, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE agent_checkpoints SET status = 'superseded', updated_at = ?
		WHERE task_id = ? AND status = 'active'`, formatTime(now), task.TaskID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO agent_checkpoints(
			id, task_id, run_id, invocation_id, phase, state_ref_json, status, created_at, updated_at
		) VALUES(?, ?, ?, ?, ?, ?, 'active', ?, ?)`,
		checkpointID, task.TaskID, task.RunID, task.InvocationID, phase, string(encodedRef), formatTime(now), formatTime(now)); err != nil {
		return err
	}
	if err := appendEvent(ctx, tx, "invocation", task.InvocationID, "agent.checkpoint.saved", map[string]any{
		"checkpoint_id": checkpointID, "phase": phase,
	}, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) MarkInvocationDispatched(ctx context.Context, invocationID string) error {
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM invocations WHERE id = ?`, invocationID).Scan(&status); err != nil {
		return err
	}
	if status == "dispatched" {
		return tx.Commit()
	}
	result, err := tx.ExecContext(ctx, `UPDATE invocations SET status = 'dispatched', updated_at = ? WHERE id = ? AND status = 'planned'`, formatTime(now), invocationID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("invocation %s is not planned", invocationID)
	}
	if err := appendEvent(ctx, tx, "invocation", invocationID, "invocation.dispatched", map[string]any{"kind": "model"}, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) UpdateInvocationContext(ctx context.Context, invocationID, manifest string) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE invocations SET context_manifest_json = ?, updated_at = ?
		WHERE id = ? AND status IN ('planned', 'dispatched')`, manifest, formatTime(time.Now().UTC()), invocationID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("invocation %s cannot update context", invocationID)
	}
	return nil
}

func (s *Store) CommitArtifact(ctx context.Context, commit agent.Commit) error {
	now := time.Now().UTC()
	if commit.EvalFindingsRef.ObjectID == "" {
		return fmt.Errorf("Eval findings reference is required")
	}
	encodedRef, err := json.Marshal(commit.ArtifactRef)
	if err != nil {
		return err
	}
	traceRef, err := json.Marshal(commit.TraceRef)
	if err != nil {
		return err
	}
	usage, err := json.Marshal(commit.Usage)
	if err != nil {
		return err
	}
	findingsRef, err := json.Marshal(commit.EvalFindingsRef)
	if err != nil {
		return err
	}
	if commit.EvalTier == "" {
		commit.EvalTier = "routine"
	}
	if commit.EvalEvaluator == "" {
		commit.EvalEvaluator = "deterministic"
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if commit.BasisInputSequence > 0 {
		var latestInputSequence int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence), 0) FROM interactions WHERE task_id = ? AND direction = 'inbound'`, commit.TaskID).Scan(&latestInputSequence); err != nil {
			return err
		}
		if latestInputSequence > commit.BasisInputSequence {
			return agent.ErrStaleTaskInput
		}
	}
	var existing int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifacts WHERE id = ?`, commit.ArtifactID).Scan(&existing); err != nil {
		return err
	}
	if existing > 0 {
		return nil
	}
	invocationStatus := "succeeded"
	stepStatus := "succeeded"
	invocationEvent := "invocation.succeeded"
	if commit.TerminalStatus == "failed" {
		invocationStatus = "failed"
		stepStatus = "failed"
		invocationEvent = "invocation.failed"
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE invocations SET status = ?, usage_json = ?, error_code = ?, updated_at = ?
		WHERE id = ? AND status IN ('planned', 'dispatched')`,
		invocationStatus, string(usage), commit.FailureCode, formatTime(now), commit.InvocationID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("invocation %s cannot commit result", commit.InvocationID)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE steps SET status = ?, updated_at = ? WHERE run_id = ? AND kind = 'model' AND status = 'running'`, stepStatus, formatTime(now), commit.RunID); err != nil {
		return err
	}
	if err := insertContentRef(ctx, tx, commit.ArtifactRef, now); err != nil {
		return err
	}
	if err := insertContentRef(ctx, tx, commit.TraceRef, now); err != nil {
		return err
	}
	if err := insertContentRef(ctx, tx, commit.EvalFindingsRef, now); err != nil {
		return err
	}
	var artifactVersion int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) + 1 FROM artifacts WHERE task_id = ?`, commit.TaskID).Scan(&artifactVersion); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO artifacts(id, task_id, run_id, version, kind, content_ref_json, status, trace_ref_json, created_at)
		VALUES(?, ?, ?, ?, ?, ?, 'approved', ?, ?)`,
		commit.ArtifactID, commit.TaskID, commit.RunID, artifactVersion, commit.ArtifactKind, string(encodedRef), string(traceRef), formatTime(now)); err != nil {
		return err
	}
	for _, attachment := range commit.Attachments {
		if err := insertContentRef(ctx, tx, attachment.ContentRef, now); err != nil {
			return err
		}
		encodedAttachmentRef, err := json.Marshal(attachment.ContentRef)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO artifact_attachments(id, artifact_id, name, media_type, size_bytes, content_ref_json, created_at)
			VALUES(?, ?, ?, ?, ?, ?, ?)`, attachment.ID, commit.ArtifactID, attachment.Name, attachment.MediaType,
			attachment.SizeBytes, string(encodedAttachmentRef), formatTime(now)); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO eval_records(id, artifact_id, tier, evaluator, result, findings_ref_json, finding_count, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		commit.EvalID, commit.ArtifactID, commit.EvalTier, commit.EvalEvaluator, string(commit.EvalResult), string(findingsRef), len(commit.EvalFindings), formatTime(now)); err != nil {
		return err
	}
	var sourceChannel string
	if err := tx.QueryRowContext(ctx, `SELECT source_channel FROM tasks WHERE id = ?`, commit.TaskID).Scan(&sourceChannel); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO deliveries(id, task_id, artifact_id, target_channel, status, receipt, idempotency_key, terminal_status, created_at, updated_at)
		VALUES(?, ?, ?, ?, 'queued', '', ?, ?, ?, ?)`,
		commit.DeliveryID, commit.TaskID, commit.ArtifactID, sourceChannel, commit.ArtifactID+":"+sourceChannel, commit.TerminalStatus, formatTime(now), formatTime(now)); err != nil {
		return err
	}
	outboxID, err := identifier.New()
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO internal_outbox(id, kind, aggregate_id, status, attempts, available_at, created_at, updated_at)
		VALUES(?, 'delivery.send', ?, 'pending', 0, ?, ?, ?)`,
		outboxID, commit.DeliveryID, formatTime(now), formatTime(now), formatTime(now)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE tasks SET status = 'waiting', wait_reason = 'delivery', terminal_status = ?, error_code = ?,
		lease_owner = NULL, lease_until = NULL, version = version + 1, updated_at = ? WHERE id = ?`,
		commit.TerminalStatus, commit.FailureCode, formatTime(now), commit.TaskID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE agent_checkpoints SET status = 'completed', updated_at = ?
		WHERE task_id = ? AND status = 'active'`, formatTime(now), commit.TaskID); err != nil {
		return err
	}
	if err := appendEvent(ctx, tx, "invocation", commit.InvocationID, invocationEvent, map[string]any{
		"run_id": commit.RunID, "error_code": commit.FailureCode,
		"provider": commit.Usage.Provider, "model": commit.Usage.Model,
		"model_calls":  commit.Usage.ModelCalls,
		"input_tokens": commit.Usage.InputTokens, "output_tokens": commit.Usage.OutputTokens,
		"cache_hit_tokens": commit.Usage.CacheHitTokens, "cache_miss_tokens": commit.Usage.CacheMissTokens,
		"reasoning_tokens": commit.Usage.ReasoningTokens, "duration_ms": commit.Usage.DurationMillis,
	}, now); err != nil {
		return err
	}
	if err := appendEvent(ctx, tx, "artifact", commit.ArtifactID, "artifact.evaluated", map[string]any{
		"eval_id": commit.EvalID, "result": commit.EvalResult, "tier": commit.EvalTier,
		"evaluator": commit.EvalEvaluator, "finding_count": len(commit.EvalFindings),
	}, now); err != nil {
		return err
	}
	if err := appendEvent(ctx, tx, "delivery", commit.DeliveryID, "delivery.queued", map[string]any{"artifact_id": commit.ArtifactID, "channel": sourceChannel}, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CommitProgress(ctx context.Context, progress agent.ProgressCommit) (bool, error) {
	commit := progress.Commit
	if commit.ArtifactKind != "progress" || commit.EvalResult != eval.Pass || commit.EvalFindingsRef.ObjectID == "" || commit.TraceRef.ObjectID == "" {
		return false, fmt.Errorf("progress delivery requires a passed evaluated artifact and trace")
	}
	now := time.Now().UTC()
	artifactRef, err := json.Marshal(commit.ArtifactRef)
	if err != nil {
		return false, err
	}
	traceRef, err := json.Marshal(commit.TraceRef)
	if err != nil {
		return false, err
	}
	findingsRef, err := json.Marshal(commit.EvalFindingsRef)
	if err != nil {
		return false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if commit.BasisInputSequence > 0 {
		var latestInputSequence int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence), 0) FROM interactions WHERE task_id = ? AND direction = 'inbound'`, commit.TaskID).Scan(&latestInputSequence); err != nil {
			return false, err
		}
		if latestInputSequence > commit.BasisInputSequence {
			return false, agent.ErrStaleTaskInput
		}
	}
	var existing int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM artifacts
		WHERE task_id = ? AND kind = 'progress' AND json_extract(content_ref_json, '$.content_hash') = ?`,
		commit.TaskID, commit.ArtifactRef.ContentHash).Scan(&existing); err != nil {
		return false, err
	}
	if existing > 0 {
		return false, nil
	}
	var sourceChannel string
	if err := tx.QueryRowContext(ctx, `SELECT source_channel FROM tasks WHERE id = ? AND status = 'running'`, commit.TaskID).Scan(&sourceChannel); err != nil {
		return false, fmt.Errorf("progress task is not running: %w", err)
	}
	for _, ref := range []content.Ref{commit.ArtifactRef, commit.TraceRef, commit.EvalFindingsRef} {
		if err := insertContentRef(ctx, tx, ref, now); err != nil {
			return false, err
		}
	}
	var version int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) + 1 FROM artifacts WHERE task_id = ?`, commit.TaskID).Scan(&version); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO artifacts(id, task_id, run_id, version, kind, content_ref_json, status, trace_ref_json, created_at)
		VALUES(?, ?, ?, ?, 'progress', ?, 'approved', ?, ?)`,
		commit.ArtifactID, commit.TaskID, commit.RunID, version, string(artifactRef), string(traceRef), formatTime(now)); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO eval_records(id, artifact_id, tier, evaluator, result, findings_ref_json, finding_count, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		commit.EvalID, commit.ArtifactID, commit.EvalTier, commit.EvalEvaluator, string(commit.EvalResult), string(findingsRef), len(commit.EvalFindings), formatTime(now)); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO deliveries(id, task_id, artifact_id, target_channel, status, receipt, idempotency_key,
			terminal_status, continue_task, created_at, updated_at)
		VALUES(?, ?, ?, ?, 'queued', '', ?, 'completed', 1, ?, ?)`,
		commit.DeliveryID, commit.TaskID, commit.ArtifactID, sourceChannel,
		commit.ArtifactID+":"+sourceChannel, formatTime(now), formatTime(now)); err != nil {
		return false, err
	}
	outboxID, err := identifier.New()
	if err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO internal_outbox(id, kind, aggregate_id, status, attempts, available_at, created_at, updated_at)
		VALUES(?, 'delivery.send', ?, 'pending', 0, ?, ?, ?)`,
		outboxID, commit.DeliveryID, formatTime(now), formatTime(now), formatTime(now)); err != nil {
		return false, err
	}
	if err := appendEvent(ctx, tx, "artifact", commit.ArtifactID, "artifact.evaluated", map[string]any{
		"eval_id": commit.EvalID, "result": commit.EvalResult, "tier": commit.EvalTier,
		"evaluator": commit.EvalEvaluator, "finding_count": len(commit.EvalFindings), "model_turn_id": progress.ModelTurnID,
	}, now); err != nil {
		return false, err
	}
	if err := appendEvent(ctx, tx, "delivery", commit.DeliveryID, "delivery.queued", map[string]any{
		"artifact_id": commit.ArtifactID, "channel": sourceChannel, "continue_task": true, "kind": "progress",
	}, now); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) PauseForApproval(ctx context.Context, commit agent.ApprovalCommit) error {
	now := time.Now().UTC()
	if commit.EvalFindingsRef.ObjectID == "" {
		return fmt.Errorf("Eval findings reference is required")
	}
	artifactRef, err := json.Marshal(commit.ArtifactRef)
	if err != nil {
		return err
	}
	continuationRef, err := json.Marshal(commit.ContinuationRef)
	if err != nil {
		return err
	}
	findingsRef, err := json.Marshal(commit.EvalFindingsRef)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if commit.EvalTier == "" {
		commit.EvalTier = "system"
	}
	if commit.EvalEvaluator == "" {
		commit.EvalEvaluator = "approval_schema_gate"
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE agent_checkpoints SET status = 'superseded', updated_at = ?
		WHERE task_id = ? AND status = 'active'`, formatTime(now), commit.TaskID); err != nil {
		return err
	}
	if err := insertContentRef(ctx, tx, commit.ArtifactRef, now); err != nil {
		return err
	}
	if err := insertContentRef(ctx, tx, commit.ContinuationRef, now); err != nil {
		return err
	}
	if err := insertContentRef(ctx, tx, commit.EvalFindingsRef, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO approvals(id, task_id, effect_intent_id, control_level, target, parameters_hash,
			continuation_ref_json, status, expires_at, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?)`,
		commit.ApprovalID, commit.TaskID, commit.Intent.ID, string(commit.Intent.Control), commit.Intent.Target,
		commit.Intent.ParametersHash, string(continuationRef), formatTime(commit.ExpiresAt), formatTime(now)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE effect_intents SET approval_id = ?, updated_at = ? WHERE id = ? AND status = 'planned'`,
		commit.ApprovalID, formatTime(now), commit.Intent.ID); err != nil {
		return err
	}
	var version int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) + 1 FROM artifacts WHERE task_id = ?`, commit.TaskID).Scan(&version); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO artifacts(id, task_id, run_id, version, kind, content_ref_json, status, trace_ref_json, created_at)
		VALUES(?, ?, ?, ?, 'approval_request', ?, 'approved', ?, ?)`,
		commit.ArtifactID, commit.TaskID, commit.RunID, version, string(artifactRef), string(continuationRef), formatTime(now)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO eval_records(id, artifact_id, tier, evaluator, result, findings_ref_json, finding_count, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		commit.EvalID, commit.ArtifactID, commit.EvalTier, commit.EvalEvaluator, string(commit.EvalResult), string(findingsRef), len(commit.EvalFindings), formatTime(now)); err != nil {
		return err
	}
	var sourceChannel string
	if err := tx.QueryRowContext(ctx, `SELECT source_channel FROM tasks WHERE id = ?`, commit.TaskID).Scan(&sourceChannel); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO deliveries(id, task_id, artifact_id, target_channel, status, receipt, idempotency_key,
			terminal_status, continue_task, created_at, updated_at)
		VALUES(?, ?, ?, ?, 'queued', '', ?, 'completed', 1, ?, ?)`,
		commit.DeliveryID, commit.TaskID, commit.ArtifactID, sourceChannel,
		commit.ArtifactID+":"+sourceChannel, formatTime(now), formatTime(now)); err != nil {
		return err
	}
	outboxID, err := identifier.New()
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO internal_outbox(id, kind, aggregate_id, status, attempts, available_at, created_at, updated_at)
		VALUES(?, 'delivery.send', ?, 'pending', 0, ?, ?, ?)`,
		outboxID, commit.DeliveryID, formatTime(now), formatTime(now), formatTime(now)); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE tasks SET status = 'waiting', wait_reason = ?, lease_owner = NULL, lease_until = NULL,
			version = version + 1, updated_at = ? WHERE id = ? AND status = 'running'`,
		"approval:"+commit.ApprovalID, formatTime(now), commit.TaskID)
	if err != nil {
		return err
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		if err != nil {
			return err
		}
		return fmt.Errorf("task %s cannot wait for approval", commit.TaskID)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE steps SET status = 'waiting', updated_at = ? WHERE run_id = ? AND status = 'running'`, formatTime(now), commit.RunID); err != nil {
		return err
	}
	if err := appendEvent(ctx, tx, "approval", commit.ApprovalID, "approval.requested", map[string]any{
		"task_id": commit.TaskID, "effect_intent_id": commit.Intent.ID, "control": commit.Intent.Control,
		"target": commit.Intent.Target, "parameters_hash": commit.Intent.ParametersHash,
	}, now); err != nil {
		return err
	}
	if err := appendEvent(ctx, tx, "task", commit.TaskID, "task.waiting", map[string]any{"reason": "approval", "approval_id": commit.ApprovalID}, now); err != nil {
		return err
	}
	if err := appendEvent(ctx, tx, "delivery", commit.DeliveryID, "delivery.queued", map[string]any{"artifact_id": commit.ArtifactID, "channel": sourceChannel}, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ResolveApproval(ctx context.Context, approvalID string, decision approval.Decision) (approval.Result, error) {
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return approval.Result{}, err
	}
	defer tx.Rollback()
	var taskID, status, expires string
	var intent tool.Intent
	var effect, control string
	err = tx.QueryRowContext(ctx, `
		SELECT a.task_id, a.status, a.expires_at, e.id, e.tool_id, e.tool_version, e.effect_class,
			e.target, e.parameters_hash, e.control_level
		FROM approvals a JOIN effect_intents e ON e.id = a.effect_intent_id WHERE a.id = ?`, approvalID).
		Scan(&taskID, &status, &expires, &intent.ID, &intent.ToolID, &intent.ToolVersion, &effect,
			&intent.Target, &intent.ParametersHash, &control)
	if errors.Is(err, sql.ErrNoRows) {
		return approval.Result{}, fmt.Errorf("approval not found")
	}
	if err != nil {
		return approval.Result{}, err
	}
	if status == "approved" || status == "denied" {
		var grantID string
		_ = tx.QueryRowContext(ctx, `SELECT COALESCE(id, '') FROM capability_grants WHERE approval_id = ?`, approvalID).Scan(&grantID)
		return approval.Result{ApprovalID: approvalID, TaskID: taskID, Status: status, GrantID: grantID}, tx.Commit()
	}
	if status != "pending" {
		return approval.Result{}, fmt.Errorf("approval is %s", status)
	}
	expiresAt, err := parseTime(expires)
	if err != nil {
		return approval.Result{}, err
	}
	if !expiresAt.After(now) {
		if err := expireApprovalTx(ctx, tx, approvalID, taskID, intent.ID, now); err != nil {
			return approval.Result{}, err
		}
		if err := tx.Commit(); err != nil {
			return approval.Result{}, err
		}
		return approval.Result{ApprovalID: approvalID, TaskID: taskID, Status: "expired"}, nil
	}
	newStatus := "denied"
	if decision == approval.Approve {
		newStatus = "approved"
	}
	if _, err := tx.ExecContext(ctx, `UPDATE approvals SET status = ?, decided_at = ? WHERE id = ? AND status = 'pending'`, newStatus, formatTime(now), approvalID); err != nil {
		return approval.Result{}, err
	}
	grantID := ""
	if newStatus == "approved" {
		grantID, err = identifier.New()
		if err != nil {
			return approval.Result{}, err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO capability_grants(id, approval_id, task_id, tool_id, tool_version, effect_class,
				target, parameters_hash, control_level, expires_at, max_uses, uses, created_at)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, 0, ?)`,
			grantID, approvalID, taskID, intent.ToolID, intent.ToolVersion, effect,
			intent.Target, intent.ParametersHash, control, formatTime(expiresAt), formatTime(now)); err != nil {
			return approval.Result{}, err
		}
	}
	outboxID, err := identifier.New()
	if err != nil {
		return approval.Result{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO internal_outbox(id, kind, aggregate_id, status, attempts, available_at, created_at, updated_at)
		VALUES(?, 'approval.resume', ?, 'pending', 0, ?, ?, ?)`,
		outboxID, approvalID, formatTime(now), formatTime(now), formatTime(now)); err != nil {
		return approval.Result{}, err
	}
	if err := appendEvent(ctx, tx, "approval", approvalID, "approval."+newStatus, map[string]any{
		"task_id": taskID, "effect_intent_id": intent.ID, "grant_id": grantID,
	}, now); err != nil {
		return approval.Result{}, err
	}
	if err := tx.Commit(); err != nil {
		return approval.Result{}, err
	}
	return approval.Result{ApprovalID: approvalID, TaskID: taskID, Status: newStatus, GrantID: grantID}, nil
}

func (s *Store) ClaimApprovalResume(ctx context.Context, approvalID, owner string, lease time.Duration) (agent.ApprovalResume, bool, error) {
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return agent.ApprovalResume{}, false, err
	}
	defer tx.Rollback()
	var resume agent.ApprovalResume
	var continuation, grantID, grantApprovalID, grantTaskID, grantToolID, grantToolVersion string
	var grantEffect, grantTarget, grantHash, grantControl, grantExpires string
	err = tx.QueryRowContext(ctx, `
		SELECT a.task_id, a.status, a.continuation_ref_json, e.run_id, i.id,
			COALESCE(g.id, ''), COALESCE(g.approval_id, ''), COALESCE(g.task_id, ''),
			COALESCE(g.tool_id, ''), COALESCE(g.tool_version, ''), COALESCE(g.effect_class, ''),
			COALESCE(g.target, ''), COALESCE(g.parameters_hash, ''), COALESCE(g.control_level, ''),
			COALESCE(g.expires_at, '')
		FROM approvals a
		JOIN effect_intents e ON e.id = a.effect_intent_id
		JOIN invocations i ON i.run_id = e.run_id AND i.kind = 'model'
		LEFT JOIN capability_grants g ON g.approval_id = a.id
		WHERE a.id = ? AND a.status IN ('approved', 'denied', 'expired')`, approvalID).
		Scan(&resume.Task.TaskID, &resume.Decision, &continuation, &resume.Task.RunID, &resume.Task.InvocationID,
			&grantID, &grantApprovalID, &grantTaskID, &grantToolID, &grantToolVersion, &grantEffect,
			&grantTarget, &grantHash, &grantControl, &grantExpires)
	if errors.Is(err, sql.ErrNoRows) {
		return agent.ApprovalResume{}, false, nil
	}
	if err != nil {
		return agent.ApprovalResume{}, false, err
	}
	resume.ApprovalID = approvalID
	if err := json.Unmarshal([]byte(continuation), &resume.ContinuationRef); err != nil {
		return agent.ApprovalResume{}, false, err
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE tasks SET status = 'running', wait_reason = NULL, lease_owner = ?, lease_until = ?,
			version = version + 1, updated_at = ?
		WHERE id = ? AND (status = 'waiting' OR (status = 'running' AND lease_until < ?))`,
		owner, formatTime(now.Add(lease)), formatTime(now), resume.Task.TaskID, formatTime(now))
	if err != nil {
		return agent.ApprovalResume{}, false, err
	}
	count, err := result.RowsAffected()
	if err != nil || count != 1 {
		return agent.ApprovalResume{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE steps SET status = 'running', updated_at = ? WHERE run_id = ? AND status = 'waiting'`, formatTime(now), resume.Task.RunID); err != nil {
		return agent.ApprovalResume{}, false, err
	}
	if resume.Decision == "approved" {
		expiresAt, err := parseTime(grantExpires)
		if err != nil {
			return agent.ApprovalResume{}, false, err
		}
		resume.Grant = &tool.Grant{
			ID: grantID, ApprovalID: grantApprovalID, TaskID: grantTaskID, ToolID: grantToolID,
			ToolVersion: grantToolVersion, Effect: policy.EffectClass(grantEffect), Target: grantTarget,
			ParametersHash: grantHash, Control: policy.ControlLevel(grantControl), ExpiresAt: expiresAt,
		}
	}
	if err := appendEvent(ctx, tx, "task", resume.Task.TaskID, "task.resumed", map[string]any{"approval_id": approvalID}, now); err != nil {
		return agent.ApprovalResume{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return agent.ApprovalResume{}, false, err
	}
	return resume, true, nil
}

func (s *Store) LoadDelivery(ctx context.Context, id string) (delivery.Record, bool, error) {
	var record delivery.Record
	var encodedRef string
	err := s.db.QueryRowContext(ctx, `
		SELECT d.id, d.task_id, d.artifact_id, a.kind, a.content_ref_json, d.target_channel, d.status, d.terminal_status, d.continue_task
		FROM deliveries d JOIN artifacts a ON a.id = d.artifact_id WHERE d.id = ?`, id).
		Scan(&record.ID, &record.TaskID, &record.ArtifactID, &record.ArtifactKind, &encodedRef, &record.TargetChannel, &record.Status, &record.TerminalStatus, &record.ContinueTask)
	if errors.Is(err, sql.ErrNoRows) {
		return delivery.Record{}, false, nil
	}
	if err != nil {
		return delivery.Record{}, false, err
	}
	if err := json.Unmarshal([]byte(encodedRef), &record.ArtifactRef); err != nil {
		return delivery.Record{}, false, err
	}
	record.Attachments, err = listArtifactAttachments(ctx, s.db, record.ArtifactID)
	if err != nil {
		return delivery.Record{}, false, err
	}
	if record.TargetChannel == "lark" {
		record.ExternalTarget, err = deliveryExternalTarget(ctx, s.db, record.TargetChannel, record.TaskID)
		if err != nil {
			return delivery.Record{}, false, fmt.Errorf("delivery %s has no durable %s target: %w", record.ID, record.TargetChannel, err)
		}
	}
	return record, true, nil
}

func deliveryExternalTarget(ctx context.Context, queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, targetChannel, taskID string) (channel.ExternalTarget, error) {
	var target channel.ExternalTarget
	err := queryer.QueryRowContext(ctx, `
		SELECT cm.external_conversation_id, cm.external_message_id
		FROM channel_messages cm JOIN interactions i ON i.id = cm.interaction_id
		WHERE cm.channel = ? AND cm.direction = 'inbound' AND i.task_id = ?
		ORDER BY i.sequence DESC LIMIT 1`, targetChannel, taskID).
		Scan(&target.ConversationID, &target.ReplyToMessageID)
	if errors.Is(err, sql.ErrNoRows) {
		err = queryer.QueryRowContext(ctx, `
			SELECT target_conversation_id, reply_to_message_id
			FROM commitment_fires WHERE task_id = ? AND target_channel = ?`, taskID, targetChannel).
			Scan(&target.ConversationID, &target.ReplyToMessageID)
	}
	if err != nil {
		return channel.ExternalTarget{}, err
	}
	if strings.TrimSpace(target.ConversationID) == "" {
		return channel.ExternalTarget{}, fmt.Errorf("empty external conversation id")
	}
	return target, nil
}

func (s *Store) CommitConversationDelivery(ctx context.Context, deliveryID, interactionID string, receipt delivery.Receipt, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var record delivery.Record
	var encodedRef string
	err = tx.QueryRowContext(ctx, `
		SELECT d.id, d.task_id, d.artifact_id, a.kind, a.content_ref_json, d.target_channel, d.status, d.terminal_status, d.continue_task
		FROM deliveries d JOIN artifacts a ON a.id = d.artifact_id WHERE d.id = ?`, deliveryID).
		Scan(&record.ID, &record.TaskID, &record.ArtifactID, &record.ArtifactKind, &encodedRef, &record.TargetChannel, &record.Status, &record.TerminalStatus, &record.ContinueTask)
	if err != nil {
		return err
	}
	if record.Status == "sent" || record.Status == "acknowledged" {
		return nil
	}
	if record.Status != "queued" && record.Status != "failed" {
		return fmt.Errorf("delivery %s has non-dispatchable status %s", deliveryID, record.Status)
	}
	if receipt.Level == "" {
		return fmt.Errorf("delivery %s receipt level is required", deliveryID)
	}
	role := "assistant"
	if record.ArtifactKind == "approval_request" || record.ArtifactKind == "runtime_error" {
		role = "system"
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO interactions(id, conversation_id, task_id, direction, role, kind, channel, content_ref_json, artifact_id, delivery_id, receipt, created_at)
		VALUES(?, ?, ?, 'outbound', ?, ?, ?, ?, ?, ?, ?, ?)`,
		interactionID, channel.ConversationID, record.TaskID, role, record.ArtifactKind, record.TargetChannel,
		encodedRef, record.ArtifactID, record.ID, receipt.Level, formatTime(now)); err != nil {
		return err
	}
	if receipt.ExternalMessageID != "" {
		target, err := deliveryExternalTarget(ctx, tx, record.TargetChannel, record.TaskID)
		if err != nil {
			return fmt.Errorf("resolve delivery %s external target: %w", deliveryID, err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO channel_messages(
				channel, external_message_id, interaction_id, external_conversation_id,
				external_sender_id, reply_to_external_message_id, direction, external_created_at, created_at
			) VALUES(?, ?, ?, ?, NULL, ?, 'outbound', ?, ?)`,
			record.TargetChannel, receipt.ExternalMessageID, interactionID, target.ConversationID,
			target.ReplyToMessageID, formatTime(now), formatTime(now)); err != nil {
			return err
		}
	}
	artifactAttachments, err := listArtifactAttachments(ctx, tx, record.ArtifactID)
	if err != nil {
		return err
	}
	for _, attachment := range artifactAttachments {
		encodedAttachmentRef, err := json.Marshal(attachment.ContentRef)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO attachments(id, interaction_id, name, media_type, size_bytes, content_ref_json, created_at)
			VALUES(?, ?, ?, ?, ?, ?, ?)`, attachment.ID, interactionID, attachment.Name, attachment.MediaType,
			attachment.SizeBytes, string(encodedAttachmentRef), formatTime(now)); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE deliveries SET status = 'sent', receipt = ?, updated_at = ? WHERE id = ?`, receipt.Level, formatTime(now), deliveryID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE artifacts SET status = 'delivered' WHERE id = ?`, record.ArtifactID); err != nil {
		return err
	}
	if record.ContinueTask {
		if err := appendEvent(ctx, tx, "delivery", deliveryID, "delivery.sent", map[string]any{"receipt": receipt.Level, "interaction_id": interactionID}, now); err != nil {
			return err
		}
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET status = terminal_status, wait_reason = NULL, version = version + 1, updated_at = ? WHERE id = ?`, formatTime(now), record.TaskID); err != nil {
		return err
	}
	runStatus := "succeeded"
	if record.TerminalStatus == "failed" {
		runStatus = "failed"
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET status = ?, ended_at = ? WHERE task_id = ? AND status = 'active'`, runStatus, formatTime(now), record.TaskID); err != nil {
		return err
	}
	if err := appendEvent(ctx, tx, "delivery", deliveryID, "delivery.sent", map[string]any{"receipt": receipt.Level, "interaction_id": interactionID}, now); err != nil {
		return err
	}
	taskEvent := "task.completed"
	if record.TerminalStatus == "failed" {
		taskEvent = "task.failed"
	}
	if err := appendEvent(ctx, tx, "task", record.TaskID, taskEvent, map[string]any{"delivery_id": deliveryID}, now); err != nil {
		return err
	}
	episodeOutboxID, err := identifier.New()
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO internal_outbox(id, kind, aggregate_id, status, attempts, available_at, created_at, updated_at)
		VALUES(?, 'episode.build', ?, 'pending', 0, ?, ?, ?)`, episodeOutboxID, record.TaskID,
		formatTime(now), formatTime(now), formatTime(now)); err != nil {
		return err
	}
	if err := queueDataErasureAfterDelivery(ctx, tx, record.TaskID, now); err != nil {
		return err
	}
	return tx.Commit()
}

func listArtifactAttachments(ctx context.Context, queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, artifactID string) ([]channel.AttachmentRecord, error) {
	rows, err := queryer.QueryContext(ctx, `
		SELECT id, name, media_type, size_bytes, content_ref_json
		FROM artifact_attachments WHERE artifact_id = ? ORDER BY created_at, id`, artifactID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	attachments := make([]channel.AttachmentRecord, 0)
	for rows.Next() {
		var attachment channel.AttachmentRecord
		var encodedRef string
		if err := rows.Scan(&attachment.ID, &attachment.Name, &attachment.MediaType, &attachment.SizeBytes, &encodedRef); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(encodedRef), &attachment.ContentRef); err != nil {
			return nil, err
		}
		attachments = append(attachments, attachment)
	}
	return attachments, rows.Err()
}

func (s *Store) PlanIntent(ctx context.Context, intent tool.Intent) (tool.Intent, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return tool.Intent{}, false, err
	}
	defer tx.Rollback()
	// Recovery must be able to replay an intent that crossed the durable
	// boundary before a newer user message arrived. The input watermark fences
	// only creation of a new stale effect; it must not hide an already-planned
	// or completed effect from reconciliation.
	existing, err := loadIntent(ctx, tx, intent.IdempotencyKey)
	if err == nil {
		return existing, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return tool.Intent{}, false, err
	}
	if intent.BasisInputSequence > 0 {
		var latestInputSequence int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence), 0) FROM interactions WHERE task_id = ? AND direction = 'inbound'`, intent.TaskID).Scan(&latestInputSequence); err != nil {
			return tool.Intent{}, false, err
		}
		if latestInputSequence > intent.BasisInputSequence {
			return tool.Intent{}, false, tool.ErrStaleTaskInput
		}
	}
	if intent.ParentIntentID != "" {
		var parentCount int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM effect_intents WHERE id = ? AND task_id = ? AND run_id = ?`, intent.ParentIntentID, intent.TaskID, intent.RunID).Scan(&parentCount); err != nil {
			return tool.Intent{}, false, err
		}
		if parentCount != 1 {
			return tool.Intent{}, false, fmt.Errorf("parent effect intent is missing or belongs to another task/run")
		}
	}
	payloadRef, err := json.Marshal(intent.PayloadRef)
	if err != nil {
		return tool.Intent{}, false, err
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO effect_intents(
			id, task_id, run_id, invocation_id, tool_call_id, parent_intent_id, tool_id, tool_version, effect_class, target,
			parameters_hash, payload_ref_json, idempotency_key, control_level, approval_id, grant_id,
			reconciliation_strategy, status, created_at, updated_at)
		VALUES(?, ?, ?, ?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?, 'planned', ?, ?)
		ON CONFLICT(idempotency_key) DO NOTHING`,
		intent.ID, intent.TaskID, intent.RunID, intent.InvocationID, intent.ToolCallID, intent.ParentIntentID, intent.ToolID, intent.ToolVersion, string(intent.Effect), intent.Target,
		intent.ParametersHash, string(payloadRef), intent.IdempotencyKey, string(intent.Control), intent.ApprovalID, intent.GrantID,
		intent.ReconciliationStrategy, formatTime(intent.CreatedAt), formatTime(intent.UpdatedAt))
	if err != nil {
		return tool.Intent{}, false, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return tool.Intent{}, false, err
	}
	if count == 1 {
		if intent.PayloadRef.ObjectID != "" {
			if err := insertContentRef(ctx, tx, intent.PayloadRef, intent.CreatedAt); err != nil {
				return tool.Intent{}, false, err
			}
		}
		if err := appendEvent(ctx, tx, "effect_intent", intent.ID, "effect.planned", map[string]any{
			"tool_id": intent.ToolID, "effect_class": intent.Effect, "control": intent.Control,
			"parameters_hash": intent.ParametersHash, "parent_intent_id": intent.ParentIntentID,
			"invocation_id": intent.InvocationID, "tool_call_id": intent.ToolCallID,
		}, intent.CreatedAt); err != nil {
			return tool.Intent{}, false, err
		}
		if err := tx.Commit(); err != nil {
			return tool.Intent{}, false, err
		}
		return intent, true, nil
	}
	existing, err = loadIntent(ctx, tx, intent.IdempotencyKey)
	if err != nil {
		return tool.Intent{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return tool.Intent{}, false, err
	}
	return existing, false, nil
}

func (s *Store) TransitionIntent(ctx context.Context, id string, from, to tool.IntentStatus, errorCode, approvalID, grantID string, resultRef content.Ref) error {
	if !validIntentTransition(from, to) {
		return fmt.Errorf("invalid effect intent transition %s -> %s", from, to)
	}
	now := time.Now().UTC()
	reconciliationOutboxID := ""
	if to == tool.IntentUnknown {
		var err error
		reconciliationOutboxID, err = identifier.New()
		if err != nil {
			return err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	encodedResultRef := ""
	if resultRef.ObjectID != "" {
		encoded, err := json.Marshal(resultRef)
		if err != nil {
			return err
		}
		encodedResultRef = string(encoded)
		if err := insertContentRef(ctx, tx, resultRef, now); err != nil {
			return err
		}
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE effect_intents SET status = ?, error_code = NULLIF(?, ''),
		approval_id = COALESCE(NULLIF(?, ''), approval_id),
		grant_id = COALESCE(NULLIF(?, ''), grant_id), result_ref_json = NULLIF(?, ''), updated_at = ?
		WHERE id = ? AND status = ?`,
		string(to), errorCode, approvalID, grantID, encodedResultRef, formatTime(now), id, string(from))
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("effect intent %s is not in state %s", id, from)
	}
	var invocationID, toolCallID string
	if err := tx.QueryRowContext(ctx, `SELECT invocation_id, tool_call_id FROM effect_intents WHERE id = ?`, id).Scan(&invocationID, &toolCallID); err != nil {
		return err
	}
	if err := appendEvent(ctx, tx, "effect_intent", id, "effect."+string(to), map[string]any{
		"from": from, "to": to, "error_code": errorCode, "invocation_id": invocationID, "tool_call_id": toolCallID,
	}, now); err != nil {
		return err
	}
	if reconciliationOutboxID != "" {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO internal_outbox(id, kind, aggregate_id, status, attempts, available_at, created_at, updated_at)
			SELECT ?, 'effect.reconcile', ?, 'pending', 0, ?, ?, ?
			WHERE NOT EXISTS (
				SELECT 1 FROM internal_outbox WHERE kind = 'effect.reconcile' AND aggregate_id = ? AND status IN ('pending', 'processing')
			)`, reconciliationOutboxID, id, formatTime(now), formatTime(now), formatTime(now), id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func loadIntent(ctx context.Context, queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, idempotencyKey string) (tool.Intent, error) {
	return scanIntent(queryer.QueryRowContext(ctx, `
		SELECT id, task_id, run_id, invocation_id, tool_call_id, COALESCE(parent_intent_id, ''), tool_id, tool_version, effect_class, target,
		       parameters_hash, payload_ref_json, idempotency_key, control_level, COALESCE(approval_id, ''),
		       COALESCE(grant_id, ''), reconciliation_strategy, status,
	       COALESCE(result_ref_json, ''), COALESCE(error_code, ''), created_at, updated_at
		FROM effect_intents WHERE idempotency_key = ?`, idempotencyKey))
}

func (s *Store) LoadIntentByID(ctx context.Context, id string) (tool.Intent, bool, error) {
	intent, err := scanIntent(s.db.QueryRowContext(ctx, `
		SELECT id, task_id, run_id, invocation_id, tool_call_id, COALESCE(parent_intent_id, ''), tool_id, tool_version, effect_class, target,
		       parameters_hash, payload_ref_json, idempotency_key, control_level, COALESCE(approval_id, ''),
		       COALESCE(grant_id, ''), reconciliation_strategy, status,
	       COALESCE(result_ref_json, ''), COALESCE(error_code, ''), created_at, updated_at
		FROM effect_intents WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return tool.Intent{}, false, nil
	}
	return intent, err == nil, err
}

type intentScanner interface{ Scan(...any) error }

func scanIntent(scanner intentScanner) (tool.Intent, error) {
	var intent tool.Intent
	var effect, control, status, payload, result, created, updated string
	err := scanner.Scan(&intent.ID, &intent.TaskID, &intent.RunID, &intent.InvocationID, &intent.ToolCallID, &intent.ParentIntentID, &intent.ToolID, &intent.ToolVersion, &effect, &intent.Target,
		&intent.ParametersHash, &payload, &intent.IdempotencyKey, &control, &intent.ApprovalID, &intent.GrantID,
		&intent.ReconciliationStrategy, &status, &result, &intent.ErrorCode, &created, &updated)
	if err != nil {
		return tool.Intent{}, err
	}
	intent.Effect = policy.EffectClass(effect)
	intent.Control = policy.ControlLevel(control)
	intent.Status = tool.IntentStatus(status)
	if payload != "" && payload != "{}" {
		if err := json.Unmarshal([]byte(payload), &intent.PayloadRef); err != nil {
			return tool.Intent{}, err
		}
	}
	if result != "" {
		if err := json.Unmarshal([]byte(result), &intent.ResultRef); err != nil {
			return tool.Intent{}, err
		}
	}
	intent.CreatedAt, err = parseTime(created)
	if err != nil {
		return tool.Intent{}, err
	}
	intent.UpdatedAt, err = parseTime(updated)
	return intent, err
}

func (s *Store) RecordReconciliationAttempt(ctx context.Context, id, outcome, errorCode string) error {
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE effect_intents SET updated_at = ? WHERE id = ?`, formatTime(now), id); err != nil {
		return err
	}
	if err := appendEvent(ctx, tx, "effect_intent", id, "effect.reconciliation_"+outcome, map[string]any{
		"outcome": outcome, "error_code": errorCode,
	}, now); err != nil {
		return err
	}
	return tx.Commit()
}

func validIntentTransition(from, to tool.IntentStatus) bool {
	switch from {
	case tool.IntentPlanned:
		return to == tool.IntentAuthorized || to == tool.IntentFailed
	case tool.IntentAuthorized:
		return to == tool.IntentDispatched || to == tool.IntentFailed
	case tool.IntentDispatched:
		return to == tool.IntentConfirmed || to == tool.IntentFailed || to == tool.IntentUnknown
	case tool.IntentUnknown:
		return to == tool.IntentConfirmed || to == tool.IntentFailed || to == tool.IntentCompensated
	default:
		return false
	}
}

func (s *Store) ClaimOutbox(ctx context.Context, owner string, lease time.Duration) (runtime.OutboxItem, bool, error) {
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return runtime.OutboxItem{}, false, err
	}
	defer tx.Rollback()
	var item runtime.OutboxItem
	err = tx.QueryRowContext(ctx, `
		SELECT id, kind, aggregate_id, attempts FROM internal_outbox
		WHERE ((status = 'pending' AND available_at <= ?)
		   OR (status = 'processing' AND lease_until < ?))
		  AND (NOT EXISTS (SELECT 1 FROM data_erasure_jobs WHERE status = 'ready') OR kind = 'data.erase')
		ORDER BY CASE WHEN kind = 'data.erase' THEN 0 ELSE 1 END, created_at ASC LIMIT 1`, formatTime(now), formatTime(now)).
		Scan(&item.ID, &item.Kind, &item.AggregateID, &item.Attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return runtime.OutboxItem{}, false, nil
	}
	if err != nil {
		return runtime.OutboxItem{}, false, err
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE internal_outbox SET status = 'processing', lease_owner = ?, lease_until = ?, attempts = attempts + 1, updated_at = ?
		WHERE id = ? AND ((status = 'pending' AND available_at <= ?) OR (status = 'processing' AND lease_until < ?))`,
		owner, formatTime(now.Add(lease)), formatTime(now), item.ID, formatTime(now), formatTime(now))
	if err != nil {
		return runtime.OutboxItem{}, false, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return runtime.OutboxItem{}, false, err
	}
	if count != 1 {
		return runtime.OutboxItem{}, false, nil
	}
	if err := tx.Commit(); err != nil {
		return runtime.OutboxItem{}, false, err
	}
	return item, true, nil
}

func (s *Store) CompleteOutbox(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE internal_outbox SET status = 'done', lease_owner = NULL, lease_until = NULL, updated_at = ? WHERE id = ?`,
		formatTime(time.Now().UTC()), id)
	return err
}

func (s *Store) RenewOutboxLease(ctx context.Context, id, owner string, lease time.Duration) error {
	now := time.Now().UTC()
	leaseUntil := formatTime(now.Add(lease))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var kind, aggregateID string
	err = tx.QueryRowContext(ctx, `
		SELECT kind, aggregate_id FROM internal_outbox
		WHERE id = ? AND status = 'processing' AND lease_owner = ?`, id, owner).Scan(&kind, &aggregateID)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("outbox lease %s is no longer owned by %s", id, owner)
	}
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE internal_outbox SET lease_until = ?, updated_at = ?
		WHERE id = ? AND status = 'processing' AND lease_owner = ?`,
		leaseUntil, formatTime(now), id, owner)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("outbox lease %s is no longer owned by %s", id, owner)
	}
	// The dispatch lease and the domain Task lease form one ownership unit for
	// long Agent runs. Renew both in the same transaction so another worker can
	// never reclaim the Task while the original outbox handler is still alive.
	taskID := ""
	switch kind {
	case "task.wake":
		taskID = aggregateID
	case "approval.resume":
		err := tx.QueryRowContext(ctx, `SELECT task_id FROM approvals WHERE id = ?`, aggregateID).Scan(&taskID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	if taskID != "" {
		result, err := tx.ExecContext(ctx, `
			UPDATE tasks SET lease_until = ?, updated_at = ?
			WHERE id = ? AND status = 'running' AND lease_owner = ?`,
			leaseUntil, formatTime(now), taskID, owner)
		if err != nil {
			return err
		}
		updated, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if updated == 0 {
			var status, taskOwner string
			err := tx.QueryRowContext(ctx, `SELECT status, COALESCE(lease_owner, '') FROM tasks WHERE id = ?`, taskID).Scan(&status, &taskOwner)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if err == nil && status == "running" {
				return fmt.Errorf("task lease %s is no longer owned by %s (owner %s)", taskID, owner, taskOwner)
			}
		}
	}
	return tx.Commit()
}

func (s *Store) RetryOutbox(ctx context.Context, id, lastError string, availableAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE internal_outbox SET status = 'pending', lease_owner = NULL, lease_until = NULL, last_error = ?, available_at = ?, updated_at = ? WHERE id = ?`,
		lastError, formatTime(availableAt), formatTime(time.Now().UTC()), id)
	return err
}

func insertContentRef(ctx context.Context, tx *sql.Tx, ref content.Ref, now time.Time) error {
	encoded, err := json.Marshal(ref)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO content_objects(object_id, version, ref_json, created_at)
		VALUES(?, ?, ?, ?) ON CONFLICT(object_id, version) DO NOTHING`,
		ref.ObjectID, ref.Version, string(encoded), formatTime(now))
	return err
}

func appendEvent(ctx context.Context, tx *sql.Tx, aggregateType, aggregateID, eventType string, payload map[string]any, now time.Time) error {
	id, err := identifier.New()
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO events(id, aggregate_type, aggregate_id, type, payload_json, created_at)
		VALUES(?, ?, ?, ?, ?, ?)`, id, aggregateType, aggregateID, eventType, string(encoded), formatTime(now))
	return err
}

func formatTime(value time.Time) string { return value.UTC().Format(timestampLayout) }

func parseTime(value string) (time.Time, error) {
	parsed, err := time.Parse(timestampLayout, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse persisted timestamp: %w", err)
	}
	return parsed, nil
}
