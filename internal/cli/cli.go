// Package cli implements the short-lived client modes of the single Eri
// binary. Product logic remains in the daemon.
package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/z-chenhao/eri/internal/approval"
	"github.com/z-chenhao/eri/internal/bootstrap"
	"github.com/z-chenhao/eri/internal/channel"
	"github.com/z-chenhao/eri/internal/config"
	"github.com/z-chenhao/eri/internal/daemon"
	"github.com/z-chenhao/eri/internal/model/ollama"
	"github.com/z-chenhao/eri/internal/observability"
	"github.com/z-chenhao/eri/internal/providersecret"
)

var errDaemonUnavailable = errors.New("Eri daemon is unavailable")

func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "daemon" {
		truncateBootstrapLog()
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(stderr, "eri:", err)
		return 1
	}
	if len(args) == 0 {
		printHelp(stdout)
		return 0
	}
	switch args[0] {
	case "daemon":
		return runDaemon(ctx, cfg, stdin, stdout, stderr)
	case "provider-secret-broker":
		return runProviderSecretBroker(ctx, cfg, stderr)
	case "install":
		return runServiceInstall(ctx, cfg, stdin, stdout, stderr)
	case "uninstall":
		return runServiceUninstall(ctx, cfg, stdout, stderr)
	case "chat":
		return runChat(ctx, cfg, args[1:], stdin, stdout, stderr)
	case "status":
		return runStatus(ctx, cfg, stdout, stderr)
	case "stop":
		return runStop(ctx, cfg, stdout, stderr)
	case "doctor":
		return runDoctor(ctx, cfg, stdout, stderr)
	case "logs":
		return runLogs(ctx, cfg, args[1:], stdout, stderr)
	case "diagnose":
		return runDiagnose(ctx, cfg, args[1:], stdout, stderr)
	case "approve":
		return runApprovalDecision(ctx, cfg, args[1:], approval.Approve, stdout, stderr)
	case "deny":
		return runApprovalDecision(ctx, cfg, args[1:], approval.Deny, stdout, stderr)
	case "help", "-h", "--help":
		printHelp(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "eri: unknown command %q\n", args[0])
		printHelp(stderr)
		return 2
	}
}

func runDaemon(ctx context.Context, cfg config.Config, stdin io.Reader, stdout, stderr io.Writer) int {
	signalContext, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	broker := providersecret.NewProcess(cfg.DataRoot, cfg.ProviderBrokerSocket)
	defer broker.Close()
	if err := daemon.PrepareDataRoot(cfg.DataRoot); err != nil {
		fmt.Fprintln(stderr, "eri daemon:", observability.SafeError(err))
		return 1
	}
	cfg, err := ensureModelConfigured(signalContext, cfg, broker, stdin, stdout, true)
	if err != nil {
		fmt.Fprintln(stderr, "eri setup:", observability.SafeError(err))
		return 1
	}
	if cfg.ModelProvider == "deepseek" && !cfg.DeepSeekKeySet {
		if err := broker.Ensure(signalContext); err != nil {
			fmt.Fprintln(stderr, "eri daemon: secure DeepSeek credential broker is unavailable")
			return 1
		}
		configured, err := providersecret.NewClient(cfg.ProviderBrokerSocket).Configured(signalContext)
		if err != nil || !configured {
			fmt.Fprintln(stderr, "eri daemon: DeepSeek has no credential in macOS Keychain; remove the model profile and run setup again")
			return 1
		}
	}
	logger, logCloser, err := observability.NewProcessLogger(filepath.Join(cfg.DataRoot, "logs", "daemon.log"), foregroundLogWriter(stderr))
	if err != nil {
		fmt.Fprintln(stderr, "eri daemon: initialize logging failed")
		return 1
	}
	defer logCloser.Close()
	d, err := daemon.New(signalContext, cfg, daemon.Dependencies{Logger: logger})
	if err != nil {
		fmt.Fprintln(stderr, "eri daemon:", observability.SafeError(err))
		return 1
	}
	defer d.Close()
	runResult := make(chan error, 1)
	go func() { runResult <- d.Run(signalContext) }()
	select {
	case err := <-runResult:
		if err != nil {
			fmt.Fprintln(stderr, "eri daemon:", observability.SafeError(err))
			return 1
		}
		return 0
	case <-d.Ready():
		printDaemonReady(stdout, cfg)
	}
	if err := <-runResult; err != nil {
		fmt.Fprintln(stderr, "eri daemon:", observability.SafeError(err))
		return 1
	}
	return 0
}

