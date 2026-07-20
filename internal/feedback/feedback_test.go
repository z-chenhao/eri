package feedback

import (
	"context"
	"errors"
	"testing"

	"github.com/z-chenhao/eri/internal/content"
)

type feedbackRepository struct {
	request CaptureRequest
	err     error
}

func (r *feedbackRepository) CaptureFeedback(_ context.Context, request CaptureRequest) (Record, error) {
	r.request = request
	if r.err != nil {
		return Record{}, r.err
	}
	request.Record.SourceTaskID = "source-task"
	request.Record.ArtifactID = "artifact"
	request.Record.DeliveryID = "delivery"
	return request.Record, nil
}

type feedbackContent struct {
	putBody []byte
	putMeta content.Metadata
	ref     content.Ref
	deleted []content.Ref
}

func (c *feedbackContent) Put(_ context.Context, body []byte, metadata content.Metadata) (content.Ref, error) {
	c.putBody = append([]byte(nil), body...)
	c.putMeta = metadata
	c.ref = content.Ref{ObjectID: "feedback-content", EncryptionDomain: metadata.EncryptionDomain, PrivacyClass: metadata.PrivacyClass, RetentionPolicy: metadata.RetentionPolicy, ProvenanceRef: metadata.ProvenanceRef}
	return c.ref, nil
}

func (c *feedbackContent) Delete(_ context.Context, ref content.Ref) error {
	c.deleted = append(c.deleted, ref)
	return nil
}

func TestCaptureStoresExplicitFeedbackAsGovernedContent(t *testing.T) {
	repository := &feedbackRepository{}
	contents := &feedbackContent{}
	service, err := NewService(repository, contents)
	if err != nil {
		t.Fatal(err)
	}
	record, err := service.Capture(context.Background(), " feedback-task ", Outcome, OutcomeMixed, " useful but incomplete ", " requested-delivery ")
	if err != nil {
		t.Fatal(err)
	}
	if string(contents.putBody) != "useful but incomplete" || contents.putMeta.EncryptionDomain != "feedback" || contents.putMeta.PrivacyClass != "private" {
		t.Fatalf("feedback content boundary = body %q metadata %+v", contents.putBody, contents.putMeta)
	}
	if repository.request.FeedbackTaskID != "feedback-task" || repository.request.RequestedDeliveryID != "requested-delivery" || repository.request.Kind != Outcome || repository.request.Outcome != OutcomeMixed {
		t.Fatalf("capture request = %+v", repository.request)
	}
	if record.SourceTaskID != "source-task" || record.StatementRef.ObjectID != "feedback-content" || len(contents.deleted) != 0 {
		t.Fatalf("record=%+v deleted=%+v", record, contents.deleted)
	}
}

func TestCaptureRejectsInvalidEvidenceBeforeWriting(t *testing.T) {
	for _, input := range []struct {
		kind    Kind
		outcome OutcomeStatus
	}{
		{kind: "invented"},
		{kind: Correction, outcome: OutcomeSuccess},
		{kind: Outcome},
	} {
		contents := &feedbackContent{}
		service, err := NewService(&feedbackRepository{}, contents)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.Capture(context.Background(), "task", input.kind, input.outcome, "statement", ""); err == nil {
			t.Fatalf("invalid feedback was accepted: %+v", input)
		}
		if len(contents.putBody) != 0 {
			t.Fatalf("invalid feedback was persisted: %+v", input)
		}
	}
}

func TestCaptureDeletesNewContentWhenRepositoryCommitFails(t *testing.T) {
	repositoryErr := errors.New("commit failed")
	repository := &feedbackRepository{err: repositoryErr}
	contents := &feedbackContent{}
	service, err := NewService(repository, contents)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Capture(context.Background(), "task", Correction, "", "wrong date", ""); !errors.Is(err, repositoryErr) {
		t.Fatalf("capture error = %v", err)
	}
	if len(contents.deleted) != 1 || contents.deleted[0].ObjectID != "feedback-content" {
		t.Fatalf("orphaned feedback content: %+v", contents.deleted)
	}
}
