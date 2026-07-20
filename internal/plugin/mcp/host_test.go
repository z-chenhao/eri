package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	protocol "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/z-chenhao/eri/internal/policy"
	"github.com/z-chenhao/eri/internal/tool"
)

type helperInput struct {
	Value string `json:"value" jsonschema:"value to echo"`
}

type helperOutput struct {
	Echo          string `json:"echo"`
	SecretVisible bool   `json:"secret_visible"`
}

func TestMCPHelperProcess(t *testing.T) {
	if os.Getenv("ERI_MCP_TEST_HELPER") != "1" {
		return
	}
	fmt.Fprintln(os.Stderr, "plugin helper ready token=super-secret-diagnostic-value")
	server := protocol.NewServer(&protocol.Implementation{Name: "eri-test-server", Version: "1.0.0"}, nil)
	protocol.AddTool(server, &protocol.Tool{Name: "echo", Description: "Echo one value from a real MCP subprocess."},
		func(_ context.Context, _ *protocol.CallToolRequest, input helperInput) (*protocol.CallToolResult, helperOutput, error) {
			return nil, helperOutput{Echo: input.Value, SecretVisible: os.Getenv("DEEPSEEK_API_KEY") != ""}, nil
		})
	_ = server.Run(context.Background(), &protocol.StdioTransport{})
	os.Exit(0)
}