func printDaemonReady(output io.Writer, cfg config.Config) {
	fmt.Fprintln(output, "Eri is ready.")
	fmt.Fprintf(output, "  Conversation: http://%s\n", cfg.ConversationAddr)
	fmt.Fprintf(output, "  Observatory:  http://%s\n", cfg.ObservatoryAddr)
	fmt.Fprintln(output, "Press Ctrl+C to stop.")
}

func ensureModelConfigured(ctx context.Context, cfg config.Config, broker *providersecret.Process, stdin io.Reader, stdout io.Writer, allowEnvironmentCredential bool) (config.Config, error) {
	if cfg.ModelConfigured && cfg.ModelProvider == "deepseek" && (!cfg.DeepSeekKeySet || !allowEnvironmentCredential) {
		if err := broker.Ensure(ctx); err != nil {
			if cfg.ModelEnvironmentOverride && allowEnvironmentCredential {
				return config.Config{}, fmt.Errorf("the DeepSeek environment override has no usable credential")
			}
			cfg.ModelConfigured = false
		} else if configured, err := providersecret.NewClient(cfg.ProviderBrokerSocket).Configured(ctx); err != nil || !configured {
			if cfg.ModelEnvironmentOverride && allowEnvironmentCredential {
				return config.Config{}, fmt.Errorf("the DeepSeek environment override has no usable credential")
			}
			cfg.ModelConfigured = false
		}
	}
	if cfg.ModelConfigured {
		return cfg, nil
	}
	prompt, err := bootstrap.NewTerminalPrompter(stdin, stdout)
	if err != nil {
		return config.Config{}, fmt.Errorf("%w; run `./bin/eri daemon` in an interactive terminal to configure a model", err)
	}
	return bootstrap.Run(ctx, cfg, broker, prompt, stdout)
}

func runProviderSecretBroker(ctx context.Context, cfg config.Config, stderr io.Writer) int {
	signalContext, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := os.MkdirAll(filepath.Join(cfg.DataRoot, "logs"), 0o700); err != nil {
		fmt.Fprintln(stderr, "eri provider-secret-broker: initialize logs failed")
		return 1
	}
	logger, closer, err := observability.NewProcessLogger(filepath.Join(cfg.DataRoot, "logs", "provider-secret-broker.log"), nil)
	if err != nil {
		fmt.Fprintln(stderr, "eri provider-secret-broker: initialize log failed")
		return 1
	}
	defer closer.Close()
	logger.Info("Provider Secret Broker starting", "component", "provider_secret_broker")
	if err := providersecret.Serve(signalContext, cfg.DataRoot, cfg.ProviderBrokerSocket, logger); err != nil {
		logger.Error("Provider Secret Broker stopped", "component", "provider_secret_broker", "error", observability.SafeError(err))
		return 1
	}
	logger.Info("Provider Secret Broker stopped", "component", "provider_secret_broker")
	return 0
}

// launchd opens bootstrap.log before starting Eri. Truncating the same inode
// on every attempt keeps only the latest safe startup diagnostic instead of
// allowing a failing KeepAlive service to grow an unbounded secondary log.
func truncateBootstrapLog() {
	root := strings.TrimSpace(os.Getenv("ERI_DATA_ROOT"))
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) == string(filepath.Separator) {
		return
	}
	directory := filepath.Join(root, "logs")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return
	}
	_ = os.Chmod(directory, 0o700)
	file, err := os.OpenFile(filepath.Join(directory, "bootstrap.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return
	}
	_ = file.Chmod(0o600)
	_ = file.Close()
}

