package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/z-chenhao/eri/internal/config"
)

func TestRunHelpAndUnknownCommand(t *testing.T) {
	t.Setenv("ERI_DATA_ROOT", t.TempDir())
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"help"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("help exit code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "eri chat") {
		t.Fatalf("help output does not describe chat: %s", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run(context.Background(), []string{"unknown"}, strings.NewReader(""), &stdout, &stderr); code != 2 {
		t.Fatalf("unknown command exit code = %d", code)
	}
	if !strings.Contains(stderr.String(), "unknown command") {
		t.Fatalf("unknown command error missing: %s", stderr.String())
	}
}

func TestDaemonReadyOutputContainsBothWebSurfaces(t *testing.T) {
	var output bytes.Buffer
	printDaemonReady(&output, config.Config{ConversationAddr: "127.0.0.1:7780", ObservatoryAddr: "127.0.0.1:7781"})
	for _, expected := range []string{"Eri is ready", "Conversation: http://127.0.0.1:7780", "Observatory:  http://127.0.0.1:7781", "Ctrl+C"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("ready output missing %q: %s", expected, output.String())
		}
	}
}

func TestDaemonRequiresTerminalForFirstRunInsteadOfServingWebSetup(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ERI_DATA_ROOT", root)
	t.Setenv("ERI_MODEL_PROVIDER", "")
	t.Setenv("ERI_MODEL", "")
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"daemon"}, strings.NewReader("1\n"), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("daemon exit code = %d, stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	message := stderr.String()
	if !strings.Contains(message, "interactive terminal") || !strings.Contains(message, "./bin/eri daemon") {
		t.Fatalf("daemon did not explain terminal setup: %s", message)
	}
	if strings.Contains(stdout.String()+stderr.String(), "http://127.0.0.1:7780") {
		t.Fatalf("daemon still offered Web setup: stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
}

func TestOfflineCommandsGiveNextStepWithoutLeakingSocketPath(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ERI_DATA_ROOT", root)
	for _, args := range [][]string{{"status"}, {"chat", "hello"}, {"stop"}, {"approve", "approval-1"}} {
		var stdout, stderr bytes.Buffer
		if code := Run(context.Background(), args, strings.NewReader(""), &stdout, &stderr); code != 1 {
			t.Fatalf("%v exit code = %d, stdout=%s stderr=%s", args, code, stdout.String(), stderr.String())
		}
		message := stderr.String()
		if !strings.Contains(message, "Eri is offline") || !strings.Contains(message, "eri daemon") || !strings.Contains(message, "eri install") {
			t.Fatalf("%v did not provide a recovery step: %s", args, message)
		}
		for _, leaked := range []string{root, "dial unix", "http://eri.local"} {
			if strings.Contains(message, leaked) {
				t.Fatalf("%v leaked transport detail %q: %s", args, leaked, message)
			}
		}
	}
}

func TestRunOneShotChatStatusApprovalAndStopThroughUnixSocket(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "eri-cli-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	t.Setenv("ERI_DATA_ROOT", root)
	socketPath := filepath.Join(root, "runtime", "eri.sock")
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeCLITestJSON(t, w, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /api/v1/presence", func(w http.ResponseWriter, _ *http.Request) {
		writeCLITestJSON(t, w, map[string]any{"state": "working", "active_tasks": 1})
	})
	mux.HandleFunc("POST /api/v1/conversation/connect", func(w http.ResponseWriter, r *http.Request) {
		var request map[string]string
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode connection request: %v", err)
		}
		writeCLITestJSON(t, w, map[string]any{"introduction_started": false, "task_id": "intro-task"})
	})
	mux.HandleFunc("POST /api/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["text"] != "hello Eri" || len(body) != 1 {
			t.Errorf("unexpected chat request: body=%v err=%v", body, err)
		}
		writeCLITestJSON(t, w, map[string]string{"interaction_id": "interaction-1", "task_id": "task-1"})
	})
	mux.HandleFunc("GET /api/v1/tasks/task-1", func(w http.ResponseWriter, _ *http.Request) {
		writeCLITestJSON(t, w, map[string]string{"id": "task-1", "status": "completed"})
	})
	mux.HandleFunc("GET /api/v1/tasks/task-1/messages", func(w http.ResponseWriter, _ *http.Request) {
		writeCLITestJSON(t, w, map[string]any{"messages": []map[string]string{{
			"task_id": "task-1", "direction": "outbound", "kind": "text", "content": "done safely",
		}}})
	})
	mux.HandleFunc("POST /api/v1/approvals/approval-1", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["decision"] != "approve" {
			t.Errorf("unexpected approval request: body=%v err=%v", body, err)
		}
		writeCLITestJSON(t, w, map[string]string{"approval_id": "approval-1", "task_id": "task-1", "status": "approved"})
	})
	mux.HandleFunc("POST /api/v1/system/stop", func(w http.ResponseWriter, _ *http.Request) {
		writeCLITestJSON(t, w, map[string]bool{"stopping": true})
	})
	stop := serveCLITestUnix(t, socketPath, mux)
	defer stop()

	tests := []struct {
		args    []string
		want    string
		wantErr string
	}{
		{args: []string{"status"}, want: "Eri is online · working · 1 active task(s)"},
		{args: []string{"chat", "hello", "Eri"}, want: "Eri > done safely"},
		{args: []string{"approve", "approval-1"}, want: "Approval approval-1 is approved."},
		{args: []string{"stop"}, want: "Eri is stopping."},
	}
	for _, test := range tests {
		var stdout, stderr bytes.Buffer
		if code := Run(context.Background(), test.args, strings.NewReader(""), &stdout, &stderr); code != 0 {
			t.Fatalf("%v exit code = %d, stderr = %s", test.args, code, stderr.String())
		}
		if !strings.Contains(stdout.String(), test.want) {
			t.Fatalf("%v output = %q, want %q", test.args, stdout.String(), test.want)
		}
		if test.wantErr != "" && !strings.Contains(stderr.String(), test.wantErr) {
			t.Fatalf("%v stderr = %q, want %q", test.args, stderr.String(), test.wantErr)
		}
	}
}

