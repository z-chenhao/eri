// Package delivery sends evaluated artifacts through idempotent channel
// adapters and records only receipts actually obtained.
package delivery

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/z-chenhao/eri/internal/channel"
	"github.com/z-chenhao/eri/internal/content"
	"github.com/z-chenhao/eri/internal/identifier"
	"github.com/z-chenhao/eri/internal/runtime"
)

type Record struct {
	ID             string
	TaskID         string
	ArtifactID     string
	ArtifactKind   string
	ArtifactRef    content.Ref
	TargetChannel  string
	Status         string
	TerminalStatus string
	ContinueTask   bool
	Attachments    []channel.AttachmentRecord
	ExternalTarget channel.ExternalTarget
}

type Receipt struct {
	Level             string
	ExternalMessageID string
}

type OutboundAttachment struct {
	Name      string
	MediaType string
	Body      []byte
}

type Outbound struct {
	DeliveryID   string
	TaskID       string
	ArtifactKind string
	Text         string
	Target       channel.ExternalTarget
	Attachments  []OutboundAttachment
}

// Adapter is defined by Delivery, its consumer. A remote Channel can dispatch
// only the evaluated bytes and durable target selected by Runtime.
type Adapter interface {
	Channel() string
	Send(context.Context, Outbound) (Receipt, error)
}

type Repository interface {
	LoadDelivery(context.Context, string) (Record, bool, error)
	CommitConversationDelivery(context.Context, string, string, Receipt, time.Time) error
}

type ContentStore interface {
	Get(context.Context, content.Ref) ([]byte, error)
}

type Service struct {
	repository Repository
	content    ContentStore
	adapters   map[string]Adapter
}

func NewService(repository Repository, contentStore ContentStore, adapters ...Adapter) *Service {
	service := &Service{repository: repository, content: contentStore, adapters: make(map[string]Adapter)}
	for _, adapter := range adapters {
		if adapter == nil || strings.TrimSpace(adapter.Channel()) == "" {
			continue
		}
		service.adapters[adapter.Channel()] = adapter
	}
	return service
}

func (s *Service) HandleSend(ctx context.Context, item runtime.OutboxItem) error {
	return s.Send(ctx, item.AggregateID)
}

func (s *Service) Send(ctx context.Context, deliveryID string) error {
	record, found, err := s.repository.LoadDelivery(ctx, deliveryID)
	if err != nil {
		return err
	}
	if !found || record.Status == "sent" || record.Status == "acknowledged" {
		return nil
	}
	// Resolve and authenticate the exact bytes immediately before dispatch.
	body, err := s.content.Get(ctx, record.ArtifactRef)
	if err != nil {
		return fmt.Errorf("verify delivery artifact %s: %w", record.ArtifactID, err)
	}
	outbound := Outbound{
		DeliveryID: record.ID, TaskID: record.TaskID, ArtifactKind: record.ArtifactKind,
		Text: string(body), Target: record.ExternalTarget,
		Attachments: make([]OutboundAttachment, 0, len(record.Attachments)),
	}
	for _, attachment := range record.Attachments {
		body, err := s.content.Get(ctx, attachment.ContentRef)
		if err != nil {
			return fmt.Errorf("verify delivery attachment %s: %w", attachment.ID, err)
		}
		outbound.Attachments = append(outbound.Attachments, OutboundAttachment{Name: attachment.Name, MediaType: attachment.MediaType, Body: body})
	}
	receipt := Receipt{Level: "accepted_by_channel"}
	if adapter := s.adapters[record.TargetChannel]; adapter != nil {
		receipt, err = adapter.Send(ctx, outbound)
		if err != nil {
			return fmt.Errorf("send delivery %s through %s: %w", deliveryID, record.TargetChannel, err)
		}
		if receipt.Level == "" || receipt.ExternalMessageID == "" {
			return fmt.Errorf("send delivery %s through %s returned an incomplete receipt", deliveryID, record.TargetChannel)
		}
	} else if record.TargetChannel != "" && record.TargetChannel != "cli" && record.TargetChannel != "conversation_web" {
		return fmt.Errorf("no delivery adapter registered for channel %q", record.TargetChannel)
	}
	interactionID, err := identifier.New()
	if err != nil {
		return err
	}
	return s.repository.CommitConversationDelivery(ctx, deliveryID, interactionID, receipt, time.Now().UTC())
}