func TestHostDiscoversAndInvokesRealMCPServerWithoutSecretInheritance(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ERI_MCP_TEST_HELPER", "1")
	t.Setenv("DEEPSEEK_API_KEY", "must-not-enter-plugin")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	host, err := OpenHost(ctx, []ServerSpec{{
		ID: "contract", Command: executable, Arguments: []string{"-test.run=TestMCPHelperProcess"},
		Environment: map[string]string{"ERI_MCP_TEST_HELPER": "1"}, DefaultEffect: policy.ReadOnly,
	}}, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	tools := host.Tools()
	if len(tools) != 1 || tools[0].Descriptor().ID != "mcp.contract.echo" || tools[0].Descriptor().Source != tool.Plugin {
		t.Fatalf("discovered tools = %+v", tools)
	}
	prepared, err := tools[0].Prepare(ctx, json.RawMessage(`{"value":"hello-mcp"}`))
	if err != nil {
		t.Fatal(err)
	}
	result, err := tools[0].Execute(ctx, prepared)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		StructuredContent map[string]any `json:"structuredContent"`
	}
	if err := json.Unmarshal(result.Output, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.StructuredContent["echo"] != "hello-mcp" || wire.StructuredContent["secret_visible"] != false {
		t.Fatalf("MCP result = %s", result.Output)
	}
	if !strings.Contains(logs.String(), "plugin_id=contract") || strings.Contains(logs.String(), "super-secret-diagnostic-value") {
		t.Fatalf("plugin stderr was not safely logged: %s", logs.String())
	}
}

func TestParseSpecsDefaultsUntrustedToolsToPrivileged(t *testing.T) {
	specs, err := ParseSpecs(`[{"id":"calendar","command":"calendar-mcp"}]`)
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 1 || specs[0].DefaultEffect != policy.Privileged {
		t.Fatalf("specs = %+v", specs)
	}
}

func TestResolvePackagedRuntimeFindsExecutableNextToEri(t *testing.T) {
	directory := t.TempDir()
	eri := filepath.Join(directory, "eri")
	plugin := filepath.Join(directory, "eri-google-workspace")
	if err := os.WriteFile(plugin, []byte("test executable"), 0o700); err != nil {
		t.Fatal(err)
	}
	resolved, err := resolvePackagedRuntime("eri-google-workspace", eri)
	if err != nil || resolved != plugin {
		t.Fatalf("resolved=%q err=%v", resolved, err)
	}
	if err := os.Chmod(plugin, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolvePackagedRuntime("eri-google-workspace", eri); err == nil {
		t.Fatal("non-executable packaged runtime was accepted")
	}
}

func TestMCPResultCannotReturnCapabilityHandleOrCredential(t *testing.T) {
	t.Parallel()

	if err := validateMCPResult([]byte(`{"echo":"opaque-once"}`), "opaque-once"); err == nil {
		t.Fatal("plugin capability handle was allowed into a tool result")
	}
	if err := validateMCPResult([]byte(`{"password":"correct-horse-battery-staple"}`), ""); err == nil {
		t.Fatal("credential-shaped plugin result was allowed into model context")
	}
	if err := validateMCPResult([]byte(`{"event_id":"evt_123","status":"confirmed"}`), "opaque-once"); err != nil {
		t.Fatalf("safe plugin result rejected: %v", err)
	}
}

func TestAuthForToolNarrowsCapabilityAndProviderReceiptIsPreserved(t *testing.T) {
	read := "https://www.googleapis.com/auth/calendar.events.readonly"
	write := "https://www.googleapis.com/auth/calendar.events"
	auth := &AuthSpec{
		Mode: "external_broker", Provider: "google", Scopes: []string{read, write},
		ToolScopes: map[string][]string{"list_events": {read}}, BrokerEndpointEnvironment: "ERI_GOOGLE_AUTH_BROKER",
		RedemptionEndpointEnvironment: "ERI_GOOGLE_AUTH_REDEMPTION_BROKER",
	}
	narrowed := authForTool(auth, "list_events")
	if len(narrowed.Scopes) != 1 || narrowed.Scopes[0] != read || len(auth.Scopes) != 2 {
		t.Fatalf("narrowed=%+v original=%+v", narrowed, auth)
	}
	metadata, err := resultMetadata(protocol.Meta{"eri": map[string]any{
		"receipt": "google-calendar:event:event-123", "external_object_id": "event-123",
	}})
	if err != nil || metadata.Receipt != "google-calendar:event:event-123" || metadata.ExternalObjectID != "event-123" {
		t.Fatalf("metadata=%+v err=%v", metadata, err)
	}
}

func TestMCPEnvironmentExposesOnlyRedemptionEndpointToPlugin(t *testing.T) {
	t.Setenv("ERI_TEST_ISSUER", "unix:///private/tmp/issuer.sock")
	t.Setenv("ERI_TEST_REDEMPTION", "unix:///private/tmp/redemption.sock")
	auth := &AuthSpec{
		Mode: "external_broker", Provider: "google", Scopes: []string{"calendar.readonly"},
		BrokerEndpointEnvironment: "ERI_TEST_ISSUER", RedemptionEndpointEnvironment: "ERI_TEST_REDEMPTION",
	}
	environment, err := mcpEnvironment(nil, auth)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(environment, "\n")
	if strings.Contains(joined, "issuer.sock") || !strings.Contains(joined, "ERI_AUTH_BROKER_ENDPOINT=unix:///private/tmp/redemption.sock") {
		t.Fatalf("plugin environment = %v", environment)
	}
	if _, err := mcpEnvironment(map[string]string{"ERI_TEST_ISSUER": "unix:///private/tmp/issuer.sock"}, auth); err == nil {
		t.Fatal("plugin was allowed to inherit the Core issuer endpoint")
	}
}

func TestMCPEnvironmentRejectsCredentialConfiguration(t *testing.T) {
	for name, value := range map[string]string{
		"DEEPSEEK_API_KEY": "ordinary-value",
		"PLUGIN_CONFIG":    "Bearer " + strings.Repeat("a", 24),
	} {
		if _, err := mcpEnvironment(map[string]string{name: value}, nil); err == nil {
			t.Fatalf("credential-shaped environment %s was accepted", name)
		}
	}
}

func TestPluginStderrIsRateLimitedAndNeverLoggedVerbatim(t *testing.T) {
	var logs bytes.Buffer
	writer := &pluginLogWriter{logger: slog.New(slog.NewTextHandler(&logs, nil)), pluginID: "noisy"}
	for index := 0; index < 25; index++ {
		if _, err := writer.Write([]byte("private-plugin-diagnostic")); err != nil {
			t.Fatal(err)
		}
	}
	if count := strings.Count(logs.String(), "plugin emitted stderr"); count != 10 {
		t.Fatalf("logged stderr records = %d, want 10", count)
	}
	if strings.Contains(logs.String(), "private-plugin-diagnostic") {
		t.Fatalf("plugin stderr was logged verbatim: %s", logs.String())
	}
}
