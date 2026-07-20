package googleauth

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	pluginv1 "github.com/z-chenhao/eri/api/plugin/v1"
)

const (
	testReadScope = "https://www.googleapis.com/auth/calendar.events.readonly"
	testSendScope = "https://www.googleapis.com/auth/gmail.send"
)

type memoryGrantStore struct {
	mu    sync.Mutex
	grant Grant
	found bool
}

func (s *memoryGrantStore) Load(context.Context) (Grant, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	copy := s.grant
	copy.Scopes = append([]string(nil), s.grant.Scopes...)
	return copy, s.found, nil
}

func (s *memoryGrantStore) Save(_ context.Context, grant Grant) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.grant = grant
	s.grant.Scopes = append([]string(nil), grant.Scopes...)
	s.found = true
	return nil
}

func (s *memoryGrantStore) Delete(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.grant = Grant{}
	s.found = false
	return nil
}

func TestOfflineAuthorizationIssuesAndConsumesOneUseCapability(t *testing.T) {
	var tokenRequests []url.Values
	var revokedToken string
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 64*1024))
		values, _ := url.ParseQuery(string(body))
		if r.URL.Path == "/revoke" {
			revokedToken = values.Get("token")
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path != "/token" {
			http.NotFound(w, r)
			return
		}
		tokenRequests = append(tokenRequests, values)
		w.Header().Set("Content-Type", "application/json")
		if values.Get("grant_type") == "authorization_code" {
			json.NewEncoder(w).Encode(map[string]any{
				"access_token": "first-short-access-token", "refresh_token": "long-refresh-token-kept-outside-eri",
				"expires_in": 3600, "scope": testReadScope + " " + testSendScope, "token_type": "Bearer",
			})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "second-short-access-token", "expires_in": 3600, "token_type": "Bearer",
		})
	}))
	defer tokenServer.Close()

	store := &memoryGrantStore{}
	broker, err := New(Options{
		OAuth: OAuthClient{
			ClientID: "desktop-client", ClientSecret: "desktop-secret", AuthURI: tokenServer.URL + "/auth",
			TokenURI: tokenServer.URL + "/token", RevokeURI: tokenServer.URL + "/revoke", RedirectURI: "http://127.0.0.1/callback",
		},
		Store: store, AllowedScopes: []string{testReadScope, testSendScope}, HTTPClient: tokenServer.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(broker.Handler())
	defer server.Close()
	broker.oauth.RedirectURI = server.URL + "/oauth/google/callback"

	status := getStatus(t, server.URL, []string{testReadScope})
	if status.Authorized || len(status.MissingScopes) != 1 {
		t.Fatalf("initial status = %+v", status)
	}
	started := postJSON[pluginv1.AuthorizationStartResponse](t, server.URL+pluginv1.AuthorizationStartPath, pluginv1.AuthorizationStartRequest{
		Provider: googleProvider, Scopes: []string{testReadScope, testSendScope},
	}, http.StatusCreated)
	authorizationURL, err := url.Parse(started.AuthorizationURL)
	if err != nil || authorizationURL.Query().Get("access_type") != "offline" || authorizationURL.Query().Get("code_challenge_method") != "S256" || authorizationURL.Query().Get("prompt") != "consent" {
		t.Fatalf("authorization URL = %s err=%v", started.AuthorizationURL, err)
	}
	state := authorizationURL.Query().Get("state")
	callback, err := http.Get(server.URL + "/oauth/google/callback?state=" + url.QueryEscape(state) + "&code=approved-code")
	if err != nil {
		t.Fatal(err)
	}
	callbackBody, _ := io.ReadAll(callback.Body)
	callback.Body.Close()
	if callback.StatusCode != http.StatusOK || !strings.Contains(string(callbackBody), "authorization is complete") {
		t.Fatalf("callback status=%d body=%s", callback.StatusCode, callbackBody)
	}
	if len(tokenRequests) != 1 || tokenRequests[0].Get("client_secret") != "desktop-secret" || tokenRequests[0].Get("code_verifier") == "" || tokenRequests[0].Get("grant_type") != "authorization_code" {
		t.Fatalf("authorization token request = %+v", tokenRequests)
	}
	status = getStatus(t, server.URL, []string{testReadScope})
	if !status.Authorized || status.CredentialSource != "external_os_credential_store" {
		t.Fatalf("authorized status = %+v", status)
	}

	issue := pluginv1.CapabilityHandleRequest{
		InvocationBinding: pluginv1.InvocationBinding{PluginID: "google-workspace", TaskID: "task", RunID: "run", InvocationID: "intent"},
		Provider:          googleProvider, Scopes: []string{testReadScope}, MaxUses: 1, TTLSeconds: 120,
	}
	issued := postJSON[pluginv1.CapabilityHandleResponse](t, server.URL+pluginv1.CapabilityIssuePath, issue, http.StatusCreated)
	if issued.Handle == "" || !issued.ExpiresAt.After(time.Now().UTC()) {
		t.Fatalf("issued = %+v", issued)
	}
	encodedIssue, _ := json.Marshal(issued)
	if bytes.Contains(encodedIssue, []byte("refresh")) || bytes.Contains(encodedIssue, []byte("access")) {
		t.Fatal("provider credential crossed the capability issue boundary")
	}
	redeem := pluginv1.RedemptionRequest{InvocationBinding: issue.InvocationBinding, Provider: googleProvider, Scopes: issue.Scopes, Handle: issued.Handle}
	token := postJSON[pluginv1.RedemptionResponse](t, server.URL+pluginv1.CapabilityRedeemPath, redeem, http.StatusOK)
	if token.AccessToken != "second-short-access-token" || token.Provider != googleProvider || !sameScopes(token.Scopes, []string{testReadScope}) {
		t.Fatalf("redemption token = %+v", token)
	}
	if len(tokenRequests) != 2 || tokenRequests[1].Get("refresh_token") != "long-refresh-token-kept-outside-eri" || tokenRequests[1].Get("grant_type") != "refresh_token" {
		t.Fatalf("refresh token request = %+v", tokenRequests)
	}
	postJSON[map[string]any](t, server.URL+pluginv1.CapabilityRedeemPath, redeem, http.StatusForbidden)

	issuedBeforeRevoke := postJSON[pluginv1.CapabilityHandleResponse](t, server.URL+pluginv1.CapabilityIssuePath, issue, http.StatusCreated)
	revoked := deleteJSON[pluginv1.AuthorizationRevokeResponse](t, server.URL+pluginv1.AuthorizationRevokePath, http.StatusOK)
	if !revoked.Revoked || revoked.Provider != googleProvider || revoked.Receipt != "google-oauth:revoked" || revokedToken != "long-refresh-token-kept-outside-eri" {
		t.Fatalf("revocation=%+v token=%q", revoked, revokedToken)
	}
	status = getStatus(t, server.URL, []string{testReadScope})
	if status.Authorized || len(status.MissingScopes) != 1 {
		t.Fatalf("status after revocation = %+v", status)
	}
	postJSON[map[string]any](t, server.URL+pluginv1.CapabilityRedeemPath, pluginv1.RedemptionRequest{
		InvocationBinding: issue.InvocationBinding, Provider: googleProvider, Scopes: issue.Scopes, Handle: issuedBeforeRevoke.Handle,
	}, http.StatusForbidden)
}

func TestOAuthClientDoesNotForwardSecretsAcrossRedirects(t *testing.T) {
	targetCalled := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		targetCalled = true
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	broker, err := New(Options{
		OAuth: OAuthClient{ClientID: "id", ClientSecret: "secret", AuthURI: source.URL + "/auth", TokenURI: source.URL + "/token", RedirectURI: "http://127.0.0.1/callback"},
		Store: &memoryGrantStore{}, AllowedScopes: []string{testReadScope}, HTTPClient: source.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = broker.requestToken(context.Background(), url.Values{"client_secret": {"must-not-forward"}})
	if err == nil || targetCalled {
		t.Fatalf("OAuth redirect err=%v target_called=%v", err, targetCalled)
	}
}

func TestCapabilityCannotExpandScopesOrMoveBetweenInvocations(t *testing.T) {
	store := &memoryGrantStore{grant: Grant{RefreshToken: "refresh", Scopes: []string{testReadScope}, AuthorizedAt: time.Now().UTC()}, found: true}
	broker, err := New(Options{
		OAuth: OAuthClient{ClientID: "id", ClientSecret: "secret", AuthURI: "https://accounts.google.com/o/oauth2/v2/auth", TokenURI: "https://oauth2.googleapis.com/token", RedirectURI: "http://127.0.0.1/callback"},
		Store: store, AllowedScopes: []string{testReadScope, testSendScope},
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(broker.Handler())
	defer server.Close()
	base := pluginv1.CapabilityHandleRequest{
		InvocationBinding: pluginv1.InvocationBinding{PluginID: "google-workspace", TaskID: "task", RunID: "run", InvocationID: "intent"},
		Provider:          googleProvider, Scopes: []string{testReadScope}, MaxUses: 1, TTLSeconds: 120,
	}
	postJSON[map[string]any](t, server.URL+pluginv1.CapabilityIssuePath, pluginv1.CapabilityHandleRequest{
		InvocationBinding: base.InvocationBinding, Provider: googleProvider, Scopes: []string{testSendScope}, MaxUses: 1, TTLSeconds: 120,
	}, http.StatusConflict)
	issued := postJSON[pluginv1.CapabilityHandleResponse](t, server.URL+pluginv1.CapabilityIssuePath, base, http.StatusCreated)
	moved := pluginv1.RedemptionRequest{InvocationBinding: base.InvocationBinding, Provider: googleProvider, Scopes: base.Scopes, Handle: issued.Handle}
	moved.InvocationID = "other-intent"
	postJSON[map[string]any](t, server.URL+pluginv1.CapabilityRedeemPath, moved, http.StatusForbidden)
	// A failed confused redemption consumes the bearer handle, preventing a
	// later replay after an attacker has learned whether it was valid.
	valid := pluginv1.RedemptionRequest{InvocationBinding: base.InvocationBinding, Provider: googleProvider, Scopes: base.Scopes, Handle: issued.Handle}
	postJSON[map[string]any](t, server.URL+pluginv1.CapabilityRedeemPath, valid, http.StatusForbidden)
}

func TestIssuerAndPluginHandlersDoNotShareCapabilityRoutes(t *testing.T) {
	store := &memoryGrantStore{grant: Grant{RefreshToken: "refresh", Scopes: []string{testReadScope}, AuthorizedAt: time.Now().UTC()}, found: true}
	broker, err := New(Options{
		OAuth: OAuthClient{ClientID: "id", ClientSecret: "secret", AuthURI: "https://accounts.google.com/o/oauth2/v2/auth", TokenURI: "https://oauth2.googleapis.com/token", RedirectURI: "http://127.0.0.1/callback"},
		Store: store, AllowedScopes: []string{testReadScope},
	})
	if err != nil {
		t.Fatal(err)
	}
	issuerRequest := httptest.NewRequest(http.MethodPost, pluginv1.CapabilityRedeemPath, strings.NewReader(`{}`))
	issuerResponse := httptest.NewRecorder()
	broker.IssuerHandler().ServeHTTP(issuerResponse, issuerRequest)
	if issuerResponse.Code != http.StatusNotFound {
		t.Fatalf("issuer exposed redemption route: %d", issuerResponse.Code)
	}
	pluginRequest := httptest.NewRequest(http.MethodPost, pluginv1.CapabilityIssuePath, strings.NewReader(`{}`))
	pluginResponse := httptest.NewRecorder()
	broker.PluginHandler().ServeHTTP(pluginResponse, pluginRequest)
	if pluginResponse.Code != http.StatusNotFound {
		t.Fatalf("plugin endpoint exposed issuer route: %d", pluginResponse.Code)
	}
}

func getStatus(t *testing.T, baseURL string, scopes []string) pluginv1.AuthorizationStatus {
	t.Helper()
	query := url.Values{"provider": {googleProvider}}
	for _, scope := range scopes {
		query.Add("scope", scope)
	}
	response, err := http.Get(baseURL + pluginv1.AuthorizationStatusPath + "?" + query.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var status pluginv1.AuthorizationStatus
	if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func postJSON[T any](t *testing.T, target string, input any, expectedStatus int) T {
	t.Helper()
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.Post(target, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != expectedStatus {
		actual, _ := io.ReadAll(response.Body)
		t.Fatalf("POST %s status=%d want=%d body=%s", target, response.StatusCode, expectedStatus, actual)
	}
	var output T
	if err := json.NewDecoder(response.Body).Decode(&output); err != nil {
		t.Fatal(err)
	}
	return output
}

func deleteJSON[T any](t *testing.T, target string, expectedStatus int) T {
	t.Helper()
	request, err := http.NewRequest(http.MethodDelete, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != expectedStatus {
		actual, _ := io.ReadAll(response.Body)
		t.Fatalf("DELETE %s status=%d want=%d body=%s", target, response.StatusCode, expectedStatus, actual)
	}
	var output T
	if err := json.NewDecoder(response.Body).Decode(&output); err != nil {
		t.Fatal(err)
	}
	return output
}