func runChat(ctx context.Context, cfg config.Config, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	client := unixClient(cfg.SocketPath)
	if err := connectCLIConversation(ctx, client, stdout); err != nil {
		printClientError(stderr, "eri chat", err)
		return 1
	}
	if len(args) > 0 {
		text, files, err := parseChatArgs(args)
		if err != nil {
			fmt.Fprintln(stderr, "eri chat:", err)
			return 2
		}
		if err := chatTurnWithFiles(ctx, client, text, files, stdout); err != nil {
			printClientError(stderr, "eri chat", err)
			return 1
		}
		return 0
	}
	scanner := bufio.NewScanner(stdin)
	buffer := make([]byte, 64*1024)
	scanner.Buffer(buffer, 1024*1024)
	fmt.Fprintln(stdout, "Eri CLI · enter /exit to quit")
	for {
		fmt.Fprint(stdout, "You > ")
		if !scanner.Scan() {
			break
		}
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		if text == "/exit" || text == "/quit" {
			break
		}
		if err := chatTurn(ctx, client, text, stdout); err != nil {
			printClientError(stderr, "eri chat", err)
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(stderr, "eri chat:", err)
		return 1
	}
	return 0
}

func connectCLIConversation(ctx context.Context, client *http.Client, stdout io.Writer) error {
	payload, err := json.Marshal(channel.ConnectRequest{
		Locale: strings.TrimSpace(os.Getenv("LANG")), Timezone: time.Now().Location().String(),
	})
	if err != nil {
		return err
	}
	var result channel.ConnectResult
	if err := doJSON(ctx, client, http.MethodPost, "/api/v1/conversation/connect", payload, &result); err != nil {
		return err
	}
	if !result.IntroductionStarted {
		return nil
	}
	return waitForTaskDelivery(ctx, client, result.TaskID, stdout)
}

func chatTurn(ctx context.Context, client *http.Client, text string, stdout io.Writer) error {
	return chatTurnWithFiles(ctx, client, text, nil, stdout)
}

func parseChatArgs(args []string) (string, []string, error) {
	files := make([]string, 0)
	words := make([]string, 0)
	for index := 0; index < len(args); index++ {
		if args[index] != "--file" {
			words = append(words, args[index])
			continue
		}
		if index+1 >= len(args) {
			return "", nil, fmt.Errorf("--file requires a path")
		}
		index++
		files = append(files, args[index])
	}
	if strings.TrimSpace(strings.Join(words, " ")) == "" && len(files) == 0 {
		return "", nil, fmt.Errorf("message text or --file is required")
	}
	return strings.Join(words, " "), files, nil
}

func chatTurnWithFiles(ctx context.Context, client *http.Client, text string, files []string, stdout io.Writer) error {
	if len(files) == 0 {
		return sendChatAndWait(ctx, client, text, nil, "application/json", stdout)
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("text", text); err != nil {
		return err
	}
	for _, filePath := range files {
		info, err := os.Stat(filePath)
		if err != nil {
			return fmt.Errorf("read attachment %s: %w", filePath, err)
		}
		if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 10*1024*1024 {
			return fmt.Errorf("attachment %s must be a regular file between 1 byte and 10 MiB", filePath)
		}
		file, err := os.Open(filePath)
		if err != nil {
			return err
		}
		part, err := writer.CreateFormFile("files", filepath.Base(filePath))
		if err == nil {
			_, err = io.Copy(part, io.LimitReader(file, 10*1024*1024+1))
		}
		file.Close()
		if err != nil {
			return err
		}
	}
	if err := writer.Close(); err != nil {
		return err
	}
	return sendChatAndWait(ctx, client, text, body.Bytes(), writer.FormDataContentType(), stdout)
}

func sendChatAndWait(ctx context.Context, client *http.Client, text string, multipartBody []byte, contentType string, stdout io.Writer) error {
	payload, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return err
	}
	var sent channel.SendResult
	if multipartBody == nil {
		if err := doJSON(ctx, client, http.MethodPost, "/api/v1/messages", payload, &sent); err != nil {
			return err
		}
	} else {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://eri.local/api/v1/messages", bytes.NewReader(multipartBody))
		if err != nil {
			return err
		}
		request.Header.Set("Content-Type", contentType)
		response, err := client.Do(request)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("%w: %s", errDaemonUnavailable, observability.SafeError(err))
		}
		defer response.Body.Close()
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return fmt.Errorf("attachment send failed: %s", response.Status)
		}
		if err := json.NewDecoder(io.LimitReader(response.Body, 64*1024)).Decode(&sent); err != nil {
			return err
		}
	}
	return waitForTaskDelivery(ctx, client, sent.TaskID, stdout)
}

