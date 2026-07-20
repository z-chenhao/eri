package googleworkspace

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	protocol "github.com/modelcontextprotocol/go-sdk/mcp"
	pluginv1 "github.com/z-chenhao/eri/api/plugin/v1"
	pluginmcp "github.com/z-chenhao/eri/internal/plugin/mcp"
	"github.com/z-chenhao/eri/internal/policy"
)

func TestGoogleWorkspaceMCPHelper(t *testing.T) {
	if os.Getenv("ERI_GOOGLE_PLUGIN_HELPER") != "1" {
		return
	}
	var scopes []string
	if err := json.Unmarshal([]byte(os.Getenv("ERI_AUTH_SCOPES_JSON")), &scopes); err != nil {
		os.Exit(2)
	}
	server, err := New(Options{
		BrokerEndpoint: os.Getenv("ERI_AUTH_BROKER_ENDPOINT"), Provider: os.Getenv("ERI_AUTH_PROVIDER"),
		AllowedScopes: scopes, CalendarBase: os.Getenv("ERI_GOOGLE_TEST_API"), GmailBase: os.Getenv("ERI_GOOGLE_TEST_API"),
	})
	if err != nil {
		os.Exit(2)
	}
	_ = server.MCP().Run(context.Background(), &protocol.StdioTransport{})
	os.Exit(0)
}

func TestCoreHostToGooglePluginUsesBrokerAndPreservesProviderReceipt(t *testing.T) {
	var mu sync.Mutex
	issuedScopes := []string{}
	handle := "one-use-google-handle"
	issuerBroker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == pluginv1.CapabilityIssuePath {
			var request pluginv1.CapabilityHandleRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				http.Error(w, "bad issue", http.StatusBadRequest)
				return
			}
			mu.Lock()
			issuedScopes = append([]string(nil), request.Scopes...)
			mu.Unlock()
			json.NewEncoder(w).Encode(pluginv1.CapabilityHandleResponse{Handle: handle, ExpiresAt: time.Now().UTC().Add(time.Minute)})
			return
		}
		http.NotFound(w, r)
	}))
	defer issuerBroker.Close()
	pluginBroker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == pluginv1.CapabilityRedeemPath:
			var request pluginv1.RedemptionRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Handle != handle || request.InvocationID != "intent-1" {
				http.Error(w, "bad redemption", http.StatusForbidden)
				return
			}
			json.NewEncoder(w).Encode(pluginv1.RedemptionResponse{
				Provider: "google", Scopes: request.Scopes, TokenType: "Bearer",
				AccessToken: "subprocess-short-access-token", ExpiresAt: time.Now().UTC().Add(time.Hour),
			})
		case r.Method == http.MethodGet && r.URL.Path == pluginv1.AuthorizationStatusPath:
			json.NewEncoder(w).Encode(pluginv1.AuthorizationStatus{
				Provider: "google", Authorized: true, GrantedScopes: r.URL.Query()["scope"],
				CredentialSource: "external_os_credential_store",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer pluginBroker.Close()
	googleAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer subprocess-short-access-token" || !strings.Contains(r.URL.Path, "/calendar/v3/calendars/primary/events") {
			http.Error(w, "bad Google request", http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"etag": `"subprocess-etag"`, "items": []map[string]any{{
				"id": "subprocess-event", "status": "confirmed", "summary": "Verified",
				"start": map[string]string{"dateTime": "2026-08-10T09:00:00+08:00"},
				"end":   map[string]string{"dateTime": "2026-08-10T10:00:00+08:00"},
			}},
		})
	}))
	defer googleAPI.Close()

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ERI_GOOGLE_PLUGIN_HELPER", "1")
	t.Setenv("ERI_GOOGLE_TEST_API", googleAPI.URL)
	t.Setenv("ERI_GOOGLE_AUTH_BROKER", issuerBroker.URL)
	t.Setenv("ERI_GOOGLE_AUTH_REDEMPTION_BROKER", pluginBroker.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	host, err := pluginmcp.OpenHost(ctx, []pluginmcp.ServerSpec{{
		ID: "google-workspace", Version: "1.0.0", Command: executable, Arguments: []string{"-test.run=TestGoogleWorkspaceMCPHelper"},
		Environment: map[string]string{"ERI_GOOGLE_PLUGIN_HELPER": "1", "ERI_GOOGLE_TEST_API": googleAPI.URL}, DefaultEffect: policy.Privileged,
		ToolEffects: map[string]policy.EffectClass{"calendar_list_events": policy.ReadOnly}, External: true,
		Auth: &pluginmcp.AuthSpec{
			Mode: "external_broker", Provider: "google", Scopes: append([]string(nil), allScopes...),
			ToolScopes:  map[string][]string{"calendar_list_events": {calendarReadScope}},
			PublicTools: []string{"authorization_status", "authorization_start", "authorization_disconnect"}, BrokerEndpointEnvironment: "ERI_GOOGLE_AUTH_BROKER",
			RedemptionEndpointEnvironment: "ERI_GOOGLE_AUTH_REDEMPTION_BROKER",
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	tools := host.Tools()
	if len(tools) != 8 {
		t.Fatalf("discovered %d Google tools", len(tools))
	}
	var calendar, status = tools[0], tools[0]
	for _, candidate := range tools {
		switch candidate.Descriptor().ID {
		case "mcp.google-workspace.calendar_list_events":
			calendar = candidate
		case "mcp.google-workspace.authorization_status":
			status = candidate
		}
	}
	prepared, err := calendar.Prepare(ctx, json.RawMessage(`{"time_min":"2026-08-10T00:00:00+08:00","time_max":"2026-08-11T00:00:00+08:00"}`))
	if err != nil {
		t.Fatal(err)
	}
	prepared.TaskID, prepared.RunID, prepared.InvocationID = "task-1", "run-1", "intent-1"
	result, err := calendar.Execute(ctx, prepared)
	if err != nil {
		t.Fatal(err)
	}
	if result.Receipt != "google-calendar:list:subprocess-etag" || strings.Contains(string(result.Output), "subprocess-short-access-token") || strings.Contains(string(result.Output), handle) {
		t.Fatalf("calendar result = %+v", result)
	}
	mu.Lock()
	scopes := append([]string(nil), issuedScopes...)
	mu.Unlock()
	if !sameScopeSet(scopes, []string{calendarReadScope}) {
		t.Fatalf("Core requested scopes = %v", scopes)
	}
	statusPrepared, err := status.Prepare(ctx, json.RawMessage(`{"capabilities":["calendar_read"]}`))
	if err != nil {
		t.Fatal(err)
	}
	statusResult, err := status.Execute(ctx, statusPrepared)
	if err != nil || !strings.Contains(string(statusResult.Output), `"authorized":true`) {
		t.Fatalf("public authorization status result=%s err=%v", statusResult.Output, err)
	}
	mu.Lock()
	if len(issuedScopes) != 1 {
		t.Fatalf("public status unexpectedly requested a provider capability: %v", issuedScopes)
	}
	mu.Unlock()
}
