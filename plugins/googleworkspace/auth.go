package googleworkspace

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"time"

	protocol "github.com/modelcontextprotocol/go-sdk/mcp"
	pluginv1 "github.com/z-chenhao/eri/api/plugin/v1"
)

type invocationMetadata struct {
	TaskID       string `json:"task_id"`
	RunID        string `json:"run_id"`
	InvocationID string `json:"invocation_id"`
	Auth         struct {
		Mode             string    `json:"mode"`
		Provider         string    `json:"provider"`
		Scopes           []string  `json:"scopes"`
		CapabilityHandle string    `json:"capability_handle"`
		ExpiresAt        time.Time `json:"expires_at"`
	} `json:"auth"`
}

type brokerClient struct {
	httpClient *http.Client
	baseURL    string
	provider   string
	pluginID   string
	allowed    map[string]struct{}
	now        func() time.Time
}

func (b *brokerClient) status(ctx context.Context, scopes []string) (pluginv1.AuthorizationStatus, error) {
	if err := b.validateRequestedScopes(scopes); err != nil {
		return pluginv1.AuthorizationStatus{}, err
	}
	query := url.Values{"provider": {b.provider}}
	for _, scope := range scopes {
		query.Add("scope", scope)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, b.baseURL+pluginv1.AuthorizationStatusPath+"?"+query.Encode(), nil)
	if err != nil {
		return pluginv1.AuthorizationStatus{}, err
	}
	var status pluginv1.AuthorizationStatus
	if err := b.doPublicJSON(request, nil, &status); err != nil {
		return pluginv1.AuthorizationStatus{}, err
	}
	if status.Provider != b.provider || !sameScopeSet(append(append([]string(nil), status.GrantedScopes...), status.MissingScopes...), scopes) {
		return pluginv1.AuthorizationStatus{}, fmt.Errorf("external authorization broker returned inconsistent status")
	}
	return status, nil
}

func (b *brokerClient) start(ctx context.Context, scopes []string) (pluginv1.AuthorizationStartResponse, error) {
	if err := b.validateRequestedScopes(scopes); err != nil {
		return pluginv1.AuthorizationStartResponse{}, err
	}
	payload := pluginv1.AuthorizationStartRequest{Provider: b.provider, Scopes: append([]string(nil), scopes...)}
	body, _ := json.Marshal(payload)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, b.baseURL+pluginv1.AuthorizationStartPath, bytes.NewReader(body))
	if err != nil {
		return pluginv1.AuthorizationStartResponse{}, err
	}
	var started pluginv1.AuthorizationStartResponse
	if err := b.doPublicJSON(request, body, &started); err != nil {
		return pluginv1.AuthorizationStartResponse{}, err
	}
	authorizationURL, err := url.Parse(started.AuthorizationURL)
	if err != nil || authorizationURL.Scheme != "https" || authorizationURL.Hostname() != "accounts.google.com" || started.Provider != b.provider || !started.ExpiresAt.After(b.now().UTC()) {
		return pluginv1.AuthorizationStartResponse{}, fmt.Errorf("external authorization broker returned an invalid Google authorization URL")
	}
	return started, nil
}

func (b *brokerClient) revoke(ctx context.Context) (pluginv1.AuthorizationRevokeResponse, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, b.baseURL+pluginv1.AuthorizationRevokePath, nil)
	if err != nil {
		return pluginv1.AuthorizationRevokeResponse{}, err
	}
	var revoked pluginv1.AuthorizationRevokeResponse
	if err := b.doPublicJSON(request, nil, &revoked); err != nil {
		return pluginv1.AuthorizationRevokeResponse{}, err
	}
	if revoked.Provider != b.provider || !revoked.Revoked || revoked.RevokedAt.IsZero() || strings.TrimSpace(revoked.Receipt) == "" {
		return pluginv1.AuthorizationRevokeResponse{}, fmt.Errorf("external authorization broker returned an invalid revocation receipt")
	}
	return revoked, nil
}

func (b *brokerClient) validateRequestedScopes(scopes []string) error {
	if len(scopes) == 0 || len(scopes) > len(b.allowed) {
		return fmt.Errorf("authorization requires a bounded non-empty scope set")
	}
	seen := make(map[string]struct{}, len(scopes))
	for _, scope := range scopes {
		if _, ok := b.allowed[scope]; !ok {
			return fmt.Errorf("authorization requested a scope outside the plugin manifest")
		}
		if _, duplicate := seen[scope]; duplicate {
			return fmt.Errorf("authorization requested a duplicate scope")
		}
		seen[scope] = struct{}{}
	}
	return nil
}

func (b *brokerClient) doPublicJSON(request *http.Request, body []byte, target any) error {
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := b.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("contact external authorization broker: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return fmt.Errorf("external authorization broker returned %s", response.Status)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 32*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode external authorization broker response: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("decode external authorization broker response: trailing JSON data")
	}
	return nil
}

func newBrokerClient(endpoint, provider, pluginID string, allowedScopes []string) (*brokerClient, error) {
	client, baseURL, err := brokerHTTP(endpoint)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(provider) == "" || strings.TrimSpace(pluginID) == "" || len(allowedScopes) == 0 {
		return nil, fmt.Errorf("auth broker provider, plugin id and allowed scopes are required")
	}
	allowed := make(map[string]struct{}, len(allowedScopes))
	for _, scope := range allowedScopes {
		scope = strings.TrimSpace(scope)
		if scope == "" {
			return nil, fmt.Errorf("auth broker contains an empty allowed scope")
		}
		allowed[scope] = struct{}{}
	}
	return &brokerClient{httpClient: client, baseURL: baseURL, provider: provider, pluginID: pluginID, allowed: allowed, now: time.Now}, nil
}

