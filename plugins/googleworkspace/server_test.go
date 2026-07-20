package googleworkspace

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	protocol "github.com/modelcontextprotocol/go-sdk/mcp"
	pluginv1 "github.com/z-chenhao/eri/api/plugin/v1"
)

func TestCalendarAndGmailUseToolScopedOneUseBrokerCapabilities(t *testing.T) {
	accessToken := "test-access-token-that-never-enters-eri-core"
	var redemptions []pluginv1.RedemptionRequest
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != pluginv1.CapabilityRedeemPath || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var request pluginv1.RedemptionRequest
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		redemptions = append(redemptions, request)
		json.NewEncoder(w).Encode(pluginv1.RedemptionResponse{
			Provider: "google", Scopes: request.Scopes, TokenType: "Bearer",
			AccessToken: accessToken, ExpiresAt: time.Now().UTC().Add(time.Hour),
		})
	}))
	defer broker.Close()

	googleAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+accessToken {
			http.Error(w, "missing authorization", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/calendar/v3/calendars/primary/events"):
			if r.URL.Query().Get("timeMin") == "" || r.URL.Query().Get("orderBy") != "startTime" {
				http.Error(w, "calendar query missing", http.StatusBadRequest)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"etag": `"calendar-etag"`, "items": []map[string]any{{
					"id": "event-1", "status": "confirmed", "summary": "Planning",
					"start": map[string]string{"dateTime": "2026-08-10T09:00:00+08:00"},
					"end":   map[string]string{"dateTime": "2026-08-10T10:00:00+08:00"},
				}},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/gmail/v1/users/me/messages/send":
			var payload struct {
				Raw string `json:"raw"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				http.Error(w, "invalid Gmail payload", http.StatusBadRequest)
				return
			}
			raw, err := base64.RawURLEncoding.DecodeString(payload.Raw)
			if err != nil || !strings.Contains(string(raw), "To: alice@example.com") || !strings.Contains(string(raw), "Subject: Confirmed plan") {
				http.Error(w, "invalid MIME message", http.StatusBadRequest)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"id": "gmail-message-1", "threadId": "gmail-thread-1"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer googleAPI.Close()

	server, err := New(Options{
		BrokerEndpoint: broker.URL, Provider: "google", AllowedScopes: allScopes,
		HTTPClient: googleAPI.Client(), CalendarBase: googleAPI.URL, GmailBase: googleAPI.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	calendarRequest := invocationRequest(calendarReadScope, "calendar-handle", time.Now().UTC().Add(time.Minute))
	calendarResult, calendarOutput, err := server.calendarList(context.Background(), calendarRequest, calendarListInput{
		TimeMin: "2026-08-10T00:00:00+08:00", TimeMax: "2026-08-12T00:00:00+08:00",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(calendarOutput.Events) != 1 || calendarOutput.Events[0].ID != "event-1" || !strings.Contains(calendarOutput.Receipt, "calendar-etag") {
		t.Fatalf("calendar output = %+v", calendarOutput)
	}
	assertProviderMetadata(t, calendarResult, calendarOutput.Receipt, "")

	gmailRequest := invocationRequest(gmailSendScope, "gmail-handle", time.Now().UTC().Add(time.Minute))
	gmailResult, gmailOutput, err := server.gmailSend(context.Background(), gmailRequest, gmailSendInput{
		To: []string{"alice@example.com"}, Subject: "Confirmed plan", Body: "The plan is approved.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if gmailOutput.MessageID != "gmail-message-1" || gmailOutput.Receipt != "gmail:message:gmail-message-1" {
		t.Fatalf("Gmail output = %+v", gmailOutput)
	}
	assertProviderMetadata(t, gmailResult, gmailOutput.Receipt, "gmail-message-1")

	if len(redemptions) != 2 || !sameScopeSet(redemptions[0].Scopes, []string{calendarReadScope}) || !sameScopeSet(redemptions[1].Scopes, []string{gmailSendScope}) {
		t.Fatalf("broker redemptions = %+v", redemptions)
	}
	for _, redemption := range redemptions {
		if redemption.PluginID != pluginID || redemption.TaskID != "task-1" || redemption.RunID != "run-1" || redemption.InvocationID != "intent-1" {
			t.Fatalf("unbound redemption = %+v", redemption)
		}
	}
	encodedCalendar, _ := json.Marshal(calendarOutput)
	encodedGmail, _ := json.Marshal(gmailOutput)
	if strings.Contains(string(encodedCalendar), accessToken) || strings.Contains(string(encodedGmail), accessToken) {
		t.Fatal("provider token escaped into MCP output")
	}
}

func TestBrokerRejectsScopeConfusionAndCredentialShapedResponse(t *testing.T) {
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"provider":"google","scopes":[%q],"token_type":"Bearer","access_token":"short-lived","expires_at":%q,"refresh_token":"forbidden"}`,
			calendarReadScope, time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano))
	}))
	defer broker.Close()
	client, err := newBrokerClient(broker.URL, "google", pluginID, allScopes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.redeem(context.Background(), invocationRequest(calendarReadScope, "handle", time.Now().UTC().Add(time.Minute)), gmailSendScope); err == nil {
		t.Fatal("a Calendar capability was accepted for Gmail send")
	}
	if _, err := client.redeem(context.Background(), invocationRequest(calendarReadScope, "handle", time.Now().UTC().Add(time.Minute)), calendarReadScope); err == nil {
		t.Fatal("broker response containing a refresh token was accepted")
	}
}