func waitForTaskDelivery(ctx context.Context, client *http.Client, taskID string, stdout io.Writer) error {
	if strings.TrimSpace(taskID) == "" {
		return fmt.Errorf("task id is required")
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		var status channel.TaskStatus
		if err := doJSON(ctx, client, http.MethodGet, "/api/v1/tasks/"+taskID, nil, &status); err != nil {
			return err
		}
		terminal := status.Status == "completed" || status.Status == "failed" || status.Status == "canceled"
		if terminal || status.Status == "waiting" {
			var response struct {
				Messages []channel.Message `json:"messages"`
			}
			if err := doJSON(ctx, client, http.MethodGet, "/api/v1/tasks/"+taskID+"/messages", nil, &response); err != nil {
				return err
			}
			for index := len(response.Messages) - 1; index >= 0; index-- {
				message := response.Messages[index]
				if message.TaskID == taskID && message.Direction == "outbound" && (terminal || message.Kind == "approval_request") {
					if message.Kind == "runtime_error" {
						fmt.Fprintf(stdout, "System > task failed (%v)\n", message.Data["code"])
					} else if message.Kind != "approval_request" {
						fmt.Fprintln(stdout, "Eri >", message.Content)
					}
					if message.Kind == "approval_request" {
						if id, ok := message.Data["approval_id"].(string); ok {
							fmt.Fprintf(stdout, "Approval > %v %v %v; %s (eri approve %s | eri deny %s)\n", message.Data["tool_id"], message.Data["effect"], message.Data["target"], id, id, id)
						}
					}
					return nil
				}
			}
			if terminal {
				return fmt.Errorf("task %s ended without a delivery", taskID)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func runApprovalDecision(ctx context.Context, cfg config.Config, args []string, decision approval.Decision, stdout, stderr io.Writer) int {
	if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
		fmt.Fprintf(stderr, "eri %s: approval id is required\n", decision)
		return 2
	}
	payload, err := json.Marshal(map[string]approval.Decision{"decision": decision})
	if err != nil {
		fmt.Fprintln(stderr, "eri approval:", err)
		return 1
	}
	var result approval.Result
	if err := doJSON(ctx, unixClient(cfg.SocketPath), http.MethodPost, "/api/v1/approvals/"+args[0], payload, &result); err != nil {
		printClientError(stderr, "eri approval", err)
		return 1
	}
	fmt.Fprintf(stdout, "Approval %s is %s.\n", result.ApprovalID, result.Status)
	return 0
}

func runStatus(ctx context.Context, cfg config.Config, stdout, stderr io.Writer) int {
	client := unixClient(cfg.SocketPath)
	var health map[string]any
	if err := doJSON(ctx, client, http.MethodGet, "/health", nil, &health); err != nil {
		printClientError(stderr, "eri status", err)
		return 1
	}
	var presence channel.Presence
	if err := doJSON(ctx, client, http.MethodGet, "/api/v1/presence", nil, &presence); err != nil {
		fmt.Fprintln(stderr, "eri status:", err)
		return 1
	}
	fmt.Fprintf(stdout, "Eri is online · %s · %d active task(s)\n", presence.State, presence.ActiveTasks)
	return 0
}

func runStop(ctx context.Context, cfg config.Config, stdout, stderr io.Writer) int {
	client := unixClient(cfg.SocketPath)
	var result map[string]any
	if err := doJSON(ctx, client, http.MethodPost, "/api/v1/system/stop", []byte(`{}`), &result); err != nil {
		printClientError(stderr, "eri stop", err)
		return 1
	}
	fmt.Fprintln(stdout, "Eri is stopping.")
	return 0
}

func runDoctor(ctx context.Context, cfg config.Config, stdout, stderr io.Writer) int {
	failed := false
	fmt.Fprintln(stdout, "Eri doctor")
	fmt.Fprintln(stdout, "  data root:", cfg.DataRoot)
	if info, err := os.Stat(cfg.DataRoot); err == nil && info.IsDir() {
		fmt.Fprintln(stdout, "  local data: ok")
	} else if errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(stdout, "  local data: not initialized (start the daemon once)")
	} else {
		fmt.Fprintln(stderr, "  local data: error:", err)
		failed = true
	}
	if !cfg.ModelConfigured {
		fmt.Fprintln(stderr, "  model resource: setup required (run `./bin/eri daemon` in a terminal)")
		failed = true
	} else if cfg.ModelProvider == "deepseek" {
		fmt.Fprintln(stdout, "  model provider: DeepSeek")
		fmt.Fprintln(stdout, "  model:", cfg.Model)
		if cfg.DeepSeekKeySet {
			fmt.Fprintln(stdout, "  credential: configured in process environment")
		} else if configured, err := providersecret.NewClient(cfg.ProviderBrokerSocket).Configured(ctx); err == nil && configured {
			fmt.Fprintln(stdout, "  credential: configured in macOS Keychain through Provider Secret Broker")
		} else {
			fmt.Fprintln(stderr, "  credential broker: unavailable or not configured")
			failed = true
		}
	} else {
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, cfg.OllamaURL+"/api/tags", nil)
		response, err := (&http.Client{Timeout: 3 * time.Second}).Do(request)
		if err != nil {
			fmt.Fprintln(stderr, "  Ollama: unavailable:", err)
			failed = true
		} else {
			if response.StatusCode == http.StatusOK {
				fmt.Fprintln(stdout, "  Ollama: reachable")
				var inventory struct {
					Models []struct {
						Name  string `json:"name"`
						Model string `json:"model"`
					} `json:"models"`
				}
				if err := json.NewDecoder(io.LimitReader(response.Body, 2*1024*1024)).Decode(&inventory); err != nil {
					fmt.Fprintln(stderr, "  Ollama models: invalid /api/tags response")
					failed = true
				} else if !ollamaModelPresent(cfg.Model, inventory.Models) {
					fmt.Fprintf(stderr, "  model: %s is not installed (run: ollama pull %s)\n", cfg.Model, cfg.Model)
					failed = true
				} else {
					fmt.Fprintln(stdout, "  model:", cfg.Model, "installed")
				}
			} else {
				fmt.Fprintln(stderr, "  Ollama: HTTP", response.StatusCode)
				failed = true
			}
			response.Body.Close()
		}
	}
	if cfg.ModelConfigured {
		if embeddingModel, err := ollama.DiscoverEmbeddingModel(ctx, cfg.OllamaURL, cfg.MemoryEmbeddingModel); err == nil && embeddingModel != "" {
			fmt.Fprintln(stdout, "  semantic memory:", embeddingModel, "available locally")
		} else {
			fmt.Fprintln(stdout, "  semantic memory: lexical and associative fallback (install a local Ollama embedding model to enable semantic recall)")
		}
	}
	if cfg.TavilyKeySet {
		fmt.Fprintf(stdout, "  Web: Tavily configured (search=%s, extract=%s; reachability not probed)\n", cfg.TavilySearchDepth, cfg.TavilyExtractDepth)
	} else {
		fmt.Fprintln(stdout, "  Web: disabled (set TAVILY_API_KEY to enable Tavily Search and Extract)")
	}
	if cfg.LarkEnabled {
		fmt.Fprintf(stdout, "  Lark channel: configured for %s (authorization and reachability not probed)\n", cfg.LarkBrand)
	} else {
		fmt.Fprintln(stdout, "  Lark channel: disabled")
	}
	if _, err := os.Stat(cfg.SocketPath); err == nil {
		fmt.Fprintln(stdout, "  daemon socket: present")
	} else {
		fmt.Fprintln(stdout, "  daemon socket: absent")
	}
	fmt.Fprintln(stdout, "  daemon log:", filepath.Join(cfg.DataRoot, "logs", "daemon.log"))
	fmt.Fprintln(stdout, "  startup log:", filepath.Join(cfg.DataRoot, "logs", "bootstrap.log"))
	if failed {
		return 1
	}
	return 0
}

func ollamaModelPresent(configured string, models []struct {
	Name  string `json:"name"`
	Model string `json:"model"`
}) bool {
	configured = strings.TrimSpace(configured)
	for _, candidate := range models {
		for _, name := range []string{candidate.Name, candidate.Model} {
			name = strings.TrimSpace(name)
			if name == configured || (name == configured+":latest" && !strings.Contains(configured, ":")) {
				return true
			}
		}
	}
	return false
}

func unixClient(socketPath string) *http.Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "unix", socketPath)
		},
		DisableCompression: true,
	}
	return &http.Client{Transport: transport, Timeout: 10 * time.Minute}
}

