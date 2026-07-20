package daemon

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/z-chenhao/eri/internal/agent"
	"github.com/z-chenhao/eri/internal/channel"
	"github.com/z-chenhao/eri/internal/config"
)

type unavailableIntegrationModel struct{ integrationModelCapabilities }

func (unavailableIntegrationModel) Complete(context.Context, agent.ModelRequest) (agent.ModelResponse, error) {
	return agent.ModelResponse{}, errors.New("provider unavailable: token=provider-secret-that-must-not-leak")
}

func TestDaemonProviderFailureIsDiagnosableWithoutFixedAssistantReplyOrSecretLeak(t *testing.T) {
	root := t.TempDir()
	socketFile, err := os.CreateTemp("", "eri-provider-failure-*.sock")
	if err != nil {
		t.Fatal(err)
	}
	socketPath := socketFile.Name()
	if err := socketFile.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(socketPath); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(socketPath) })
	cfg := config.Config{
		DataRoot: root, DatabasePath: filepath.Join(root, "metadata", "eri.db"), SocketPath: socketPath,
		ConversationAddr: "127.0.0.1:0", ObservatoryAddr: "127.0.0.1:0", Model: "unavailable-integration",
		ModelTimeout: time.Second, PollInterval: 5 * time.Millisecond, MaxEvalAttempts: 3, MaxOutputTokens: 256,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, err := New(ctx, cfg, Dependencies{
		MasterKey: bytes.Repeat([]byte{0x4d}, 32), Model: unavailableIntegrationModel{}, Judge: testPassJudge{},
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	waitForSocket(t, socketPath)
	client := integrationUnixClient(socketPath)

	var sent channel.SendResult
	integrationJSON(t, client, http.MethodPost, "/api/v1/messages", map[string]string{"text": "synthetic provider failure"}, &sent)
	var status channel.TaskStatus
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		integrationJSON(t, client, http.MethodGet, "/api/v1/tasks/"+sent.TaskID, nil, &status)
		if status.Status == "failed" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if status.Status != "failed" || status.ErrorCode != "model_unavailable" {
		t.Fatalf("task status = %+v", status)
	}
	var timeline struct {
		Messages []channel.Message `json:"messages"`
	}
	integrationJSON(t, client, http.MethodGet, "/api/v1/tasks/"+sent.TaskID+"/messages", nil, &timeline)
	if len(timeline.Messages) != 2 {
		t.Fatalf("task timeline = %+v", timeline.Messages)
	}
	failure := timeline.Messages[1]
	if failure.Kind != "runtime_error" || failure.Content != "" || failure.Data["code"] != "model_unavailable" {
		t.Fatalf("failure was presented as an assistant reply: %+v", failure)
	}

	integrationJSON(t, client, http.MethodPost, "/api/v1/system/stop", map[string]string{}, &map[string]any{})
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not stop")
	}
	logBody, err := os.ReadFile(filepath.Join(root, "logs", "daemon.log"))
	if err != nil {
		t.Fatal(err)
	}
	logText := string(logBody)
	for _, required := range []string{`"msg":"model call finished"`, `"error_code":"operation_failed"`, `"error":"provider unavailable: token=[REDACTED]"`, `"msg":"invocation failed"`, `"error_code":"model_unavailable"`, `"task_id":"` + sent.TaskID + `"`} {
		if !strings.Contains(logText, required) {
			t.Fatalf("log is missing %q: %s", required, logText)
		}
	}
	for _, forbidden := range []string{"provider-secret-that-must-not-leak", "synthetic provider failure"} {
		if strings.Contains(logText, forbidden) {
			t.Fatalf("log contains private provider or user data %q", forbidden)
		}
	}
}