func TestAuthorizationToolsExposeConsentURLWithoutProviderCredential(t *testing.T) {
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == pluginv1.AuthorizationStatusPath:
			json.NewEncoder(w).Encode(pluginv1.AuthorizationStatus{
				Provider: "google", Authorized: false, MissingScopes: []string{calendarReadScope},
				CredentialSource: "external_os_credential_store",
			})
		case r.Method == http.MethodPost && r.URL.Path == pluginv1.AuthorizationStartPath:
			var request pluginv1.AuthorizationStartRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || !sameScopeSet(request.Scopes, []string{calendarReadScope}) {
				http.Error(w, "invalid start", http.StatusBadRequest)
				return
			}
			json.NewEncoder(w).Encode(pluginv1.AuthorizationStartResponse{
				Provider: "google", AuthorizationURL: "https://accounts.google.com/o/oauth2/v2/auth?state=opaque",
				ExpiresAt: time.Now().UTC().Add(10 * time.Minute),
			})
		case r.Method == http.MethodDelete && r.URL.Path == pluginv1.AuthorizationRevokePath:
			json.NewEncoder(w).Encode(pluginv1.AuthorizationRevokeResponse{
				Provider: "google", Revoked: true, RevokedAt: time.Now().UTC(), Receipt: "google-oauth:revoked",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer broker.Close()
	server, err := New(Options{BrokerEndpoint: broker.URL, Provider: "google", AllowedScopes: allScopes})
	if err != nil {
		t.Fatal(err)
	}
	_, status, err := server.authorizationStatus(context.Background(), nil, authorizationInput{Capabilities: []string{"calendar_read"}})
	if err != nil || status.Authorized || len(status.Missing) != 1 || status.Missing[0] != "calendar_read" {
		t.Fatalf("authorization status=%+v err=%v", status, err)
	}
	_, started, err := server.authorizationStart(context.Background(), nil, authorizationInput{Capabilities: []string{"calendar_read"}})
	if err != nil || !strings.HasPrefix(started.AuthorizationURL, "https://accounts.google.com/") || len(started.Capabilities) != 1 {
		t.Fatalf("authorization start=%+v err=%v", started, err)
	}
	if _, _, err := server.authorizationStart(context.Background(), nil, authorizationInput{Capabilities: []string{"all_google_data"}}); err == nil {
		t.Fatal("unknown broad Google capability was accepted")
	}
	revokeResult, revoked, err := server.authorizationRevoke(context.Background(), nil, struct{}{})
	if err != nil || !revoked.Revoked || revoked.Receipt != "google-oauth:revoked" {
		t.Fatalf("authorization revoke=%+v err=%v", revoked, err)
	}
	assertProviderMetadata(t, revokeResult, revoked.Receipt, "")
}

func TestCalendarListRequiresOrderedBoundedWindowAndValidTimeZone(t *testing.T) {
	server := &Server{}
	request := invocationRequest(calendarReadScope, "handle", time.Now().UTC().Add(time.Minute))
	for _, input := range []calendarListInput{
		{TimeMin: "2026-08-10T00:00:00Z", TimeMax: "2026-08-09T00:00:00Z"},
		{TimeMin: "2026-01-01T00:00:00Z", TimeMax: "2028-01-01T00:00:00Z"},
		{TimeMin: "2026-08-10T00:00:00Z", TimeMax: "2026-08-11T00:00:00Z", TimeZone: "Mars/Olympus"},
	} {
		if _, _, err := server.calendarList(context.Background(), request, input); err == nil {
			t.Fatalf("invalid Calendar window accepted: %+v", input)
		}
	}
}

func TestEmailHeaderInjectionIsRejected(t *testing.T) {
	if _, err := buildPlainTextMessage(gmailSendInput{To: []string{"alice@example.com"}, Subject: "Hello\r\nBcc: attacker@example.com", Body: "body"}); err == nil {
		t.Fatal("email header injection was accepted")
	}
}

func TestPlainTextMessageNormalizesLineEndingsOnce(t *testing.T) {
	message, err := buildPlainTextMessage(gmailSendInput{To: []string{"alice@example.com"}, Subject: "Hello", Body: "first\r\nsecond\nthird\rfourth"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(message), "\r\r\n") || !strings.Contains(string(message), "first\r\nsecond\r\nthird\r\nfourth") {
		t.Fatalf("normalized MIME body = %q", message)
	}
}

func invocationRequest(scope, handle string, expiresAt time.Time) *protocol.CallToolRequest {
	return &protocol.CallToolRequest{Params: &protocol.CallToolParamsRaw{Meta: protocol.Meta{
		"eri": map[string]any{
			"task_id": "task-1", "run_id": "run-1", "invocation_id": "intent-1",
			"auth": map[string]any{
				"mode": "external_broker", "provider": "google", "scopes": []string{scope},
				"capability_handle": handle, "expires_at": expiresAt,
			},
		},
	}}}
}

func assertProviderMetadata(t *testing.T, result *protocol.CallToolResult, receipt, objectID string) {
	t.Helper()
	body, err := json.Marshal(result.Meta[pluginv1.ResultMetadataKey])
	if err != nil {
		t.Fatal(err)
	}
	var metadata pluginv1.ResultMetadata
	if err := json.Unmarshal(body, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.Receipt != receipt || metadata.ExternalObjectID != objectID || metadata.FreshAt.IsZero() {
		t.Fatalf("provider metadata = %+v", metadata)
	}
}
