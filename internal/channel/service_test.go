package channel

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/z-chenhao/eri/internal/content"
	"github.com/z-chenhao/eri/internal/eventlog"
)

func TestMessageValidationReturnsStableInputError(t *testing.T) {
	service := NewService(nil, nil)
	_, err := service.Send(context.Background(), "cli", "")
	var input *InputError
	if !errors.As(err, &input) || input.Code != "message_required" || input.Message == "" {
		t.Fatalf("input error = %#v, err = %v", input, err)
	}
}

type channelRepository struct {
	createErr       error
	createdChannel  string
	createdRef      content.Ref
	createdUploads  []AttachmentRecord
	records         []MessageRecord
	listMode        string
	approvalStatus  string
	connectResult   ConnectResult
	introductionRef content.Ref
}

func (r *channelRepository) EnsureIntroduction(_ context.Context, _ string, ref content.Ref) (ConnectResult, error) {
	r.introductionRef = ref
	return r.connectResult, r.createErr
}

func (r *channelRepository) CreateInbound(_ context.Context, source string, ref content.Ref, uploads []AttachmentRecord) (SendResult, error) {
	r.createdChannel, r.createdRef = source, ref
	r.createdUploads = append([]AttachmentRecord(nil), uploads...)
	if r.createErr != nil {
		return SendResult{}, r.createErr
	}
	return SendResult{InteractionID: "interaction", TaskID: "task"}, nil
}
func (r *channelRepository) CreateExternalInbound(_ context.Context, source string, _ ExternalInteraction, ref content.Ref, uploads []AttachmentRecord) (SendResult, bool, error) {
	result, err := r.CreateInbound(context.Background(), source, ref, uploads)
	return result, true, err
}
func (r *channelRepository) ListMessages(context.Context, int64, int) ([]MessageRecord, error) {
	r.listMode = "after"
	return r.records, nil
}
func (r *channelRepository) ListMessagesBefore(context.Context, int64, int) ([]MessageRecord, error) {
	r.listMode = "before"
	return r.records, nil
}
func (r *channelRepository) ListMessagesForTask(context.Context, string) ([]MessageRecord, error) {
	return r.records, nil
}
func (*channelRepository) TaskStatus(context.Context, string) (TaskStatus, error) {
	return TaskStatus{}, nil
}
func (*channelRepository) Presence(context.Context) (Presence, error) { return Presence{}, nil }
func (*channelRepository) ListEvents(context.Context, int64, int) ([]eventlog.Event, error) {
	return nil, nil
}
func (r *channelRepository) ApprovalStatus(context.Context, string) (string, error) {
	return r.approvalStatus, nil
}
func (*channelRepository) LoadAttachment(context.Context, string) (AttachmentRecord, bool, error) {
	return AttachmentRecord{}, false, nil
}

type channelContent struct {
	bodies  map[string][]byte
	deleted []string
	next    int
}

func (c *channelContent) Put(_ context.Context, body []byte, metadata content.Metadata) (content.Ref, error) {
	c.next++
	id := fmt.Sprintf("content-%d", c.next)
	if c.bodies == nil {
		c.bodies = map[string][]byte{}
	}
	c.bodies[id] = append([]byte(nil), body...)
	return content.Ref{ObjectID: id, MediaType: metadata.MediaType, EncryptionDomain: metadata.EncryptionDomain, PrivacyClass: metadata.PrivacyClass, RetentionPolicy: metadata.RetentionPolicy, ProvenanceRef: metadata.ProvenanceRef}, nil
}
func (c *channelContent) Get(_ context.Context, ref content.Ref) ([]byte, error) {
	body, found := c.bodies[ref.ObjectID]
	if !found {
		return nil, errors.New("content missing")
	}
	return append([]byte(nil), body...), nil
}
func (c *channelContent) Delete(_ context.Context, ref content.Ref) error {
	c.deleted = append(c.deleted, ref.ObjectID)
	delete(c.bodies, ref.ObjectID)
	return nil
}

