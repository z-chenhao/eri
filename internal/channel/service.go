// Package channel owns the single user-facing conversation boundary.
package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/z-chenhao/eri/internal/content"
	"github.com/z-chenhao/eri/internal/eventlog"
	"github.com/z-chenhao/eri/internal/identifier"
	"github.com/z-chenhao/eri/internal/secret"
)

const ConversationID = "primary"

const (
	maxAttachmentBytes = 20 * 1024 * 1024
	maxUploadBytes     = 32 * 1024 * 1024
)

// Message is a resolved user-visible interaction.
type Message struct {
	Sequence    int64          `json:"sequence"`
	ID          string         `json:"id"`
	TaskID      string         `json:"task_id,omitempty"`
	Direction   string         `json:"direction"`
	Role        string         `json:"role"`
	Kind        string         `json:"kind"`
	Channel     string         `json:"channel"`
	Content     string         `json:"content"`
	ArtifactID  string         `json:"artifact_id,omitempty"`
	DeliveryID  string         `json:"delivery_id,omitempty"`
	Receipt     string         `json:"receipt,omitempty"`
	Data        map[string]any `json:"data,omitempty"`
	Attachments []Attachment   `json:"attachments,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
}

type Attachment struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	MediaType string `json:"media_type"`
	SizeBytes int64  `json:"size_bytes"`
}

type AttachmentUpload struct {
	Name      string
	MediaType string
	Body      []byte
}

type AttachmentRecord struct {
	Attachment
	ContentRef content.Ref
}

type AttachmentContent struct {
	Attachment
	Body []byte
}

// MessageRecord is the operational interaction with a governed content ref.
type MessageRecord struct {
	Sequence    int64
	ID          string
	TaskID      string
	Direction   string
	Role        string
	Kind        string
	Channel     string
	ContentRef  content.Ref
	ArtifactID  string
	DeliveryID  string
	Receipt     string
	Attachments []AttachmentRecord
	CreatedAt   time.Time
}

type SendResult struct {
	InteractionID string `json:"interaction_id"`
	TaskID        string `json:"task_id"`
}

// ExternalInteraction is the platform-neutral identity envelope accepted from
// a trusted remote Channel adapter. Platform DTOs are converted before this
// boundary so Conversation and Task never depend on Lark-specific fields.
type ExternalInteraction struct {
	MessageID        string
	ConversationID   string
	SenderID         string
	ReplyToMessageID string
	CreatedAt        time.Time
}

// ExternalTarget identifies where a remote-channel Delivery must be sent.
// It is loaded from the durable inbound mapping immediately before dispatch.
type ExternalTarget struct {
	ConversationID   string
	ReplyToMessageID string
}

type ConnectRequest struct {
	Locale   string `json:"locale,omitempty"`
	Timezone string `json:"timezone,omitempty"`
}

type ConnectResult struct {
	IntroductionStarted bool   `json:"introduction_started"`
	TaskID              string `json:"task_id,omitempty"`
}

// InputError identifies a user-correctable message rejection without turning
// storage or runtime failures into public validation details.
type InputError struct {
	Code    string
	Message string
}

func (e *InputError) Error() string { return e.Message }

func inputError(code, message string) error {
	return &InputError{Code: code, Message: message}
}

type TaskStatus struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	UpdatedAt time.Time `json:"updated_at"`
	ErrorCode string    `json:"error_code,omitempty"`
}

type Presence struct {
	State       string `json:"state"`
	ActiveTasks int    `json:"active_tasks"`
}

type Repository interface {
	EnsureIntroduction(context.Context, string, content.Ref) (ConnectResult, error)
	CreateInbound(context.Context, string, content.Ref, []AttachmentRecord) (SendResult, error)
	CreateExternalInbound(context.Context, string, ExternalInteraction, content.Ref, []AttachmentRecord) (SendResult, bool, error)
	ListMessages(context.Context, int64, int) ([]MessageRecord, error)
	ListMessagesBefore(context.Context, int64, int) ([]MessageRecord, error)
	ListMessagesForTask(context.Context, string) ([]MessageRecord, error)
	TaskStatus(context.Context, string) (TaskStatus, error)
	Presence(context.Context) (Presence, error)
	ListEvents(context.Context, int64, int) ([]eventlog.Event, error)
	ApprovalStatus(context.Context, string) (string, error)
	LoadAttachment(context.Context, string) (AttachmentRecord, bool, error)
}

type ContentStore interface {
	Put(context.Context, []byte, content.Metadata) (content.Ref, error)
	Get(context.Context, content.Ref) ([]byte, error)
	Delete(context.Context, content.Ref) error
}

type Service struct {
	repository Repository
	content    ContentStore
}

func NewService(repository Repository, contentStore ContentStore) *Service {
	return &Service{repository: repository, content: contentStore}
}

// Connect records the first authenticated contact with Eri. The repository
// turns the governed instruction into one ordinary Agent Task exactly once;
// no user-visible introduction text is compiled into the client or Core.
func (s *Service) Connect(ctx context.Context, sourceChannel string, request ConnectRequest) (ConnectResult, error) {
	request.Locale = strings.TrimSpace(request.Locale)
	request.Timezone = strings.TrimSpace(request.Timezone)
	if len(request.Locale) > 64 || len(request.Timezone) > 128 || secret.LooksLikeCredential([]byte(request.Locale+"\n"+request.Timezone)) {
		return ConnectResult{}, inputError("connection_context_invalid", "connection context is invalid")
	}
	if request.Timezone != "" {
		if _, err := time.LoadLocation(request.Timezone); err != nil {
			request.Timezone = ""
		}
	}
	instruction := strings.Join([]string{
		"This is the user's first authenticated connection to Eri's canonical conversation.",
		"Say hello as Eri in one or two short, natural sentences. This should feel like meeting a real assistant, not reading a product introduction.",
		"Do not list capabilities, explain your mission, promise reliability, describe the relationship, claim shared history, ask a setup questionnaire, mention internal architecture, or append a question just to keep the conversation going.",
		"Use the interface locale as a language hint when it is available. Treat locale and timezone as device observations, not durable user preferences.",
		fmt.Sprintf("Interface locale: %q. Device timezone: %q.", request.Locale, request.Timezone),
	}, "\n")
	ref, err := s.content.Put(ctx, []byte(instruction), content.Metadata{
		MediaType: "text/plain; charset=utf-8", EncryptionDomain: "conversation-trigger",
		PrivacyClass: "private", RetentionPolicy: "user_owned", ProvenanceRef: ConversationID,
	})
	if err != nil {
		return ConnectResult{}, fmt.Errorf("store introduction trigger: %w", err)
	}
	result, err := s.repository.EnsureIntroduction(ctx, sourceChannel, ref)
	if err != nil {
		_ = s.content.Delete(context.Background(), ref)
		return ConnectResult{}, fmt.Errorf("commit introduction trigger: %w", err)
	}
	if !result.IntroductionStarted {
		_ = s.content.Delete(context.Background(), ref)
	}
	return result, nil
}

func (s *Service) Send(ctx context.Context, sourceChannel, text string) (SendResult, error) {
	return s.SendWithAttachments(ctx, sourceChannel, text, nil)
}

func (s *Service) SendWithAttachments(ctx context.Context, sourceChannel, text string, uploads []AttachmentUpload) (SendResult, error) {
	result, _, err := s.storeInbound(ctx, sourceChannel, ExternalInteraction{}, text, uploads)
	return result, err
}

// SendExternalWithAttachments commits a remote message and its external
// identity atomically. A redelivered platform message returns the original
// result with created=false and does not create another Task or content owner.
func (s *Service) SendExternalWithAttachments(ctx context.Context, sourceChannel string, external ExternalInteraction, text string, uploads []AttachmentUpload) (SendResult, bool, error) {
	if strings.TrimSpace(external.MessageID) == "" || strings.TrimSpace(external.ConversationID) == "" || strings.TrimSpace(external.SenderID) == "" {
		return SendResult{}, false, inputError("external_identity_invalid", "remote message identity is invalid")
	}
	return s.storeInbound(ctx, sourceChannel, external, text, uploads)
}

func (s *Service) storeInbound(ctx context.Context, sourceChannel string, external ExternalInteraction, text string, uploads []AttachmentUpload) (SendResult, bool, error) {
	text = strings.TrimSpace(text)
	if text == "" && len(uploads) == 0 {
		return SendResult{}, false, inputError("message_required", "message text or attachment is required")
	}
	if len([]byte(text)) > 1024*1024 {
		return SendResult{}, false, inputError("message_too_large", "message exceeds 1 MiB")
	}
	if secret.LooksLikeCredential([]byte(text)) {
		return SendResult{}, false, inputError("credential_detected", "credentials cannot be sent through or stored in Eri conversation; use the browser or operating-system authentication surface")
	}
	totalUploadBytes := 0
	for _, upload := range uploads {
		if len(upload.Body) > maxAttachmentBytes {
			return SendResult{}, false, inputError("attachment_too_large", fmt.Sprintf("attachment %q exceeds 20 MiB", upload.Name))
		}
		totalUploadBytes += len(upload.Body)
		if totalUploadBytes > maxUploadBytes {
			return SendResult{}, false, inputError("attachments_too_large", "attachments exceed 32 MiB per message")
		}
		if secret.LooksLikeCredential(upload.Body) {
			return SendResult{}, false, inputError("attachment_credential_detected", "an attachment appears to contain a credential and was not stored")
		}
	}
	ref, err := s.content.Put(ctx, []byte(text), content.Metadata{
		MediaType:        "text/plain; charset=utf-8",
		EncryptionDomain: "conversation",
		PrivacyClass:     "private",
		RetentionPolicy:  "user_owned",
	})
	if err != nil {
		return SendResult{}, false, fmt.Errorf("store inbound content: %w", err)
	}
	storedRefs := []content.Ref{ref}
	committed := false
	defer func() {
		if committed {
			return
		}
		for _, storedRef := range storedRefs {
			_ = s.content.Delete(context.Background(), storedRef)
		}
	}()
	attachments := make([]AttachmentRecord, 0, len(uploads))
	for _, upload := range uploads {
		if strings.TrimSpace(upload.Name) == "" || len(upload.Body) == 0 {
			return SendResult{}, false, inputError("attachment_invalid", "attachment name and body are required")
		}
		id, err := identifier.New()
		if err != nil {
			return SendResult{}, false, err
		}
		attachmentRef, err := s.content.Put(ctx, upload.Body, content.Metadata{
			MediaType: upload.MediaType, EncryptionDomain: "attachment", PrivacyClass: "private",
			RetentionPolicy: "user_owned", ProvenanceRef: id,
		})
		if err != nil {
			return SendResult{}, false, fmt.Errorf("store attachment %s: %w", upload.Name, err)
		}
		storedRefs = append(storedRefs, attachmentRef)
		attachments = append(attachments, AttachmentRecord{
			Attachment: Attachment{ID: id, Name: upload.Name, MediaType: upload.MediaType, SizeBytes: int64(len(upload.Body))},
			ContentRef: attachmentRef,
		})
	}
	var result SendResult
	created := true
	if external.MessageID == "" {
		result, err = s.repository.CreateInbound(ctx, sourceChannel, ref, attachments)
	} else {
		result, created, err = s.repository.CreateExternalInbound(ctx, sourceChannel, external, ref, attachments)
	}
	if err != nil {
		return SendResult{}, false, fmt.Errorf("commit inbound interaction: %w", err)
	}
	committed = created
	if !created {
		// The durable copy already owns the original content. Delete the fresh
		// redelivery objects created before the atomic deduplication check.
		return result, false, nil
	}
	return result, true, nil
}

func (s *Service) Attachment(ctx context.Context, id string) (AttachmentContent, bool, error) {
	record, found, err := s.repository.LoadAttachment(ctx, id)
	if err != nil || !found {
		return AttachmentContent{}, found, err
	}
	body, err := s.content.Get(ctx, record.ContentRef)
	if err != nil {
		return AttachmentContent{}, false, fmt.Errorf("read attachment %s: %w", id, err)
	}
	return AttachmentContent{Attachment: record.Attachment, Body: body}, true, nil
}

func (s *Service) Messages(ctx context.Context, after, before int64, limit int) ([]Message, error) {
	if after > 0 && before > 0 {
		return nil, fmt.Errorf("after and before cursors are mutually exclusive")
	}
	var records []MessageRecord
	var err error
	if before >= 0 {
		records, err = s.repository.ListMessagesBefore(ctx, before, clampLimit(limit))
	} else {
		records, err = s.repository.ListMessages(ctx, after, clampLimit(limit))
	}
	if err != nil {
		return nil, err
	}
	return s.resolve(ctx, records)
}

// TaskMessages returns the complete user-visible interaction history for one
// task. It exists so short-lived clients can wait for the exact task result
// without guessing that it is present in an arbitrary global page.
func (s *Service) TaskMessages(ctx context.Context, taskID string) ([]Message, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return nil, fmt.Errorf("task id is required")
	}
	records, err := s.repository.ListMessagesForTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	return s.resolve(ctx, records)
}

func (s *Service) Search(ctx context.Context, query string, limit int) ([]Message, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return []Message{}, nil
	}
	needle := strings.ToLower(query)
	matches := make([]Message, 0)
	after := int64(0)
	resultLimit := clampLimit(limit)
	for {
		records, err := s.repository.ListMessages(ctx, after, 500)
		if err != nil {
			return nil, err
		}
		resolved, err := s.resolve(ctx, records)
		if err != nil {
			return nil, err
		}
		for _, message := range resolved {
			if strings.Contains(strings.ToLower(message.Content), needle) {
				matches = append(matches, message)
				if len(matches) > resultLimit {
					matches = matches[len(matches)-resultLimit:]
				}
			}
		}
		if len(records) < 500 {
			break
		}
		after = records[len(records)-1].Sequence
	}
	return matches, nil
}

func (s *Service) Task(ctx context.Context, id string) (TaskStatus, error) {
	return s.repository.TaskStatus(ctx, id)
}

func (s *Service) CurrentPresence(ctx context.Context) (Presence, error) {
	return s.repository.Presence(ctx)
}

func (s *Service) Events(ctx context.Context, after int64, limit int) ([]eventlog.Event, error) {
	return s.repository.ListEvents(ctx, after, clampLimit(limit))
}

func (s *Service) resolve(ctx context.Context, records []MessageRecord) ([]Message, error) {
	messages := make([]Message, 0, len(records))
	for _, record := range records {
		body, err := s.content.Get(ctx, record.ContentRef)
		if err != nil {
			return nil, fmt.Errorf("resolve message %s: %w", record.ID, err)
		}
		text := string(body)
		var data map[string]any
		if record.Kind == "approval_request" || record.Kind == "runtime_error" {
			if err := json.Unmarshal(body, &data); err != nil {
				return nil, fmt.Errorf("decode system message %s: %w", record.ID, err)
			}
			text = ""
		}
		if record.Kind == "approval_request" {
			if id, ok := data["approval_id"].(string); ok && id != "" {
				status, err := s.repository.ApprovalStatus(ctx, id)
				if err != nil {
					return nil, fmt.Errorf("read approval status %s: %w", id, err)
				}
				data["status"] = status
			}
		}
		messages = append(messages, Message{
			Sequence: record.Sequence, ID: record.ID, TaskID: record.TaskID,
			Direction: record.Direction, Role: record.Role, Kind: record.Kind,
			Channel: record.Channel, Content: text, ArtifactID: record.ArtifactID,
			DeliveryID: record.DeliveryID, Receipt: record.Receipt, Data: data,
			Attachments: publicAttachments(record.Attachments), CreatedAt: record.CreatedAt,
		})
	}
	return messages, nil
}

func publicAttachments(records []AttachmentRecord) []Attachment {
	attachments := make([]Attachment, 0, len(records))
	for _, record := range records {
		attachments = append(attachments, record.Attachment)
	}
	return attachments
}

func clampLimit(limit int) int {
	if limit <= 0 {
		return 100
	}
	if limit > 500 {
		return 500
	}
	return limit
}
