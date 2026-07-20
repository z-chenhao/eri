// Package googleauth implements Eri's optional, independently deployed Google
// authorization broker. It is a separate trust zone: refresh tokens live in
// the OS credential store, access tokens live only in broker/plugin memory,
// and Eri Core receives only one-use opaque capability handles.
package googleauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	pluginv1 "github.com/z-chenhao/eri/api/plugin/v1"
	"github.com/z-chenhao/eri/internal/observability"
)

const (
	googleProvider = "google"
	maxBodyBytes   = 64 * 1024
)

type OAuthClient struct {
	ClientID     string
	ClientSecret string
	AuthURI      string
	TokenURI     string
	RevokeURI    string
	RedirectURI  string
}

type Grant struct {
	RefreshToken string    `json:"refresh_token"`
	Scopes       []string  `json:"scopes"`
	AuthorizedAt time.Time `json:"authorized_at"`
}

type GrantStore interface {
	Load(context.Context) (Grant, bool, error)
	Save(context.Context, Grant) error
	Delete(context.Context) error
}

type Options struct {
	OAuth         OAuthClient
	Store         GrantStore
	HTTPClient    *http.Client
	AllowedScopes []string
	Now           func() time.Time
	Logger        *slog.Logger
}

type pendingAuthorization struct {
	Verifier  string
	Scopes    []string
	ExpiresAt time.Time
}

type issuedCapability struct {
	Request   pluginv1.CapabilityHandleRequest
	ExpiresAt time.Time
}

type Broker struct {
	oauth      OAuthClient
	store      GrantStore
	httpClient *http.Client
	allowed    map[string]struct{}
	now        func() time.Time
	logger     *slog.Logger

	mu           sync.Mutex
	pending      map[string]pendingAuthorization
	capabilities map[string]issuedCapability
}

func New(options Options) (*Broker, error) {
	if options.Store == nil || strings.TrimSpace(options.OAuth.ClientID) == "" || strings.TrimSpace(options.OAuth.ClientSecret) == "" ||
		strings.TrimSpace(options.OAuth.AuthURI) == "" || strings.TrimSpace(options.OAuth.TokenURI) == "" || strings.TrimSpace(options.OAuth.RedirectURI) == "" {
		return nil, fmt.Errorf("Google OAuth client, redirect URI and credential store are required")
	}
	if options.HTTPClient == nil {
		options.HTTPClient = &http.Client{Timeout: 20 * time.Second}
	}
	httpClient := *options.HTTPClient
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return fmt.Errorf("Google OAuth endpoints must not redirect credential-bearing requests")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	allowed := make(map[string]struct{}, len(options.AllowedScopes))
	for _, scope := range options.AllowedScopes {
		scope = strings.TrimSpace(scope)
		if scope == "" {
			return nil, fmt.Errorf("allowed Google scopes contain an empty value")
		}
		allowed[scope] = struct{}{}
	}
	if len(allowed) == 0 {
		return nil, fmt.Errorf("at least one allowed Google scope is required")
	}
	return &Broker{
		oauth: options.OAuth, store: options.Store, httpClient: &httpClient, allowed: allowed, now: options.Now, logger: options.Logger,
		pending: make(map[string]pendingAuthorization), capabilities: make(map[string]issuedCapability),
	}, nil
}

func (b *Broker) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+pluginv1.AuthorizationStatusPath, b.authorizationStatus)
	mux.HandleFunc("POST "+pluginv1.AuthorizationStartPath, b.authorizationStart)
	mux.HandleFunc("DELETE "+pluginv1.AuthorizationRevokePath, b.authorizationRevoke)
	mux.HandleFunc("POST "+pluginv1.CapabilityIssuePath, b.issueCapability)
	mux.HandleFunc("POST "+pluginv1.CapabilityRedeemPath, b.redeemCapability)
	mux.Handle("/oauth/google/callback", b.CallbackHandler())
	return securityHeaders(mux)
}

// IssuerHandler is exposed only on the Core-side Unix socket. The plugin must
// never receive this endpoint, otherwise it could mint its own capabilities.
func (b *Broker) IssuerHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+pluginv1.CapabilityIssuePath, b.issueCapability)
	return securityHeaders(mux)
}

