// Package feedback records explicit post-delivery user evidence and links it
// back to the exact task, artifact and delivery it evaluates.
package feedback

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/z-chenhao/eri/internal/content"
	"github.com/z-chenhao/eri/internal/identifier"
)

type Kind string

const (
	Correction Kind = "correction"
	Accepted   Kind = "accepted"
	Rejected   Kind = "rejected"
	Outcome    Kind = "outcome"
)

type OutcomeStatus string

const (
	OutcomeSuccess OutcomeStatus = "success"
	OutcomeFailure OutcomeStatus = "failure"
	OutcomeMixed   OutcomeStatus = "mixed"
	OutcomeUnknown OutcomeStatus = "unknown"
)

type Record struct {
	ID             string        `json:"id"`
	FeedbackTaskID string        `json:"feedback_task_id"`
	SourceTaskID   string        `json:"source_task_id"`
	ArtifactID     string        `json:"artifact_id"`
	DeliveryID     string        `json:"delivery_id"`
	Kind           Kind          `json:"kind"`
	Outcome        OutcomeStatus `json:"outcome,omitempty"`
	StatementRef   content.Ref   `json:"-"`
	CreatedAt      time.Time     `json:"created_at"`
}

type CaptureRequest struct {
	Record
	RequestedDeliveryID string
}

type Repository interface {
	CaptureFeedback(context.Context, CaptureRequest) (Record, error)
}

type ContentStore interface {
	Put(context.Context, []byte, content.Metadata) (content.Ref, error)
	Delete(context.Context, content.Ref) error
}

type Service struct {
	repository Repository
	content    ContentStore
	now        func() time.Time
}

func NewService(repository Repository, contentStore ContentStore) (*Service, error) {
	if repository == nil || contentStore == nil {
		return nil, fmt.Errorf("feedback repository and content store are required")
	}
	return &Service{repository: repository, content: contentStore, now: time.Now}, nil
}

func (s *Service) Capture(ctx context.Context, feedbackTaskID string, kind Kind, outcome OutcomeStatus, statement, deliveryID string) (Record, error) {
	feedbackTaskID = strings.TrimSpace(feedbackTaskID)
	statement = strings.TrimSpace(statement)
	if feedbackTaskID == "" || statement == "" {
		return Record{}, fmt.Errorf("feedback task id and statement are required")
	}
	if err := validate(kind, outcome); err != nil {
		return Record{}, err
	}
	id, err := identifier.New()
	if err != nil {
		return Record{}, err
	}
	ref, err := s.content.Put(ctx, []byte(statement), content.Metadata{
		MediaType: "text/plain; charset=utf-8", EncryptionDomain: "feedback", PrivacyClass: "private",
		RetentionPolicy: "user_owned", ProvenanceRef: id,
	})
	if err != nil {
		return Record{}, fmt.Errorf("store feedback statement: %w", err)
	}
	record, err := s.repository.CaptureFeedback(ctx, CaptureRequest{
		Record:              Record{ID: id, FeedbackTaskID: feedbackTaskID, Kind: kind, Outcome: outcome, StatementRef: ref, CreatedAt: s.now().UTC()},
		RequestedDeliveryID: strings.TrimSpace(deliveryID),
	})
	if err != nil {
		_ = s.content.Delete(context.Background(), ref)
		return Record{}, err
	}
	return record, nil
}

func validate(kind Kind, outcome OutcomeStatus) error {
	switch kind {
	case Correction, Accepted, Rejected:
		if outcome != "" {
			return fmt.Errorf("outcome is only valid when kind is outcome")
		}
	case Outcome:
		switch outcome {
		case OutcomeSuccess, OutcomeFailure, OutcomeMixed, OutcomeUnknown:
		default:
			return fmt.Errorf("outcome kind requires success, failure, mixed or unknown")
		}
	default:
		return fmt.Errorf("unsupported feedback kind %q", kind)
	}
	return nil
}
