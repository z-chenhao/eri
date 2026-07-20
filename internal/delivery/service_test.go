package delivery

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/z-chenhao/eri/internal/channel"
	"github.com/z-chenhao/eri/internal/content"
)

type deliveryRepository struct {
	record      Record
	found       bool
	loadErr     error
	commitErr   error
	committedID string
	interaction string
	committedAt time.Time
	receipt     Receipt
}

func (r *deliveryRepository) LoadDelivery(context.Context, string) (Record, bool, error) {
	return r.record, r.found, r.loadErr
}

func (r *deliveryRepository) CommitConversationDelivery(_ context.Context, deliveryID, interactionID string, receipt Receipt, now time.Time) error {
	r.committedID = deliveryID
	r.interaction = interactionID
	r.committedAt = now
	r.receipt = receipt
	return r.commitErr
}

type deliveryAdapter struct {
	outbound Outbound
	receipt  Receipt
}

func (*deliveryAdapter) Channel() string { return "lark" }
func (a *deliveryAdapter) Send(_ context.Context, outbound Outbound) (Receipt, error) {
	a.outbound = outbound
	return a.receipt, nil
}

type deliveryContent struct {
	missing map[string]error
	reads   []string
}

func (c *deliveryContent) Get(_ context.Context, ref content.Ref) ([]byte, error) {
	c.reads = append(c.reads, ref.ObjectID)
	if err := c.missing[ref.ObjectID]; err != nil {
		return nil, err
	}
	return []byte("verified"), nil
}

func TestSendVerifiesArtifactAndAttachmentsBeforeCommit(t *testing.T) {
	repository := &deliveryRepository{found: true, record: Record{
		ID: "delivery-1", ArtifactID: "artifact-1", ArtifactRef: testDeliveryRef("artifact-ref"), Status: "pending",
		Attachments: []channel.AttachmentRecord{{Attachment: channel.Attachment{ID: "attachment-1"}, ContentRef: testDeliveryRef("attachment-ref")}},
	}}
	contents := &deliveryContent{missing: map[string]error{}}
	service := NewService(repository, contents)
	before := time.Now().UTC()
	if err := service.Send(context.Background(), "delivery-1"); err != nil {
		t.Fatal(err)
	}
	if repository.committedID != "delivery-1" || repository.interaction == "" || repository.committedAt.Before(before) {
		t.Fatalf("delivery was not committed with a fresh receipt: %+v", repository)
	}
	if len(contents.reads) != 2 || contents.reads[0] != "artifact-ref" || contents.reads[1] != "attachment-ref" {
		t.Fatalf("verified refs = %v", contents.reads)
	}
}

func TestSendIsIdempotentForMissingOrTerminalDelivery(t *testing.T) {
	for _, state := range []struct {
		name   string
		found  bool
		status string
	}{
		{name: "missing"},
		{name: "sent", found: true, status: "sent"},
		{name: "acknowledged", found: true, status: "acknowledged"},
	} {
		t.Run(state.name, func(t *testing.T) {
			repository := &deliveryRepository{found: state.found, record: Record{Status: state.status}}
			contents := &deliveryContent{missing: map[string]error{}}
			if err := NewService(repository, contents).Send(context.Background(), "delivery"); err != nil {
				t.Fatal(err)
			}
			if repository.committedID != "" || len(contents.reads) != 0 {
				t.Fatalf("terminal delivery performed work: repository=%+v reads=%v", repository, contents.reads)
			}
		})
	}
}

func TestSendDoesNotCommitUnverifiableContent(t *testing.T) {
	verificationErr := errors.New("content integrity failed")
	for _, record := range []Record{
		{ArtifactID: "artifact", ArtifactRef: testDeliveryRef("missing-artifact"), Status: "pending"},
		{ArtifactID: "artifact", ArtifactRef: testDeliveryRef("artifact"), Status: "pending", Attachments: []channel.AttachmentRecord{{Attachment: channel.Attachment{ID: "attachment"}, ContentRef: testDeliveryRef("missing-attachment")}}},
	} {
		repository := &deliveryRepository{found: true, record: record}
		contents := &deliveryContent{missing: map[string]error{"missing-artifact": verificationErr, "missing-attachment": verificationErr}}
		err := NewService(repository, contents).Send(context.Background(), "delivery")
		if !errors.Is(err, verificationErr) || repository.committedID != "" {
			t.Fatalf("err=%v committed=%q", err, repository.committedID)
		}
	}
}

func TestSendDispatchesRemoteChannelBeforeCommittingPlatformReceipt(t *testing.T) {
	repository := &deliveryRepository{found: true, record: Record{
		ID: "delivery-1", TaskID: "task-1", ArtifactID: "artifact-1", ArtifactRef: testDeliveryRef("artifact-ref"), Status: "pending", TargetChannel: "lark",
		ExternalTarget: channel.ExternalTarget{ConversationID: "oc_chat", ReplyToMessageID: "om_inbound"},
	}}
	contents := &deliveryContent{missing: map[string]error{}}
	adapter := &deliveryAdapter{receipt: Receipt{Level: "accepted_by_channel", ExternalMessageID: "om_outbound"}}
	if err := NewService(repository, contents, adapter).Send(context.Background(), "delivery-1"); err != nil {
		t.Fatal(err)
	}
	if adapter.outbound.Text != "verified" || adapter.outbound.Target.ReplyToMessageID != "om_inbound" || repository.receipt.ExternalMessageID != "om_outbound" {
		t.Fatalf("outbound=%+v receipt=%+v", adapter.outbound, repository.receipt)
	}
}

func TestSendPreservesArtifactKindForChannelPresentation(t *testing.T) {
	repository := &deliveryRepository{found: true, record: Record{
		ID: "delivery-1", TaskID: "task-1", ArtifactID: "artifact-1", ArtifactKind: "runtime_error",
		ArtifactRef: testDeliveryRef("artifact-ref"), Status: "pending", TargetChannel: "lark",
		ExternalTarget: channel.ExternalTarget{ConversationID: "oc_chat"},
	}}
	adapter := &deliveryAdapter{receipt: Receipt{Level: "accepted_by_channel", ExternalMessageID: "om_outbound"}}
	if err := NewService(repository, &deliveryContent{missing: map[string]error{}}, adapter).Send(context.Background(), "delivery-1"); err != nil {
		t.Fatal(err)
	}
	if adapter.outbound.ArtifactKind != "runtime_error" {
		t.Fatalf("artifact kind = %q", adapter.outbound.ArtifactKind)
	}
}

func testDeliveryRef(id string) content.Ref {
	return content.Ref{ObjectID: id, ContentHash: "digest", MediaType: "text/plain", EncryptionDomain: "conversation", PrivacyClass: "private", RetentionPolicy: "user_owned"}
}
