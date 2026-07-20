// Package lark connects one trusted Lark or Feishu application bot to Eri's
// authoritative Conversation and evaluated Delivery boundary.
package lark

import (
	"context"
	"fmt"
	"log/slog"
	"mime"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/z-chenhao/eri/internal/channel"
	"github.com/z-chenhao/eri/internal/delivery"
)

const ChannelName = "lark"

type Resource struct {
	Type     string
	FileKey  string
	FileName string
}

type Message struct {
	MessageID      string
	ConversationID string
	SenderID       string
	ChatType       string
	Content        string
	ReplyToID      string
	CreatedAt      time.Time
	Resources      []Resource
}

type Platform interface {
	Run(context.Context, func(context.Context, Message) error, func()) error
	Stop(context.Context) error
	Download(context.Context, Resource) ([]byte, error)
	Send(context.Context, delivery.Outbound) (delivery.Receipt, error)
}

type Service struct {
	ownerOpenID string
	ingress     *channel.Service
	platform    Platform
	logger      *slog.Logger
	ready       chan struct{}
	readyOnce   sync.Once
	receiveMu   sync.Mutex
}

func NewService(ownerOpenID string, ingress *channel.Service, platform Platform, logger *slog.Logger) (*Service, error) {
	ownerOpenID = strings.TrimSpace(ownerOpenID)
	if ownerOpenID == "" || ingress == nil || platform == nil {
		return nil, fmt.Errorf("Lark owner binding, conversation ingress, and platform are required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{ownerOpenID: ownerOpenID, ingress: ingress, platform: platform, logger: logger, ready: make(chan struct{})}, nil
}

func (*Service) Channel() string { return ChannelName }

func (s *Service) Ready() <-chan struct{} { return s.ready }

func (s *Service) Run(ctx context.Context) error {
	return s.platform.Run(ctx, s.receive, func() {
		s.readyOnce.Do(func() { close(s.ready) })
		s.logger.Info("Lark channel ready", "component", "lark_channel")
	})
}

func (s *Service) Stop(ctx context.Context) error { return s.platform.Stop(ctx) }

func (s *Service) Send(ctx context.Context, outbound delivery.Outbound) (delivery.Receipt, error) {
	if strings.TrimSpace(outbound.Target.ConversationID) == "" {
		return delivery.Receipt{}, fmt.Errorf("Lark delivery target is missing")
	}
	return s.platform.Send(ctx, outbound)
}

func (s *Service) receive(ctx context.Context, message Message) error {
	// The owner has one authoritative Conversation. Serialize platform callbacks
	// so their observed arrival order is preserved through durable ingestion.
	s.receiveMu.Lock()
	defer s.receiveMu.Unlock()

	if message.ChatType != "p2p" || message.SenderID != s.ownerOpenID {
		s.logger.Warn("Lark message rejected", "component", "lark_channel", "reason", "sender_or_chat_not_bound")
		return nil
	}
	if message.MessageID == "" || message.ConversationID == "" {
		return fmt.Errorf("Lark message identity is incomplete")
	}
	uploads := make([]channel.AttachmentUpload, 0, len(message.Resources))
	for _, resource := range message.Resources {
		if resource.FileKey == "" {
			continue
		}
		body, err := s.platform.Download(ctx, resource)
		if err != nil {
			return fmt.Errorf("download Lark attachment: %w", err)
		}
		name := strings.TrimSpace(filepath.Base(resource.FileName))
		if name == "" || name == "." {
			name = defaultResourceName(resource.Type)
		}
		uploads = append(uploads, channel.AttachmentUpload{Name: name, MediaType: resourceMediaType(resource, name), Body: body})
	}
	result, created, err := s.ingress.SendExternalWithAttachments(ctx, ChannelName, channel.ExternalInteraction{
		MessageID: message.MessageID, ConversationID: message.ConversationID, SenderID: message.SenderID,
		ReplyToMessageID: message.ReplyToID, CreatedAt: message.CreatedAt,
	}, message.Content, uploads)
	if err != nil {
		return err
	}
	s.logger.Info("Lark inbound message accepted", "component", "lark_channel", "created", created,
		"interaction_id", result.InteractionID, "task_id", result.TaskID, "text_bytes", len([]byte(message.Content)), "attachment_count", len(uploads))
	return nil
}

func defaultResourceName(kind string) string {
	switch kind {
	case "image", "sticker":
		return "image.png"
	case "audio":
		return "audio.opus"
	case "video", "media":
		return "video.mp4"
	default:
		return "attachment.bin"
	}
}

func resourceMediaType(resource Resource, name string) string {
	if mediaType := mime.TypeByExtension(strings.ToLower(filepath.Ext(name))); mediaType != "" {
		return mediaType
	}
	switch resource.Type {
	case "image", "sticker":
		return "image/png"
	case "audio":
		return "audio/ogg"
	case "video", "media":
		return "video/mp4"
	default:
		return "application/octet-stream"
	}
}