// PluginHandler is exposed on a distinct Unix socket inherited by the plugin.
// It can redeem a Core-issued handle, but it cannot issue one.
func (b *Broker) PluginHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+pluginv1.AuthorizationStatusPath, b.authorizationStatus)
	mux.HandleFunc("POST "+pluginv1.AuthorizationStartPath, b.authorizationStart)
	mux.HandleFunc("DELETE "+pluginv1.AuthorizationRevokePath, b.authorizationRevoke)
	mux.HandleFunc("POST "+pluginv1.CapabilityRedeemPath, b.redeemCapability)
	return securityHeaders(mux)
}

func (b *Broker) CallbackHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /oauth/google/callback", b.authorizationCallback)
	return securityHeaders(mux)
}

func (b *Broker) authorizationStatus(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("provider") != googleProvider {
		writeError(w, http.StatusBadRequest, "unsupported provider")
		return
	}
	scopes := r.URL.Query()["scope"]
	if err := b.validateScopes(scopes); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	grant, found, err := b.store.Load(r.Context())
	if err != nil {
		b.logger.Error("Google authorization status failed", "component", "google_auth_broker", "error_code", observability.ErrorCode(err), "error", observability.SafeError(err))
		writeError(w, http.StatusServiceUnavailable, "credential store unavailable")
		return
	}
	granted, missing := partitionScopes(scopes, grant.Scopes)
	writeJSON(w, http.StatusOK, pluginv1.AuthorizationStatus{
		Provider: googleProvider, Authorized: found && len(missing) == 0,
		GrantedScopes: granted, MissingScopes: missing, AuthorizedAt: grant.AuthorizedAt,
		CredentialSource: "external_os_credential_store",
	})
}

func (b *Broker) authorizationStart(w http.ResponseWriter, r *http.Request) {
	var request pluginv1.AuthorizationStartRequest
	if err := decodeStrict(r.Body, &request); err != nil || request.Provider != googleProvider {
		writeError(w, http.StatusBadRequest, "invalid authorization request")
		return
	}
	if err := b.validateScopes(request.Scopes); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	state, err := randomToken(32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "authorization state unavailable")
		return
	}
	verifier, err := randomToken(48)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "authorization verifier unavailable")
		return
	}
	challengeDigest := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(challengeDigest[:])
	expiresAt := b.now().UTC().Add(10 * time.Minute)
	b.mu.Lock()
	b.pruneLocked(b.now().UTC())
	b.pending[state] = pendingAuthorization{Verifier: verifier, Scopes: append([]string(nil), request.Scopes...), ExpiresAt: expiresAt}
	b.mu.Unlock()
	b.logger.Info("Google authorization started", "component", "google_auth_broker", "scope_count", len(request.Scopes), "expires_at", expiresAt)
	query := url.Values{
		"client_id": {b.oauth.ClientID}, "redirect_uri": {b.oauth.RedirectURI}, "response_type": {"code"},
		"scope": {strings.Join(request.Scopes, " ")}, "state": {state}, "access_type": {"offline"},
		"prompt": {"consent"}, "include_granted_scopes": {"true"}, "code_challenge": {challenge}, "code_challenge_method": {"S256"},
	}
	writeJSON(w, http.StatusCreated, pluginv1.AuthorizationStartResponse{
		Provider: googleProvider, AuthorizationURL: strings.TrimRight(b.oauth.AuthURI, "?") + "?" + query.Encode(), ExpiresAt: expiresAt,
	})
}