func doJSON(ctx context.Context, client *http.Client, method, path string, body []byte, target any) error {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://eri.local"+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: %s", errDaemonUnavailable, observability.SafeError(err))
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var apiError struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		json.NewDecoder(io.LimitReader(response.Body, 64*1024)).Decode(&apiError)
		if apiError.Error.Message == "" {
			apiError.Error.Message = response.Status
		}
		return fmt.Errorf("%s", apiError.Error.Message)
	}
	if target == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(response.Body, 2*1024*1024)).Decode(target)
}

func printClientError(stderr io.Writer, command string, err error) {
	if errors.Is(err, errDaemonUnavailable) {
		fmt.Fprintf(stderr, "%s: Eri is offline. Start it with `eri daemon`, or install the background service with `eri install`.\n", command)
		return
	}
	fmt.Fprintf(stderr, "%s: %s\n", command, observability.SafeError(err))
}

func printHelp(w io.Writer) {
	fmt.Fprintln(w, `Eri — local-first personal Agent Assistant

Usage:
  eri daemon          run the long-lived daemon
  eri install         install or upgrade the per-user macOS launchd service
  eri uninstall       remove the launchd service without deleting user data
  eri chat [--file PATH] [message]
                      chat through the canonical conversation
  eri status          inspect daemon presence
  eri stop            stop the daemon gracefully
  eri doctor          check local dependencies
  eri logs [--follow] [--lines N] [--task-id ID]
                      read redacted structured runtime logs
  eri diagnose [--output PATH]
                      create a redacted diagnostic bundle
  eri approve ID      approve one exact pending action
  eri deny ID         deny one exact pending action`)
}
