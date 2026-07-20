package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	pluginv1 "github.com/z-chenhao/eri/api/plugin/v1"
)

func TestAuthBrokerHTTPRejectsUntrustedPlaintextEndpoint(t *testing.T) {
	t.Parallel()

	if _, _, err := authBrokerHTTP("http://auth.example.com"); err == nil {
		t.Fatal("non-loopback plaintext auth broker endpoint was accepted")
	}
	if _, _, err := authBrokerHTTP("http://localhost:7788"); err == nil {
		t.Fatal("hostname-based plaintext auth broker endpoint was accepted")
	}
	if _, _, err := authBrokerHTTP("file:///tmp/broker.sock"); err == nil {
		t.Fatal("unsupported auth broker endpoint scheme was accepted")
	}
}

func TestIssueCapabilityHandleRejectsCrossOriginRedirect(t *testing.T) {
	targetCalled := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		targetCalled = true
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	t.Setenv("ERI_TEST_AUTH_BROKER", source.URL)
	_, err := issueCapabilityHandle(context.Background(), &AuthSpec{
		Mode: "external_broker", Provider: "google", Scopes: []string{"calendar.readonly"},
		BrokerEndpointEnvironment: "ERI_TEST_AUTH_BROKER", RedemptionEndpointEnvironment: "ERI_TEST_AUTH_REDEMPTION",
	}, capabilityHandleRequest{
		InvocationBinding: pluginv1.InvocationBinding{PluginID: "calendar", TaskID: "task-1", RunID: "run-1", InvocationID: "invocation-1"},
		Provider:          "google", Scopes: []string{"calendar.readonly"}, MaxUses: 1, TTLSeconds: 120,
	})
	if err == nil || targetCalled {
		t.Fatalf("cross-origin broker redirect err=%v target_called=%v", err, targetCalled)
	}
}

func TestIssueCapabilityHandleBindsInvocationAndRejectsCredentialShapedResponse(t *testing.T) {
	var received capabilityHandleRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/capability-handles" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if err := decodeStrictJSON(r.Body, &received); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"handle":"opaque-once","expires_at":%q}`, time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano))
	}))
	defer server.Close()

	t.Setenv("ERI_TEST_AUTH_BROKER", server.URL)
	issued, err := issueCapabilityHandle(context.Background(), &AuthSpec{
		Mode: "external_broker", Provider: "google", Scopes: []string{"calendar.readonly"},
		BrokerEndpointEnvironment: "ERI_TEST_AUTH_BROKER", RedemptionEndpointEnvironment: "ERI_TEST_AUTH_REDEMPTION",
	}, capabilityHandleRequest{
		InvocationBinding: pluginv1.InvocationBinding{PluginID: "calendar", TaskID: "task-1", RunID: "run-1", InvocationID: "invocation-1"},
		Provider:          "google", Scopes: []string{"calendar.readonly"}, MaxUses: 1, TTLSeconds: 120,
	})
	if err != nil {
		t.Fatal(err)
	}
	if issued.Handle != "opaque-once" || received.TaskID != "task-1" || received.RunID != "run-1" || received.InvocationID != "invocation-1" || received.MaxUses != 1 {
		t.Fatalf("issued=%+v request=%+v", issued, received)
	}
}

func TestIssueCapabilityHandleRejectsUnknownCredentialFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"handle":"opaque-once","expires_at":%q,"access_token":"must-not-cross-boundary"}`, time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano))
	}))
	defer server.Close()

	t.Setenv("ERI_TEST_AUTH_BROKER", server.URL)
	_, err := issueCapabilityHandle(context.Background(), &AuthSpec{
		Mode: "external_broker", Provider: "google", Scopes: []string{"calendar.readonly"},
		BrokerEndpointEnvironment: "ERI_TEST_AUTH_BROKER", RedemptionEndpointEnvironment: "ERI_TEST_AUTH_REDEMPTION",
	}, capabilityHandleRequest{InvocationBinding: pluginv1.InvocationBinding{PluginID: "calendar", TaskID: "task-1", RunID: "run-1", InvocationID: "invocation-1"}, MaxUses: 1, TTLSeconds: 120})
	if err == nil {
		t.Fatal("auth broker response containing a provider credential was accepted")
	}
}

func TestIssueCapabilityHandleRejectsExpandedLifetime(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"handle":"opaque-once","expires_at":%q}`, time.Now().UTC().Add(10*time.Minute).Format(time.RFC3339Nano))
	}))
	defer server.Close()

	t.Setenv("ERI_TEST_AUTH_BROKER", server.URL)
	_, err := issueCapabilityHandle(context.Background(), &AuthSpec{
		Mode: "external_broker", Provider: "google", Scopes: []string{"calendar.readonly"},
		BrokerEndpointEnvironment: "ERI_TEST_AUTH_BROKER", RedemptionEndpointEnvironment: "ERI_TEST_AUTH_REDEMPTION",
	}, capabilityHandleRequest{InvocationBinding: pluginv1.InvocationBinding{PluginID: "calendar", TaskID: "task-1", RunID: "run-1", InvocationID: "invocation-1"}, MaxUses: 1, TTLSeconds: 120})
	if err == nil {
		t.Fatal("auth broker expanded the requested capability lifetime")
	}
}

func TestIssueCapabilityHandleRejectsBindingOutsideManifest(t *testing.T) {
	t.Setenv("ERI_TEST_AUTH_BROKER", "http://127.0.0.1:1")
	auth := &AuthSpec{
		Mode: "external_broker", Provider: "google", Scopes: []string{"calendar.readonly"},
		BrokerEndpointEnvironment: "ERI_TEST_AUTH_BROKER", RedemptionEndpointEnvironment: "ERI_TEST_AUTH_REDEMPTION",
	}
	_, err := issueCapabilityHandle(context.Background(), auth, capabilityHandleRequest{
		InvocationBinding: pluginv1.InvocationBinding{PluginID: "calendar", TaskID: "task-1", RunID: "run-1", InvocationID: "invocation-1"},
		Provider:          "google", Scopes: []string{"calendar.events"}, MaxUses: 1, TTLSeconds: 120,
	})
	if err == nil {
		t.Fatal("capability request expanded scopes beyond the plugin manifest")
	}
}

func TestIssueCapabilityHandleRejectsTrailingResponseData(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"handle":"opaque-once","expires_at":%q}{}`, time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano))
	}))
	defer server.Close()

	t.Setenv("ERI_TEST_AUTH_BROKER", server.URL)
	_, err := issueCapabilityHandle(context.Background(), &AuthSpec{
		Mode: "external_broker", Provider: "google", Scopes: []string{"calendar.readonly"},
		BrokerEndpointEnvironment: "ERI_TEST_AUTH_BROKER", RedemptionEndpointEnvironment: "ERI_TEST_AUTH_REDEMPTION",
	}, capabilityHandleRequest{
		InvocationBinding: pluginv1.InvocationBinding{PluginID: "calendar", TaskID: "task-1", RunID: "run-1", InvocationID: "invocation-1"},
		Provider:          "google", Scopes: []string{"calendar.readonly"}, MaxUses: 1, TTLSeconds: 120,
	})
	if err == nil {
		t.Fatal("auth broker response with trailing JSON data was accepted")
	}
}

func decodeStrictJSON(body interface{ Read([]byte) (int, error) }, target any) error {
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}