func (b *Broker) authorizationCallback(w http.ResponseWriter, r *http.Request) {
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	if state == "" || code == "" || r.URL.Query().Get("error") != "" {
		http.Error(w, "Google authorization was not completed.", http.StatusBadRequest)
		return
	}
	b.mu.Lock()
	pending, found := b.pending[state]
	delete(b.pending, state)
	b.mu.Unlock()
	if !found || !pending.ExpiresAt.After(b.now().UTC()) {
		http.Error(w, "Google authorization state is invalid or expired.", http.StatusBadRequest)
		return
	}
	token, err := b.exchangeAuthorizationCode(r.Context(), code, pending.Verifier)
	if err != nil {
		b.logger.Error("Google authorization exchange failed", "component", "google_auth_broker", "error_code", observability.ErrorCode(err), "error", observability.SafeError(err))
		http.Error(w, "Google authorization exchange failed.", http.StatusBadGateway)
		return
	}
	existing, existingFound, err := b.store.Load(r.Context())
	if err != nil {
		b.logger.Error("Google credential store read failed", "component", "google_auth_broker", "error_code", observability.ErrorCode(err), "error", observability.SafeError(err))
		http.Error(w, "External credential store is unavailable.", http.StatusServiceUnavailable)
		return
	}
	refreshToken := token.RefreshToken
	if refreshToken == "" && existingFound {
		refreshToken = existing.RefreshToken
	}
	if refreshToken == "" {
		http.Error(w, "Google did not return an offline refresh grant; revoke the previous grant and authorize again.", http.StatusConflict)
		return
	}
	if len(token.Scopes) > 0 {
		_, missing := partitionScopes(pending.Scopes, token.Scopes)
		if len(missing) != 0 {
			http.Error(w, "Google did not grant every requested scope.", http.StatusConflict)
			return
		}
	}
	grantedScopes := append(append(append([]string(nil), existing.Scopes...), token.Scopes...), pending.Scopes...)
	grantedScopes = b.allowedOnly(grantedScopes)
	if err := b.store.Save(r.Context(), Grant{RefreshToken: refreshToken, Scopes: uniqueSorted(grantedScopes), AuthorizedAt: b.now().UTC()}); err != nil {
		b.logger.Error("Google credential store write failed", "component", "google_auth_broker", "error_code", observability.ErrorCode(err), "error", observability.SafeError(err))
		http.Error(w, "Google grant could not be stored in the external credential store.", http.StatusServiceUnavailable)
		return
	}
	b.logger.Info("Google authorization completed", "component", "google_auth_broker", "scope_count", len(grantedScopes))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, "<!doctype html><meta charset=utf-8><title>Eri Google authorization</title><p>Google authorization is complete. You can close this window and return to Eri.</p>")
}

func (b *Broker) authorizationRevoke(w http.ResponseWriter, r *http.Request) {
	grant, found, err := b.store.Load(r.Context())
	if err != nil {
		b.logger.Error("Google authorization revoke read failed", "component", "google_auth_broker", "error_code", observability.ErrorCode(err), "error", observability.SafeError(err))
		writeError(w, http.StatusServiceUnavailable, "credential store unavailable")
		return
	}
	if !found {
		b.logger.Info("Google authorization already disconnected", "component", "google_auth_broker")
		writeJSON(w, http.StatusOK, pluginv1.AuthorizationRevokeResponse{
			Provider: googleProvider, Revoked: true, RevokedAt: b.now().UTC(), Receipt: "google-oauth:already-disconnected",
		})
		return
	}
	revokeURI := b.oauth.RevokeURI
	if revokeURI == "" {
		revokeURI = "https://oauth2.googleapis.com/revoke"
	}
	request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, revokeURI, strings.NewReader(url.Values{"token": {grant.RefreshToken}}.Encode()))
	if err != nil {
		writeError(w, http.StatusBadGateway, "Google revocation request failed")
		return
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := b.httpClient.Do(request)
	if err != nil {
		b.logger.Error("Google authorization revoke request failed", "component", "google_auth_broker", "error_code", observability.ErrorCode(err), "error", observability.SafeError(err))
		writeError(w, http.StatusBadGateway, "Google revocation request failed")
		return
	}
	io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusBadRequest {
		b.logger.Error("Google authorization revoke endpoint rejected request", "component", "google_auth_broker", "status_code", response.StatusCode)
		writeError(w, http.StatusBadGateway, "Google revocation endpoint rejected the request")
		return
	}
	if err := b.store.Delete(r.Context()); err != nil {
		b.logger.Error("Google revoked grant deletion failed", "component", "google_auth_broker", "error_code", observability.ErrorCode(err), "error", observability.SafeError(err))
		writeError(w, http.StatusServiceUnavailable, "revoked grant could not be removed from the credential store")
		return
	}
	b.mu.Lock()
	b.capabilities = make(map[string]issuedCapability)
	b.mu.Unlock()
	b.logger.Info("Google authorization revoked", "component", "google_auth_broker")
	writeJSON(w, http.StatusOK, pluginv1.AuthorizationRevokeResponse{
		Provider: googleProvider, Revoked: true, RevokedAt: b.now().UTC(), Receipt: "google-oauth:revoked",
	})
}

