package lark

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/z-chenhao/eri/internal/channel"
	"github.com/z-chenhao/eri/internal/content"
	"github.com/z-chenhao/eri/internal/delivery"
	"github.com/z-chenhao/eri/internal/store/sqlite"
)

type fakePlatform struct {
	downloads map[string][]byte
	sent      delivery.Outbound
	receipt   delivery.Receipt
}

func (*fakePlatform) Run(context.Context, func(context.Context, Message) error, func()) error {
	return nil
}
func (*fakePlatform) Stop(context.Context) error { return nil }
func (p *fakePlatform) Download(_ context.Context, resource Resource) ([]byte, error) {
	return append([]byte(nil), p.downloads[resource.FileKey]...), nil
}
func (p *fakePlatform) Send(_ context.Context, outbound delivery.Outbound) (delivery.Receipt, error) {
	p.sent = outbound
	return p.receipt, nil
}

func TestReceiveBindsOwnerDeduplicatesAndNormalizesAttachment(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "eri.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	contents, err := content.New(filepath.Join(t.TempDir(), "content"), []byte("01234567890123456789012345678901"))
	if err != nil {
		t.Fatal(err)
	}
	ingress := channel.NewService(store, contents)
	platform := &fakePlatform{downloads: map[string][]byte{"file-key": []byte("attachment")}}
	service, err := NewService("ou_owner", ingress, platform, slog.New(slog.NewTextHandler(testDiscardWriter{}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	message := Message{
		MessageID: "om_message", ConversationID: "oc_chat", SenderID: "ou_owner", ChatType: "p2p",
		Content: "hello", CreatedAt: time.Now().UTC(), Resources: []Resource{{Type: "file", FileKey: "file-key", FileName: "note.txt"}},
	}
	if err := service.receive(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	if err := service.receive(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	messages, err := ingress.Messages(context.Background(), 0, -1, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].Channel != ChannelName || messages[0].Content != "hello" || len(messages[0].Attachments) != 1 || messages[0].Attachments[0].MediaType != "text/plain; charset=utf-8" {
		t.Fatalf("messages = %+v", messages)
	}
	events, err := ingress.Events(context.Background(), 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	taskCreated := 0
	for _, event := range events {
		if event.Type == "task.created" {
			taskCreated++
		}
	}
	if taskCreated != 1 {
		t.Fatalf("task.created events = %d, want 1", taskCreated)
	}

	rejected := message
	rejected.MessageID = "om_intruder"
	rejected.SenderID = "ou_intruder"
	if err := service.receive(context.Background(), rejected); err != nil {
		t.Fatal(err)
	}
	rejected.MessageID = "om_group"
	rejected.SenderID = "ou_owner"
	rejected.ChatType = "group"
	if err := service.receive(context.Background(), rejected); err != nil {
		t.Fatal(err)
	}
	messages, err = ingress.Messages(context.Background(), 0, -1, 20)
	if err != nil || len(messages) != 1 {
		t.Fatalf("rejected message changed conversation: count=%d err=%v", len(messages), err)
	}
}

func TestSendRequiresDurableTargetAndReturnsPlatformReceipt(t *testing.T) {
	platform := &fakePlatform{receipt: delivery.Receipt{Level: "accepted_by_channel", ExternalMessageID: "om_reply"}}
	service := &Service{platform: platform}
	if _, err := service.Send(context.Background(), delivery.Outbound{}); err == nil {
		t.Fatal("missing durable target accepted")
	}
	outbound := delivery.Outbound{DeliveryID: "delivery", Target: channel.ExternalTarget{ConversationID: "oc_chat", ReplyToMessageID: "om_inbound"}, Text: "reply"}
	receipt, err := service.Send(context.Background(), outbound)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.ExternalMessageID != "om_reply" || platform.sent.DeliveryID != "delivery" || platform.sent.Target.ReplyToMessageID != "om_inbound" {
		t.Fatalf("receipt=%+v outbound=%+v", receipt, platform.sent)
	}
}

type testDiscardWriter struct{}

func (testDiscardWriter) Write(body []byte) (int, error) { return len(body), nil }
