package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	pluginv1 "github.com/z-chenhao/eri/api/plugin/v1"
)

type capabilityHandleRequest = pluginv1.CapabilityHandleRequest
type capabilityHandleResponse = pluginv1.CapabilityHandleResponse

// issueCapabilityHandle asks the independent Auth Broker for an opaque,
// invocation-bound handle. Eri never receives the provider access/refresh
// token. The plugin redeems the handle directly with the broker in memory.
func issueCapabilityHandle(ctx context.Context, auth *AuthSpec, request capabilityHandleRequest) (capabilityHandleResponse, error) {
	if auth == nil {
		return capabilityHandleResponse{}, nil
	}
	if err := validateAuthSpec(auth); err != nil {
		return capabilityHandleResponse{}, err
	}
	if request.MaxUses != 1 || request.TTLSeconds <= 0 || request.TTLSeconds > 120 {
		return capabilityHandleResponse{}, fmt.Errorf("external auth capability must be single-use and expire within 120 seconds")
	}
	if strings.TrimSpace(request.PluginID) == "" || strings.TrimSpace(request.TaskID) == "" || strings.TrimSpace(request.RunID) == "" || strings.TrimSpace(request.InvocationID) == "" || request.Provider != auth.Provider || !slices.Equal(request.Scopes, auth.Scopes) {
		return capabilityHandleResponse{}, fmt.Errorf("external auth capability binding does not match the plugin declaration")
	}
	issuedAt := time.Now().UTC()
	endpoint := strings.TrimSpace(os.Getenv(auth.BrokerEndpointEnvironment))
	client, target, err := authBrokerHTTP(endpoint)
	if err != nil {
		return capabilityHandleResponse{}, err
	}
	body, _ := json.Marshal(request)
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, target+pluginv1.CapabilityIssuePath, bytes.NewReader(body))
	if err != nil {
		return capabilityHandleResponse{}, err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	response, err := client.Do(httpRequest)
	if err != nil {
		return capabilityHandleResponse{}, fmt.Errorf("contact external auth broker: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return capabilityHandleResponse{}, fmt.Errorf("external auth broker denied capability handle: %s", response.Status)
	}
	var issued capabilityHandleResponse
	decoder := json.NewDecoder(io.LimitReader(response.Body, 16*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&issued); err != nil {
		return capabilityHandleResponse{}, fmt.Errorf("decode auth broker response: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return capabilityHandleResponse{}, fmt.Errorf("decode auth broker response: trailing JSON data")
	}
	if strings.TrimSpace(issued.Handle) == "" || len(issued.Handle) > 4096 || !issued.ExpiresAt.After(issuedAt) || issued.ExpiresAt.After(issuedAt.Add(time.Duration(request.TTLSeconds)*time.Second+5*time.Second)) {
		return capabilityHandleResponse{}, fmt.Errorf("external auth broker returned an invalid capability handle")
	}
	return issued, nil
}

func authBrokerHTTP(endpoint string) (*http.Client, string, error) {
	parsed, err := url.Parse(endpoint)
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

func brokerEndpointIdentity(endpoint string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || (parsed.Host == "" && parsed.Scheme != "unix") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("invalid external auth broker endpoint")
	}
	if parsed.Scheme == "unix" {
		if parsed.Host != "" {
			return "", fmt.Errorf("auth broker unix endpoint cannot contain a host")
		}
		path := filepath.Clean(parsed.Path)
		if !filepath.IsAbs(path) || path == string(filepath.Separator) {
			return "", fmt.Errorf("auth broker unix socket must be an absolute file path")
		}
		if resolved, resolveErr := filepath.EvalSymlinks(path); resolveErr == nil {
			path = resolved
		}
		return "unix:" + path, nil
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("auth broker endpoint must use unix, loopback http, or https")
	}
	if parsed.Scheme == "http" {
		ip := net.ParseIP(parsed.Hostname())
		if ip == nil || !ip.IsLoopback() {
			return "", fmt.Errorf("plaintext auth broker must be loopback-only")
		}
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed.String(), nil
}