func (b *brokerClient) redeem(ctx context.Context, request *protocol.CallToolRequest, requiredScope string) (string, error) {
	metadata, err := parseInvocationMetadata(request)
	if err != nil {
		return "", err
	}
	if metadata.Auth.Mode != "external_broker" || metadata.Auth.Provider != b.provider || metadata.Auth.ExpiresAt.Before(b.now().UTC()) {
		return "", fmt.Errorf("Eri capability metadata is invalid or expired")
	}
	if _, ok := b.allowed[requiredScope]; !ok || !slices.Contains(metadata.Auth.Scopes, requiredScope) {
		return "", fmt.Errorf("Eri capability does not grant the tool's required scope")
	}
	for _, scope := range metadata.Auth.Scopes {
		if _, ok := b.allowed[scope]; !ok {
			return "", fmt.Errorf("Eri capability expanded beyond the plugin manifest")
		}
	}
	redeem := pluginv1.RedemptionRequest{
		InvocationBinding: pluginv1.InvocationBinding{
			PluginID: b.pluginID, TaskID: metadata.TaskID, RunID: metadata.RunID, InvocationID: metadata.InvocationID,
		},
		Provider: b.provider, Scopes: append([]string(nil), metadata.Auth.Scopes...), Handle: metadata.Auth.CapabilityHandle,
	}
	body, _ := json.Marshal(redeem)
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, b.baseURL+pluginv1.CapabilityRedeemPath, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	response, err := b.httpClient.Do(httpRequest)
	if err != nil {
		return "", fmt.Errorf("redeem external authorization capability: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return "", fmt.Errorf("external authorization broker denied redemption: %s", response.Status)
	}
	var token pluginv1.RedemptionResponse
	decoder := json.NewDecoder(io.LimitReader(response.Body, 32*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&token); err != nil {
		return "", fmt.Errorf("decode authorization redemption: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return "", fmt.Errorf("decode authorization redemption: trailing JSON data")
	}
	now := b.now().UTC()
	if token.Provider != b.provider || !sameScopeSet(token.Scopes, metadata.Auth.Scopes) || !strings.EqualFold(token.TokenType, "Bearer") ||
		strings.TrimSpace(token.AccessToken) == "" || len(token.AccessToken) > 16*1024 || !token.ExpiresAt.After(now.Add(5*time.Second)) || token.ExpiresAt.After(now.Add(2*time.Hour)) {
		return "", fmt.Errorf("external authorization broker returned an invalid short-lived token")
	}
	return token.AccessToken, nil
}

func parseInvocationMetadata(request *protocol.CallToolRequest) (invocationMetadata, error) {
	if request == nil || request.Params == nil {
		return invocationMetadata{}, fmt.Errorf("Eri invocation metadata is missing")
	}
	raw, ok := request.Params.Meta[pluginv1.ResultMetadataKey]
	if !ok {
		return invocationMetadata{}, fmt.Errorf("Eri invocation metadata is missing")
	}
	body, err := json.Marshal(raw)
	if err != nil {
		return invocationMetadata{}, fmt.Errorf("encode Eri invocation metadata: %w", err)
	}
	var metadata invocationMetadata
	if err := json.Unmarshal(body, &metadata); err != nil {
		return invocationMetadata{}, fmt.Errorf("decode Eri invocation metadata: %w", err)
	}
	if strings.TrimSpace(metadata.TaskID) == "" || strings.TrimSpace(metadata.RunID) == "" || strings.TrimSpace(metadata.InvocationID) == "" ||
		strings.TrimSpace(metadata.Auth.CapabilityHandle) == "" || len(metadata.Auth.CapabilityHandle) > 4096 || len(metadata.Auth.Scopes) == 0 {
		return invocationMetadata{}, fmt.Errorf("Eri invocation metadata is incomplete")
	}
	return metadata, nil
}

func sameScopeSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	values := make(map[string]int, len(left))
	for _, value := range left {
		values[value]++
	}
	for _, value := range right {
		values[value]--
	}
	for _, count := range values {
		if count != 0 {
			return false
		}
	}
	return true
}

func brokerHTTP(endpoint string) (*http.Client, string, error) {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || (parsed.Host == "" && parsed.Scheme != "unix") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, "", fmt.Errorf("invalid external auth broker endpoint")
	}
	switch parsed.Scheme {
	case "unix":
		if parsed.Host != "" {
			return nil, "", fmt.Errorf("auth broker unix endpoint cannot contain a host")
		}
		socket := filepath.Clean(parsed.Path)
		if !filepath.IsAbs(socket) || socket == string(filepath.Separator) {
			return nil, "", fmt.Errorf("auth broker unix socket must be an absolute file path")
		}
		transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socket)
		}}
		return &http.Client{Transport: transport, Timeout: 15 * time.Second}, "http://eri-auth-broker", nil
	case "http":
		ip := net.ParseIP(parsed.Hostname())
		if ip == nil || !ip.IsLoopback() {
			return nil, "", fmt.Errorf("plaintext auth broker must be loopback-only")
		}
	case "https":
	default:
		return nil, "", fmt.Errorf("auth broker endpoint must use unix, loopback http, or https")
	}
	origin := parsed.Scheme + "://" + parsed.Host
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(request *http.Request, _ []*http.Request) error {
		if request.URL.Scheme+"://"+request.URL.Host != origin {
			return fmt.Errorf("external auth broker redirect left the configured origin")
		}
		return nil
	}}
	if parsed.Scheme == "http" {
		client.Transport = &http.Transport{Proxy: nil}
	}
	return client, strings.TrimRight(endpoint, "/"), nil
}