func TestDoctorVerifiesConfiguredOllamaModel(t *testing.T) {
	server := http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeCLITestJSON(t, w, map[string]any{"models": []map[string]string{{"name": "other:latest"}}})
	})}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()

	root := t.TempDir()
	t.Setenv("ERI_DATA_ROOT", root)
	t.Setenv("ERI_OLLAMA_URL", "http://"+listener.Addr().String())
	t.Setenv("ERI_MODEL", "required-model")
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"doctor"}, strings.NewReader(""), &stdout, &stderr); code != 1 {
		t.Fatalf("doctor exit code = %d, stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "required-model is not installed") || !strings.Contains(stdout.String(), filepath.Join(root, "logs", "daemon.log")) || !strings.Contains(stdout.String(), filepath.Join(root, "logs", "bootstrap.log")) {
		t.Fatalf("doctor did not explain model/log diagnosis: stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
}

func TestDoctorExplainsFirstRunSetupRequirement(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ERI_DATA_ROOT", root)
	t.Setenv("ERI_MODEL_PROVIDER", "")
	t.Setenv("ERI_MODEL", "")
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"doctor"}, strings.NewReader(""), &stdout, &stderr); code != 1 {
		t.Fatalf("doctor exit code = %d", code)
	}
	if !strings.Contains(stderr.String(), "model resource: setup required") || !strings.Contains(stderr.String(), "./bin/eri daemon") || strings.Contains(stderr.String(), "http://127.0.0.1:7780") {
		t.Fatalf("doctor did not explain setup: stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
}

func TestParseChatArgsRequiresFilePathAndAcceptsAttachmentOnly(t *testing.T) {
	if _, _, err := parseChatArgs([]string{"--file"}); err == nil {
		t.Fatal("expected missing --file path to fail")
	}
	text, files, err := parseChatArgs([]string{"--file", "brief.txt"})
	if err != nil || text != "" || len(files) != 1 || files[0] != "brief.txt" {
		t.Fatalf("text=%q files=%v err=%v", text, files, err)
	}
}

func serveCLITestUnix(t *testing.T, socketPath string, handler http.Handler) func() {
	t.Helper()
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	go func() { _ = server.Serve(listener) }()
	return func() {
		_ = server.Close()
		_ = listener.Close()
	}
}

func writeCLITestJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("encode response: %v", err)
	}
}
