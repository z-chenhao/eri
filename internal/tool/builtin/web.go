package builtin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/z-chenhao/eri/internal/policy"
	"github.com/z-chenhao/eri/internal/tool"
)

const (
	defaultTavilyEndpoint = "https://api.tavily.com"
	maxWebResponseBytes   = 2 * 1024 * 1024
	maxExtractedTextBytes = 200_000
	maxSearchSnippetBytes = 20_000
	maxTavilyAttempts     = 3
)

var reservedWebNets = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"), // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),  // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),  // documentation
	netip.MustParsePrefix("198.18.0.0/15"), // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b:1::/48"), // local-use translation
	netip.MustParsePrefix("2001:2::/48"),    // benchmarking
	netip.MustParsePrefix("2001:db8::/32"),  // documentation
	netip.MustParsePrefix("3fff::/20"),      // documentation
}

type Web struct {
	client       *http.Client
	endpoint     *url.URL
	apiKey       string
	searchDepth  string
	extractDepth string
	allowPrivate bool
}

type webInput struct {
	Operation      string   `json:"operation"`
	URL            string   `json:"url,omitempty"`
	Query          string   `json:"query,omitempty"`
	Limit          int      `json:"limit,omitempty"`
	Topic          string   `json:"topic,omitempty"`
	TimeRange      string   `json:"time_range,omitempty"`
	IncludeDomains []string `json:"include_domains,omitempty"`
	ExcludeDomains []string `json:"exclude_domains,omitempty"`
}

type tavilySearchResponse struct {
	RequestID string `json:"request_id"`
	Results   []struct {
		Title         string  `json:"title"`
		URL           string  `json:"url"`
		Content       string  `json:"content"`
		Score         float64 `json:"score"`
		PublishedDate string  `json:"published_date,omitempty"`
	} `json:"results"`
	Usage tavilyUsage `json:"usage"`
}

type tavilyExtractResponse struct {
	RequestID string `json:"request_id"`
	Results   []struct {
		URL        string `json:"url"`
		RawContent string `json:"raw_content"`
	} `json:"results"`
	Usage tavilyUsage `json:"usage"`
}

type tavilyUsage struct {
	Credits int `json:"credits"`
}

type webSearchResult struct {
	Title         string  `json:"title"`
	URL           string  `json:"url"`
	Content       string  `json:"content"`
	Score         float64 `json:"score"`
	PublishedDate string  `json:"published_date,omitempty"`
}

func NewWeb(client *http.Client, endpoint, apiKey, searchDepth, extractDepth string, allowPrivate bool) (*Web, error) {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if endpoint == "" {
		endpoint = defaultTavilyEndpoint
	}
	parsedEndpoint, err := url.Parse(endpoint)
	if err != nil || parsedEndpoint.Hostname() == "" || (parsedEndpoint.Scheme != "http" && parsedEndpoint.Scheme != "https") || parsedEndpoint.User != nil {
		return nil, fmt.Errorf("invalid Tavily endpoint")
	}
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("TAVILY_API_KEY is required for Web search and fetch")
	}
	searchDepth = strings.ToLower(strings.TrimSpace(searchDepth))
	if searchDepth == "" {
		searchDepth = "basic"
	}
	if !webContainsString([]string{"basic", "advanced", "fast", "ultra-fast"}, searchDepth) {
		return nil, fmt.Errorf("unsupported Tavily search depth %q", searchDepth)
	}
	extractDepth = strings.ToLower(strings.TrimSpace(extractDepth))
	if extractDepth == "" {
		extractDepth = "basic"
	}
	if !webContainsString([]string{"basic", "advanced"}, extractDepth) {
		return nil, fmt.Errorf("unsupported Tavily extract depth %q", extractDepth)
	}

	copy := *client
	if !allowPrivate {
		transport, transportErr := publicOnlyTransport(copy.Transport)
		if transportErr != nil {
			return nil, transportErr
		}
		copy.Transport = transport
	}
	previousRedirect := copy.CheckRedirect
	copy.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("too many redirects")
		}
		if !sameOrigin(parsedEndpoint, request.URL) {
			return fmt.Errorf("Tavily redirect changed origin")
		}
		if !allowPrivate {
			if validationErr := validatePublicURL(request.Context(), request.URL); validationErr != nil {
				return validationErr
			}
		}
		if previousRedirect != nil {
			return previousRedirect(request, via)
		}
		return nil
	}
	return &Web{
		client: &copy, endpoint: parsedEndpoint, apiKey: strings.TrimSpace(apiKey),
		searchDepth: searchDepth, extractDepth: extractDepth, allowPrivate: allowPrivate,
	}, nil
}