func (b *Broker) issueCapability(w http.ResponseWriter, r *http.Request) {
	var request pluginv1.CapabilityHandleRequest
	if err := decodeStrict(r.Body, &request); err != nil || request.Provider != googleProvider || request.MaxUses != 1 || request.TTLSeconds <= 0 || request.TTLSeconds > 120 || !validBinding(request.InvocationBinding) {
		writeError(w, http.StatusBadRequest, "invalid capability request")
		return
	}
	if err := b.validateScopes(request.Scopes); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	grant, found, err := b.store.Load(r.Context())
	if err != nil {
		b.logger.Error("Google capability issue credential read failed", "component", "google_auth_broker", "error_code", observability.ErrorCode(err), "error", observability.SafeError(err))
		writeError(w, http.StatusServiceUnavailable, "credential store unavailable")
		return
	}
	_, missing := partitionScopes(request.Scopes, grant.Scopes)
	if !found || len(missing) != 0 {
		writeError(w, http.StatusConflict, "Google authorization is missing required scopes")
		return
	}
	handle, err := randomToken(48)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "capability generation unavailable")
		return
	}
	expiresAt := b.now().UTC().Add(time.Duration(request.TTLSeconds) * time.Second)
	b.mu.Lock()
	b.pruneLocked(b.now().UTC())
	b.capabilities[handle] = issuedCapability{Request: request, ExpiresAt: expiresAt}
	b.mu.Unlock()
	b.logger.Info("Google capability issued", "component", "google_auth_broker", "binding", request.InvocationBinding, "scope_count", len(request.Scopes), "expires_at", expiresAt)
	writeJSON(w, http.StatusCreated, pluginv1.CapabilityHandleResponse{Handle: handle, ExpiresAt: expiresAt})
}

func (b *Broker) redeemCapability(w http.ResponseWriter, r *http.Request) {
	var request pluginv1.RedemptionRequest
	if err := decodeStrict(r.Body, &request); err != nil || request.Provider != googleProvider || !validBinding(request.InvocationBinding) || strings.TrimSpace(request.Handle) == "" {
		writeError(w, http.StatusBadRequest, "invalid capability redemption")
		return
	}
	if err := b.validateScopes(request.Scopes); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	b.mu.Lock()
	issued, found := b.capabilities[request.Handle]
	if found {
		delete(b.capabilities, request.Handle)
	}
	b.mu.Unlock()
	if !found || !issued.ExpiresAt.After(b.now().UTC()) || issued.Request.InvocationBinding != request.InvocationBinding || issued.Request.Provider != request.Provider || !sameScopes(issued.Request.Scopes, request.Scopes) {
		b.logger.Warn("Google capability redemption rejected", "component", "google_auth_broker", "binding", request.InvocationBinding, "reason", "invalid_or_expired")
		writeError(w, http.StatusForbidden, "capability is invalid, expired, consumed, or bound elsewhere")
		return
	}
	grant, found, err := b.store.Load(r.Context())
	if err != nil || !found {
		if err != nil {
			b.logger.Error("Google capability redemption credential read failed", "component", "google_auth_broker", "binding", request.InvocationBinding, "error_code", observability.ErrorCode(err), "error", observability.SafeError(err))
		}
		writeError(w, http.StatusServiceUnavailable, "Google credential is unavailable")
		return
	}
	token, err := b.refreshAccessToken(r.Context(), grant.RefreshToken)
	if err != nil {
		b.logger.Error("Google access token refresh failed", "component", "google_auth_broker", "binding", request.InvocationBinding, "error_code", observability.ErrorCode(err), "error", observability.SafeError(err))
		writeError(w, http.StatusBadGateway, "Google access token refresh failed")
		return
	}
	if token.RefreshToken != "" && token.RefreshToken != grant.RefreshToken {
		grant.RefreshToken = token.RefreshToken
		if err := b.store.Save(r.Context(), grant); err != nil {
			b.logger.Error("rotated Google grant write failed", "component", "google_auth_broker", "binding", request.InvocationBinding, "error_code", observability.ErrorCode(err), "error", observability.SafeError(err))
			writeError(w, http.StatusServiceUnavailable, "rotated Google grant could not be stored")
			return
		}
	}
	b.logger.Info("Google capability redeemed", "component", "google_auth_broker", "binding", request.InvocationBinding, "scope_count", len(request.Scopes), "expires_in_seconds", token.ExpiresIn)
	writeJSON(w, http.StatusOK, pluginv1.RedemptionResponse{
		Provider: googleProvider, Scopes: append([]string(nil), request.Scopes...), TokenType: "Bearer",
		AccessToken: token.AccessToken, ExpiresAt: b.now().UTC().Add(time.Duration(token.ExpiresIn) * time.Second),
	})
}