func TestSendCommitsTrimmedContentAndGovernedAttachments(t *testing.T) {
	repository := &channelRepository{}
	contents := &channelContent{}
	service := NewService(repository, contents)
	result, err := service.SendWithAttachments(context.Background(), "cli", "  hello  ", []AttachmentUpload{{Name: "note.txt", MediaType: "text/plain", Body: []byte("attachment")}})
	if err != nil {
		t.Fatal(err)
	}
	if result.TaskID != "task" || repository.createdChannel != "cli" || string(contents.bodies[repository.createdRef.ObjectID]) != "hello" {
		t.Fatalf("inbound boundary changed content: result=%+v repository=%+v", result, repository)
	}
	if len(repository.createdUploads) != 1 || repository.createdUploads[0].Name != "note.txt" || repository.createdUploads[0].ContentRef.ProvenanceRef != repository.createdUploads[0].ID {
		t.Fatalf("attachment metadata = %+v", repository.createdUploads)
	}
	if len(contents.deleted) != 0 {
		t.Fatalf("committed content was deleted: %v", contents.deleted)
	}
}

func TestConnectStoresOnlyAnInternalInstructionAndDeletesDuplicateContent(t *testing.T) {
	repository := &channelRepository{connectResult: ConnectResult{IntroductionStarted: true, TaskID: "intro-task"}}
	contents := &channelContent{}
	result, err := NewService(repository, contents).Connect(context.Background(), "conversation_web", ConnectRequest{Locale: "zh-CN", Timezone: "Asia/Shanghai"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IntroductionStarted || result.TaskID != "intro-task" {
		t.Fatalf("connect result=%+v", result)
	}
	instruction := string(contents.bodies[repository.introductionRef.ObjectID])
	if !strings.Contains(instruction, "one or two short, natural sentences") || !strings.Contains(instruction, `"zh-CN"`) || strings.Contains(instruction, "Hello") || strings.Contains(instruction, "I am Eri") {
		t.Fatalf("introduction trigger is not a model instruction: %q", instruction)
	}
	repository.connectResult = ConnectResult{IntroductionStarted: false, TaskID: "intro-task"}
	if _, err := NewService(repository, contents).Connect(context.Background(), "conversation_web", ConnectRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(contents.deleted) != 1 {
		t.Fatalf("duplicate connection content was not deleted: %v", contents.deleted)
	}
}

func TestSendDeletesEveryNewContentObjectWhenCommitFails(t *testing.T) {
	repository := &channelRepository{createErr: errors.New("database unavailable")}
	contents := &channelContent{}
	_, err := NewService(repository, contents).SendWithAttachments(context.Background(), "cli", "hello", []AttachmentUpload{{Name: "note.txt", Body: []byte("attachment")}})
	if err == nil {
		t.Fatal("commit failure was hidden")
	}
	if len(contents.deleted) != 2 || len(contents.bodies) != 0 {
		t.Fatalf("orphaned governed content: deleted=%v bodies=%v", contents.deleted, contents.bodies)
	}
}

func TestMessagesKeepSystemCardsStructuredAndResolveApprovalStatus(t *testing.T) {
	contents := &channelContent{bodies: map[string][]byte{
		"approval": []byte(`{"approval_id":"approval-1","tool_id":"builtin.files"}`),
		"failure":  []byte(`{"code":"model_unavailable"}`),
		"reply":    []byte("model-generated reply"),
	}}
	repository := &channelRepository{approvalStatus: "pending", records: []MessageRecord{
		{ID: "approval-message", Kind: "approval_request", Role: "system", ContentRef: content.Ref{ObjectID: "approval"}, CreatedAt: time.Now()},
		{ID: "failure-message", Kind: "runtime_error", Role: "system", ContentRef: content.Ref{ObjectID: "failure"}, CreatedAt: time.Now()},
		{ID: "reply-message", Kind: "text", Role: "assistant", ContentRef: content.Ref{ObjectID: "reply"}, CreatedAt: time.Now()},
	}}
	messages, err := NewService(repository, contents).Messages(context.Background(), 0, -1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if repository.listMode != "after" || len(messages) != 3 {
		t.Fatalf("messages=%+v list_mode=%s", messages, repository.listMode)
	}
	if messages[0].Content != "" || messages[0].Data["status"] != "pending" || messages[1].Content != "" || messages[1].Data["code"] != "model_unavailable" {
		t.Fatalf("system cards leaked into assistant text: %+v", messages)
	}
	if messages[2].Content != "model-generated reply" || messages[2].Data != nil {
		t.Fatalf("assistant reply was not preserved: %+v", messages[2])
	}
}