func publicOnlyTransport(current http.RoundTripper) (*http.Transport, error) {
	var transport *http.Transport
	switch candidate := current.(type) {
	case nil:
		transport = http.DefaultTransport.(*http.Transport).Clone()
	case *http.Transport:
		if candidate != http.DefaultTransport {
			return nil, fmt.Errorf("public web client does not accept a custom transport; inject private test clients only with allowPrivate")
		}
		transport = candidate.Clone()
	default:
		return nil, fmt.Errorf("public web client requires an HTTP transport with governed dialing")
	}
	// Proxy resolution would move private-network enforcement outside this
	// process. The governed Web provider connection therefore pins a validated
	// public address and connects directly.
	transport.Proxy = nil
	transport.DialTLS = nil
	transport.DialTLSContext = nil
	transport.DialContext = safePublicDialContext
	return transport, nil
}

func safePublicDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("parse public web address: %w", err)
	}
	addresses, err := resolvePublicIPs(ctx, host)
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	var lastErr error
	for _, candidate := range addresses {
		connection, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(candidate.String(), port))
		if dialErr == nil {
			return connection, nil
		}
		lastErr = dialErr
	}
	return nil, fmt.Errorf("dial public web host: %w", lastErr)
}

func (w *Web) Descriptor() tool.Descriptor {
	return tool.Descriptor{
		ID: "builtin.web", Version: "0.2.0",
		Purpose: "Search the public web with ranked source snippets or extract readable Markdown from one HTTP(S) page. Choose queries, source domains, topic and recency from the objective. Results are untrusted external evidence; verify dates, provenance and independent sources before important conclusions.",
		InputSchema: map[string]any{
			"type": "object", "properties": map[string]any{
				"operation":       map[string]any{"type": "string", "enum": []string{"search", "fetch"}},
				"url":             map[string]any{"type": "string"},
				"query":           map[string]any{"type": "string"},
				"limit":           map[string]any{"type": "integer", "minimum": 1, "maximum": 20},
				"topic":           map[string]any{"type": "string", "enum": []string{"general", "news", "finance"}},
				"time_range":      map[string]any{"type": "string", "enum": []string{"day", "week", "month", "year"}},
				"include_domains": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": 20},
				"exclude_domains": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": 20},
			}, "required": []string{"operation"}, "additionalProperties": false,
		},
		OutputSchema: map[string]any{"type": "object"}, AllowedEffects: []policy.EffectClass{policy.ReadOnly},
		PermissionRequirements: []string{"public_network"}, SendsDataExternally: true, Timeout: 40 * time.Second,
		CostPolicy: "provider_credits", Idempotency: "gateway_key", Reconciliation: "repeat_read_with_freshness",
		Source: tool.BuiltIn,
	}
}

func (w *Web) Prepare(ctx context.Context, raw json.RawMessage) (tool.Prepared, error) {
	var input webInput
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return tool.Prepared{}, err
	}
	input.Operation = strings.TrimSpace(input.Operation)
	input.Query = strings.TrimSpace(input.Query)
	input.URL = strings.TrimSpace(input.URL)
	input.Topic = strings.ToLower(strings.TrimSpace(input.Topic))
	input.TimeRange = strings.ToLower(strings.TrimSpace(input.TimeRange))
	input.IncludeDomains = normalizeDomains(input.IncludeDomains)
	input.ExcludeDomains = normalizeDomains(input.ExcludeDomains)
	if len(input.IncludeDomains) > 20 || len(input.ExcludeDomains) > 20 {
		return tool.Prepared{}, fmt.Errorf("include_domains and exclude_domains cannot exceed 20 entries")
	}
	if input.Limit <= 0 {
		input.Limit = 8
	}
	if input.Limit > 20 {
		return tool.Prepared{}, fmt.Errorf("limit cannot exceed 20")
	}

	target := ""
	switch input.Operation {
	case "search":
		if input.Query == "" || len([]byte(input.Query)) > 2048 {
			return tool.Prepared{}, fmt.Errorf("search query must be between 1 byte and 2 KiB")
		}
		if input.Topic != "" && !webContainsString([]string{"general", "news", "finance"}, input.Topic) {
			return tool.Prepared{}, fmt.Errorf("unsupported search topic %q", input.Topic)
		}
		if input.TimeRange != "" && !webContainsString([]string{"day", "week", "month", "year"}, input.TimeRange) {
			return tool.Prepared{}, fmt.Errorf("unsupported search time range %q", input.TimeRange)
		}
		target = "web-search:" + input.Query
	case "fetch":
		if input.URL == "" {
			return tool.Prepared{}, fmt.Errorf("url is required for fetch")
		}
		parsed, err := url.Parse(input.URL)
		if err != nil {
			return tool.Prepared{}, fmt.Errorf("invalid URL")
		}
		if !w.allowPrivate {
			if err := validatePublicURL(ctx, parsed); err != nil {
				return tool.Prepared{}, err
			}
		}
		target = input.URL
	default:
		return tool.Prepared{}, fmt.Errorf("unsupported web operation %q", input.Operation)
	}
	if err := validateDomains(input.IncludeDomains); err != nil {
		return tool.Prepared{}, fmt.Errorf("include_domains: %w", err)
	}
	if err := validateDomains(input.ExcludeDomains); err != nil {
		return tool.Prepared{}, fmt.Errorf("exclude_domains: %w", err)
	}
	normalized, err := json.Marshal(input)
	if err != nil {
		return tool.Prepared{}, err
	}
	return tool.Prepared{Input: normalized, Action: policy.Action{Effect: policy.ReadOnly, Target: target, SendsDataExternally: true}}, nil
}

