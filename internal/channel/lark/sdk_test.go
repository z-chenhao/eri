package lark

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/z-chenhao/eri/internal/delivery"
)

func TestLarkTextPayloadRendersAssistantMarkdownAsPost(t *testing.T) {
	messageType, body, err := larkTextPayload(delivery.Outbound{
		ArtifactKind: "text",
		Text:         "## Update\n\n- first\n- second",
	})
	if err != nil {
		t.Fatal(err)
	}
	if messageType != "post" {
		t.Fatalf("message type = %q", messageType)
	}
	var payload struct {
		Chinese struct {
			Content [][]struct {
				Tag  string `json:"tag"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"zh_cn"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Chinese.Content) != 1 || len(payload.Chinese.Content[0]) != 1 ||
		payload.Chinese.Content[0][0].Tag != "md" || payload.Chinese.Content[0][0].Text != "## Update\n\n- first\n- second" {
		t.Fatalf("post payload = %+v", payload)
	}
}

func TestLarkRuntimeFailureDoesNotExposeInternalPayload(t *testing.T) {
	messageType, body, err := larkTextPayload(delivery.Outbound{
		ArtifactKind: "runtime_error",
		Text:         `{"code":"model_unavailable","task_id":"private-task","run_id":"private-run"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if messageType != "text" || strings.Contains(body, "model_unavailable") || strings.Contains(body, "private-task") || strings.Contains(body, "private-run") {
		t.Fatalf("unsafe failure payload type=%q body=%s", messageType, body)
	}
}
