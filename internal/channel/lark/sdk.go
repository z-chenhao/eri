package lark

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	larksdk "github.com/larksuite/oapi-sdk-go/v3"
	"github.com/larksuite/oapi-sdk-go/v3/channel/normalize"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
	"github.com/z-chenhao/eri/internal/channel"
	"github.com/z-chenhao/eri/internal/delivery"
)

type SDKPlatform struct {
	client     *larksdk.Client
	websocket  *larkws.Client
	dispatcher *dispatcher.EventDispatcher
	handler    func(context.Context, Message) error
}

func NewSDKPlatform(appID, appSecret, brand string) (*SDKPlatform, error) {
	appID, appSecret = strings.TrimSpace(appID), strings.TrimSpace(appSecret)
	if appID == "" || appSecret == "" {
		return nil, fmt.Errorf("Lark App ID and App Secret are required")
	}
	domain := larksdk.FeishuBaseUrl
	if brand == "lark" {
		domain = larksdk.LarkBaseUrl
	} else if brand != "feishu" {
		return nil, fmt.Errorf("Lark brand must be feishu or lark")
	}
	silent := silentSDKLogger{}
	events := dispatcher.NewEventDispatcher("", "")
	platform := &SDKPlatform{dispatcher: events}
	events.OnP2MessageReceiveV1(platform.receive)
	platform.client = larksdk.NewClient(appID, appSecret,
		larksdk.WithOpenBaseUrl(domain), larksdk.WithLogLevel(larkcore.LogLevelError), larksdk.WithLogger(silent), larksdk.WithReqTimeout(30*time.Second))
	platform.websocket = larkws.NewClient(appID, appSecret,
		larkws.WithDomain(domain), larkws.WithEventHandler(events), larkws.WithLogLevel(larkcore.LogLevelError), larkws.WithLogger(silent))
	return platform, nil
}

func (p *SDKPlatform) Run(ctx context.Context, handler func(context.Context, Message) error, ready func()) error {
	if handler == nil || ready == nil {
		return fmt.Errorf("Lark event handler and ready callback are required")
	}
	p.handler = handler
	p.websocket.SetOnReady(ready)
	return p.websocket.Start(ctx)
}

func (p *SDKPlatform) Stop(context.Context) error {
	p.websocket.Close()
	return nil
}

func (p *SDKPlatform) receive(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
	normalized := normalize.ParseMessage(event)
	if normalized == nil || p.handler == nil {
		return nil
	}
	message := Message{
		MessageID: normalized.MessageID, ConversationID: normalized.ChatID, SenderID: normalized.UserID,
		ChatType: normalized.ChatType, Content: normalized.Content,
	}
	if normalized.CreateTimeMs > 0 {
		message.CreatedAt = time.UnixMilli(normalized.CreateTimeMs).UTC()
	}
	if event.Event != nil && event.Event.Message != nil && event.Event.Message.ParentId != nil {
		message.ReplyToID = *event.Event.Message.ParentId
	}
	for _, resource := range normalized.Resources {
		message.Resources = append(message.Resources, Resource{Type: resource.Type, FileKey: resource.FileKey, FileName: resource.FileName})
	}
	return p.handler(ctx, message)
}

func (p *SDKPlatform) Download(ctx context.Context, resource Resource) ([]byte, error) {
	var reader io.Reader
	if resource.Type == "image" || resource.Type == "sticker" {
		response, err := p.client.Im.V1.Image.Get(ctx, larkim.NewGetImageReqBuilder().ImageKey(resource.FileKey).Build())
		if err != nil {
			return nil, err
		}
		if !response.Success() {
			return nil, fmt.Errorf("Lark image download failed with code %d", response.Code)
		}
		reader = response.File
	} else {
		response, err := p.client.Im.V1.File.Get(ctx, larkim.NewGetFileReqBuilder().FileKey(resource.FileKey).Build())
		if err != nil {
			return nil, err
		}
		if !response.Success() {
			return nil, fmt.Errorf("Lark file download failed with code %d", response.Code)
		}
		reader = response.File
	}
	return io.ReadAll(io.LimitReader(reader, 20*1024*1024+1))
}