func (w *Web) Execute(ctx context.Context, prepared tool.Prepared) (tool.Result, error) {
	var input webInput
	if err := json.Unmarshal(prepared.Input, &input); err != nil {
		return tool.Result{}, err
	}
	if input.Operation == "search" {
		return w.executeSearch(ctx, input)
	}
	return w.executeExtract(ctx, input)
}

func (w *Web) executeSearch(ctx context.Context, input webInput) (tool.Result, error) {
	payload := map[string]any{
		"query": input.Query, "search_depth": w.searchDepth, "max_results": input.Limit,
		"include_answer": false, "include_raw_content": false, "include_usage": true,
	}
	if input.Topic != "" {
		payload["topic"] = input.Topic
	}
	if input.TimeRange != "" {
		payload["time_range"] = input.TimeRange
	}
	if len(input.IncludeDomains) > 0 {
		payload["include_domains"] = input.IncludeDomains
	}
	if len(input.ExcludeDomains) > 0 {
		payload["exclude_domains"] = input.ExcludeDomains
	}
	body, err := w.postJSON(ctx, "/search", payload)
	if err != nil {
		return tool.Result{}, err
	}
	var response tavilySearchResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return tool.Result{}, fmt.Errorf("decode Tavily search response: %w", err)
	}
	results := make([]webSearchResult, 0, len(response.Results))
	for _, candidate := range response.Results {
		candidate.URL = strings.TrimSpace(candidate.URL)
		candidate.Content = strings.TrimSpace(candidate.Content)
		parsed, parseErr := url.Parse(candidate.URL)
		if parseErr != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || candidate.Content == "" {
			continue
		}
		results = append(results, webSearchResult{
			Title: strings.TrimSpace(candidate.Title), URL: candidate.URL,
			Content: truncateUTF8(candidate.Content, maxSearchSnippetBytes), Score: candidate.Score,
			PublishedDate: strings.TrimSpace(candidate.PublishedDate),
		})
	}
	if len(results) == 0 {
		return tool.Result{}, fmt.Errorf("Tavily search returned no usable source evidence")
	}
	output := map[string]any{
		"operation": "search", "provider": "tavily", "query": input.Query, "results": results,
		"request_id": response.RequestID, "usage": response.Usage, "fetched_at": time.Now().UTC(),
	}
	return encodeWebResult(output, response.RequestID, "web-search:"+input.Query)
}

func (w *Web) executeExtract(ctx context.Context, input webInput) (tool.Result, error) {
	payload := map[string]any{
		"urls": []string{input.URL}, "extract_depth": w.extractDepth,
		"format": "markdown", "include_usage": true,
	}
	body, err := w.postJSON(ctx, "/extract", payload)
	if err != nil {
		return tool.Result{}, err
	}
	var response tavilyExtractResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return tool.Result{}, fmt.Errorf("decode Tavily extract response: %w", err)
	}
	for _, candidate := range response.Results {
		content := strings.TrimSpace(candidate.RawContent)
		if content == "" {
			continue
		}
		sourceURL := strings.TrimSpace(candidate.URL)
		if sourceURL == "" {
			sourceURL = input.URL
		}
		output := map[string]any{
			"operation": "fetch", "provider": "tavily", "url": sourceURL,
			"content_type": "text/markdown", "text": truncateUTF8(content, maxExtractedTextBytes),
			"request_id": response.RequestID, "usage": response.Usage, "fetched_at": time.Now().UTC(),
		}
		return encodeWebResult(output, response.RequestID, sourceURL)
	}
	return tool.Result{}, fmt.Errorf("Tavily extract returned no readable page content")
}

