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
	"strings"
)

const maxGoogleResponseBytes = 2 * 1024 * 1024

type googleClient struct {
	httpClient   *http.Client
	calendarBase string
	gmailBase    string
}

func newGoogleClient(httpClient *http.Client, calendarBase, gmailBase string) (*googleClient, error) {
	for _, candidate := range []string{calendarBase, gmailBase} {
		if err := validateGoogleAPIBase(candidate); err != nil {
			return nil, err
		}
	}
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	copy := *httpClient
	previousRedirect := copy.CheckRedirect
	copy.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) == 0 || request.URL.Scheme+"://"+request.URL.Host != via[0].URL.Scheme+"://"+via[0].URL.Host {
			return fmt.Errorf("Google API redirect left the original origin")
		}
		if previousRedirect != nil {
			return previousRedirect(request, via)
		}
		return nil
	}
	return &googleClient{httpClient: &copy, calendarBase: strings.TrimRight(calendarBase, "/"), gmailBase: strings.TrimRight(gmailBase, "/")}, nil
}

func validateGoogleAPIBase(value string) error {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("Google API base URL must be absolute and contain no credentials, query or fragment")
	}
	ip := net.ParseIP(parsed.Hostname())
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && ip != nil && ip.IsLoopback()) {
		return fmt.Errorf("Google API base URL must use HTTPS except on numeric loopback")
	}
	return nil
}

func (c *googleClient) doJSON(ctx context.Context, token, method, target string, input, output any) (http.Header, error) {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "Eri-Google-Workspace-Plugin/1.0")
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("Google API request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return response.Header, fmt.Errorf("Google API returned %s", response.Status)
	}
	if output == nil {
		responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxGoogleResponseBytes+1))
		if err != nil {
			return response.Header, fmt.Errorf("read Google API response: %w", err)
		}
		if len(responseBody) > maxGoogleResponseBytes {
			return response.Header, fmt.Errorf("Google API response exceeds 2 MiB")
		}
		return response.Header, nil
	}
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxGoogleResponseBytes+1))
	if err != nil {
		return response.Header, fmt.Errorf("read Google API response: %w", err)
	}
	if len(responseBody) > maxGoogleResponseBytes {
		return response.Header, fmt.Errorf("Google API response exceeds 2 MiB")
	}
	if err := json.Unmarshal(responseBody, output); err != nil {
		return response.Header, fmt.Errorf("decode Google API response: %w", err)
	}
	return response.Header, nil
}