func (p *SDKPlatform) Send(ctx context.Context, outbound delivery.Outbound) (delivery.Receipt, error) {
	var firstMessageID string
	if strings.TrimSpace(outbound.Text) != "" {
		messageType, content, err := larkTextPayload(outbound)
		if err != nil {
			return delivery.Receipt{}, err
		}
		firstMessageID, err = p.sendMessage(ctx, outbound.Target, messageType, content, outbound.DeliveryID)
		if err != nil {
			return delivery.Receipt{}, err
		}
	}
	for index, attachment := range outbound.Attachments {
		messageType, content, err := p.uploadAttachment(ctx, attachment)
		if err != nil {
			return delivery.Receipt{}, err
		}
		messageID, err := p.sendMessage(ctx, outbound.Target, messageType, content, fmt.Sprintf("%s-%d", outbound.DeliveryID, index+1))
		if err != nil {
			return delivery.Receipt{}, err
		}
		if firstMessageID == "" {
			firstMessageID = messageID
		}
	}
	if firstMessageID == "" {
		return delivery.Receipt{}, fmt.Errorf("Lark delivery has no sendable content")
	}
	return delivery.Receipt{Level: "accepted_by_channel", ExternalMessageID: firstMessageID}, nil
}

func larkTextPayload(outbound delivery.Outbound) (string, string, error) {
	if outbound.ArtifactKind == "runtime_error" {
		// Internal error codes and correlation IDs belong in the Observatory.
		// This is a last-resort system disclosure after Runtime recovery is
		// exhausted, not model-authored Eri prose.
		body, err := json.Marshal(map[string]string{"text": "I couldn't complete this reliably after retrying. I kept the run evidence so the work can continue safely."})
		return "text", string(body), err
	}
	body, err := json.Marshal(map[string]any{
		"zh_cn": map[string]any{
			"content": [][]map[string]string{{{"tag": "md", "text": outbound.Text}}},
		},
	})
	return "post", string(body), err
}

func (p *SDKPlatform) sendMessage(ctx context.Context, target channel.ExternalTarget, messageType, content, idempotencyKey string) (string, error) {
	if target.ReplyToMessageID != "" {
		request := larkim.NewReplyMessageReqBuilder().MessageId(target.ReplyToMessageID).Body(
			larkim.NewReplyMessageReqBodyBuilder().MsgType(messageType).Content(content).Uuid(idempotencyKey).Build()).Build()
		response, err := p.client.Im.V1.Message.Reply(ctx, request)
		if err != nil {
			return "", err
		}
		if !response.Success() || response.Data == nil || response.Data.MessageId == nil {
			return "", fmt.Errorf("Lark reply failed with code %d", response.Code)
		}
		return *response.Data.MessageId, nil
	}
	request := larkim.NewCreateMessageReqBuilder().ReceiveIdType("chat_id").Body(
		larkim.NewCreateMessageReqBodyBuilder().ReceiveId(target.ConversationID).MsgType(messageType).Content(content).Uuid(idempotencyKey).Build()).Build()
	response, err := p.client.Im.V1.Message.Create(ctx, request)
	if err != nil {
		return "", err
	}
	if !response.Success() || response.Data == nil || response.Data.MessageId == nil {
		return "", fmt.Errorf("Lark message send failed with code %d", response.Code)
	}
	return *response.Data.MessageId, nil
}

func (p *SDKPlatform) uploadAttachment(ctx context.Context, attachment delivery.OutboundAttachment) (string, string, error) {
	if strings.HasPrefix(strings.ToLower(attachment.MediaType), "image/") {
		request := larkim.NewCreateImageReqBuilder().Body(larkim.NewCreateImageReqBodyBuilder().ImageType("message").Image(bytes.NewReader(attachment.Body)).Build()).Build()
		response, err := p.client.Im.V1.Image.Create(ctx, request)
		if err != nil {
			return "", "", err
		}
		if !response.Success() || response.Data == nil || response.Data.ImageKey == nil {
			return "", "", fmt.Errorf("Lark image upload failed with code %d", response.Code)
		}
		content, _ := json.Marshal(map[string]string{"image_key": *response.Data.ImageKey})
		return "image", string(content), nil
	}
	request := larkim.NewCreateFileReqBuilder().Body(larkim.NewCreateFileReqBodyBuilder().FileType("stream").FileName(attachment.Name).File(bytes.NewReader(attachment.Body)).Build()).Build()
	response, err := p.client.Im.V1.File.Create(ctx, request)
	if err != nil {
		return "", "", err
	}
	if !response.Success() || response.Data == nil || response.Data.FileKey == nil {
		return "", "", fmt.Errorf("Lark file upload failed with code %d", response.Code)
	}
	content, _ := json.Marshal(map[string]string{"file_key": *response.Data.FileKey})
	return "file", string(content), nil
}

type silentSDKLogger struct{}

func (silentSDKLogger) Debug(context.Context, ...interface{}) {}
func (silentSDKLogger) Info(context.Context, ...interface{})  {}
func (silentSDKLogger) Warn(context.Context, ...interface{})  {}
func (silentSDKLogger) Error(context.Context, ...interface{}) {}