type oauthTokenResponse struct {
	AccessToken  string `json:"access_token"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
	TokenType    string `json:"token_type"`
	Scopes       []string
}

func (b *Broker) exchangeAuthorizationCode(ctx context.Context, code, verifier string) (oauthTokenResponse, error) {
	values := url.Values{
		"client_id": {b.oauth.ClientID}, "client_secret": {b.oauth.ClientSecret}, "code": {code},
		"code_verifier": {verifier}, "redirect_uri": {b.oauth.RedirectURI}, "grant_type": {"authorization_code"},
	}
	return b.requestToken(ctx, values)
}

func (b *Broker) refreshAccessToken(ctx context.Context, refreshToken string) (oauthTokenResponse, error) {
	values := url.Values{
		"client_id": {b.oauth.ClientID}, "client_secret": {b.oauth.ClientSecret},
		"refresh_token": {refreshToken}, "grant_type": {"refresh_token"},
	}
	return b.requestToken(ctx, values)
}

func (b *Broker) requestToken(ctx context.Context, values url.Values) (oauthTokenResponse, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, b.oauth.TokenURI, strings.NewReader(values.Encode()))
	if err != nil {
		return oauthTokenResponse{}, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := b.httpClient.Do(request)
	if err != nil {
		return oauthTokenResponse{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return oauthTokenResponse{}, fmt.Errorf("Google token endpoint returned %s", response.Status)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxBodyBytes+1))
	if err != nil {
		return oauthTokenResponse{}, fmt.Errorf("read Google token endpoint response: %w", err)
	}
	if len(body) > maxBodyBytes {
		return oauthTokenResponse{}, fmt.Errorf("Google token endpoint response exceeds 64 KiB")
	}
	var token oauthTokenResponse
	if err := json.Unmarshal(body, &token); err != nil {
		return oauthTokenResponse{}, err
	}
	if strings.TrimSpace(token.AccessToken) == "" || token.ExpiresIn < 30 || token.ExpiresIn > 24*60*60 || !strings.EqualFold(token.TokenType, "Bearer") {
		return oauthTokenResponse{}, fmt.Errorf("Google token endpoint returned an invalid token")
	}
	token.Scopes = strings.Fields(token.Scope)
	return token, nil
}

func (b *Broker) validateScopes(scopes []string) error {
	if len(scopes) == 0 || len(scopes) > len(b.allowed) {
		return fmt.Errorf("scope set is empty or exceeds the broker allowlist")
	}
	seen := make(map[string]struct{}, len(scopes))
	for _, scope := range scopes {
		if _, ok := b.allowed[scope]; !ok {
			return fmt.Errorf("scope is outside the broker allowlist")
		}
		if _, duplicate := seen[scope]; duplicate {
			return fmt.Errorf("scope set contains a duplicate")
		}
		seen[scope] = struct{}{}
	}
	return nil
}

func (b *Broker) allowedOnly(scopes []string) []string {
	result := make([]string, 0, len(scopes))
	for _, scope := range uniqueSorted(scopes) {
		if _, ok := b.allowed[scope]; ok {
			result = append(result, scope)
		}
	}
	return result
}

func (b *Broker) pruneLocked(now time.Time) {
	for state, pending := range b.pending {
		if !pending.ExpiresAt.After(now) {
			delete(b.pending, state)
		}
	}
	for handle, capability := range b.capabilities {
		if !capability.ExpiresAt.After(now) {
			delete(b.capabilities, handle)
		}
	}
}

func validBinding(binding pluginv1.InvocationBinding) bool {
	return strings.TrimSpace(binding.PluginID) != "" && strings.TrimSpace(binding.TaskID) != "" && strings.TrimSpace(binding.RunID) != "" && strings.TrimSpace(binding.InvocationID) != ""
}

func partitionScopes(requested, available []string) ([]string, []string) {
	set := make(map[string]struct{}, len(available))
	for _, scope := range available {
		set[scope] = struct{}{}
	}
	granted, missing := make([]string, 0), make([]string, 0)
	for _, scope := range requested {
		if _, ok := set[scope]; ok {
			granted = append(granted, scope)
		} else {
			missing = append(missing, scope)
		}
	}
	return granted, missing
}

func uniqueSorted(values []string) []string {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value != "" {
			set[value] = struct{}{}
		}
	}
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func sameScopes(left, right []string) bool {
	left = uniqueSorted(left)
	right = uniqueSorted(right)
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func randomToken(size int) (string, error) {
	body := make([]byte, size)
	if _, err := rand.Read(body); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(body), nil
}

func decodeStrict(reader io.Reader, target any) error {
	decoder := json.NewDecoder(io.LimitReader(reader, maxBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("trailing JSON data")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}