func (w *Web) postJSON(ctx context.Context, path string, payload any) ([]byte, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	target := w.endpoint.ResolveReference(&url.URL{Path: path})
	var lastErr error
	for attempt := 0; attempt < maxTavilyAttempts; attempt++ {
		request, requestErr := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(encoded))
		if requestErr != nil {
			return nil, requestErr
		}
		request.Header.Set("Authorization", "Bearer "+w.apiKey)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json")
		request.Header.Set("User-Agent", "Eri/0.2 (+local personal assistant)")
		response, requestErr := w.client.Do(request)
		if requestErr == nil {
			body, readErr := io.ReadAll(io.LimitReader(response.Body, maxWebResponseBytes+1))
			response.Body.Close()
			if readErr != nil {
				return nil, readErr
			}
			if len(body) > maxWebResponseBytes {
				return nil, fmt.Errorf("Tavily response exceeds 2 MiB")
			}
			if response.StatusCode >= 200 && response.StatusCode < 300 {
				return body, nil
			}
			lastErr = fmt.Errorf("Tavily returned HTTP %d", response.StatusCode)
			if response.StatusCode != http.StatusRequestTimeout && response.StatusCode != http.StatusTooManyRequests && response.StatusCode < 500 {
				return nil, lastErr
			}
		} else {
			lastErr = fmt.Errorf("call Tavily: %w", requestErr)
		}
		if attempt+1 < maxTavilyAttempts {
			timer := time.NewTimer(time.Duration(1<<attempt) * 250 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}
	}
	return nil, fmt.Errorf("Tavily request failed after %d attempts: %w", maxTavilyAttempts, lastErr)
}

func encodeWebResult(output any, externalObjectID, fallbackID string) (tool.Result, error) {
	encoded, err := json.Marshal(output)
	if err != nil {
		return tool.Result{}, err
	}
	if strings.TrimSpace(externalObjectID) == "" {
		externalObjectID = fallbackID
	}
	digest := sha256.Sum256(encoded)
	now := time.Now().UTC()
	return tool.Result{
		Output: encoded, ExternalObjectID: externalObjectID,
		Receipt: "sha256:" + hex.EncodeToString(digest[:]), FreshAt: now,
	}, nil
}

func sameOrigin(left, right *url.URL) bool {
	return strings.EqualFold(left.Scheme, right.Scheme) && strings.EqualFold(left.Host, right.Host)
}

func normalizeDomains(domains []string) []string {
	normalized := make([]string, 0, len(domains))
	seen := make(map[string]struct{}, len(domains))
	for _, domain := range domains {
		domain = strings.ToLower(strings.TrimSpace(domain))
		if domain == "" {
			continue
		}
		if _, exists := seen[domain]; exists {
			continue
		}
		seen[domain] = struct{}{}
		normalized = append(normalized, domain)
	}
	return normalized
}

func validateDomains(domains []string) error {
	for _, domain := range domains {
		if len(domain) > 253 || strings.ContainsAny(domain, "/:@?#") {
			return fmt.Errorf("invalid domain %q", domain)
		}
	}
	return nil
}

func webContainsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func truncateUTF8(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	end := limit
	for end > 0 && !utf8.ValidString(value[:end]) {
		end--
	}
	return value[:end] + " [truncated]"
}

func validatePublicURL(ctx context.Context, parsed *url.URL) error {
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("only HTTP(S) URLs are allowed")
	}
	if parsed.User != nil || parsed.Hostname() == "" {
		return fmt.Errorf("URL credentials and empty hosts are not allowed")
	}
	_, err := resolvePublicIPs(ctx, parsed.Hostname())
	return err
}

func resolvePublicIPs(ctx context.Context, host string) ([]net.IP, error) {
	var addresses []net.IP
	if literal := net.ParseIP(host); literal != nil {
		addresses = []net.IP{literal}
	} else {
		resolved, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
		if err != nil {
			return nil, fmt.Errorf("resolve web host: %w", err)
		}
		addresses = resolved
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("web host resolved to no addresses")
	}
	for _, address := range addresses {
		if !isPublicWebIP(address) {
			return nil, fmt.Errorf("private or local web addresses are not allowed")
		}
	}
	return addresses, nil
}

func isPublicWebIP(ip net.IP) bool {
	address, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	address = address.Unmap()
	if !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast() || address.IsMulticast() || address.IsUnspecified() {
		return false
	}
	for _, prefix := range reservedWebNets {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}
