package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestWebSearchAndExtractReturnTavilyEvidence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.Header.Get("Authorization") != "Bearer test-key" {
			t.Fatalf("unexpected request: %s auth=%q", request.Method, request.Header.Get("Authorization"))
		}
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/search":
			var payload map[string]any
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if payload["search_depth"] != "advanced" || payload["topic"] != "news" || payload["time_range"] != "week" {
				t.Fatalf("search payload = %+v", payload)
			}
			_, _ = writer.Write([]byte(`{"request_id":"search-request","results":[{"title":"Primary source","url":"https://example.com/primary","content":"Current evidence and date","score":0.91,"published_date":"2026-07-20"}],"usage":{"credits":2}}`))
		case "/extract":
			var payload map[string]any
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if payload["extract_depth"] != "basic" || payload["format"] != "markdown" {
				t.Fatalf("extract payload = %+v", payload)
			}
			_, _ = writer.Write([]byte(`{"request_id":"extract-request","results":[{"url":"https://example.com/primary","raw_content":"# Useful title\n\nEvidence and date"}],"failed_results":[],"usage":{"credits":1}}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	web, err := NewWeb(server.Client(), server.URL, "test-key", "advanced", "basic", true)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := web.Prepare(context.Background(), json.RawMessage(`{"operation":"search","query":"eri assistant","limit":5,"topic":"news","time_range":"week","include_domains":["example.com"]}`))
	if err != nil {
		t.Fatal(err)
	}
	result, err := web.Execute(context.Background(), prepared)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result.Output), "Current evidence and date") || result.ExternalObjectID != "search-request" {
		t.Fatalf("search output = %s, external ID = %q", result.Output, result.ExternalObjectID)
	}

	prepared, err = web.Prepare(context.Background(), json.RawMessage(`{"operation":"fetch","url":"`+server.URL+`/page"}`))
	if err != nil {
		t.Fatal(err)
	}
	result, err = web.Execute(context.Background(), prepared)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result.Output), "# Useful title") || !strings.Contains(string(result.Output), `"provider":"tavily"`) {
		t.Fatalf("fetch output = %s", result.Output)
	}
}

func TestWebRejectsEmptyProviderEvidence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/search" {
			_, _ = writer.Write([]byte(`{"request_id":"empty-search","results":[]}`))
			return
		}
		_, _ = writer.Write([]byte(`{"request_id":"empty-extract","results":[{"url":"https://example.com","raw_content":""}]}`))
	}))
	defer server.Close()
	web, err := NewWeb(server.Client(), server.URL, "test-key", "basic", "basic", true)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := web.Prepare(context.Background(), json.RawMessage(`{"operation":"search","query":"nothing"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := web.Execute(context.Background(), prepared); err == nil || !strings.Contains(err.Error(), "no usable source evidence") {
		t.Fatalf("empty search result = %v", err)
	}
	prepared, err = web.Prepare(context.Background(), json.RawMessage(`{"operation":"fetch","url":"`+server.URL+`/empty"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := web.Execute(context.Background(), prepared); err == nil || !strings.Contains(err.Error(), "no readable page content") {
		t.Fatalf("empty extract result = %v", err)
	}
}

func TestWebRetriesTransientTavilyFailure(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) < 3 {
			writer.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = writer.Write([]byte(`{"request_id":"recovered","results":[{"title":"Source","url":"https://example.com","content":"Recovered evidence","score":0.8}]}`))
	}))
	defer server.Close()
	web, err := NewWeb(server.Client(), server.URL, "test-key", "basic", "basic", true)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := web.Prepare(context.Background(), json.RawMessage(`{"operation":"search","query":"recover"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := web.Execute(context.Background(), prepared); err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 3 {
		t.Fatalf("attempts = %d", attempts.Load())
	}
}

func TestWebRejectsLoopbackAndCredentialURLs(t *testing.T) {
	web, err := NewWeb(nil, "https://api.tavily.com", "test-key", "basic", "basic", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{
		`{"operation":"fetch","url":"http://127.0.0.1/private"}`,
		`{"operation":"fetch","url":"https://user:password@example.com/private"}`,
	} {
		if _, err := web.Prepare(context.Background(), json.RawMessage(input)); err == nil {
			t.Fatalf("unsafe URL accepted: %s", input)
		}
	}
}

func TestWebRejectsSpecialUseAddressesThatAreNotPublicInternet(t *testing.T) {
	for _, host := range []string{
		"100.64.0.1",
		"192.0.2.1",
		"198.18.0.1",
		"203.0.113.1",
		"[2001:db8::1]",
	} {
		parsed := "http://" + host + "/"
		web, err := NewWeb(nil, "https://api.tavily.com", "test-key", "basic", "basic", false)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := web.Prepare(context.Background(), json.RawMessage(`{"operation":"fetch","url":"`+parsed+`"}`)); err == nil {
			t.Fatalf("special-use address accepted: %s", parsed)
		}
	}
}

func TestPublicWebTransportPinsValidatedDNSResultAndDisablesProxy(t *testing.T) {
	web, err := NewWeb(nil, "https://api.tavily.com", "test-key", "basic", "basic", false)
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := web.client.Transport.(*http.Transport)
	if !ok || transport.DialContext == nil || transport.Proxy != nil {
		t.Fatalf("public transport is not governed: %#v", web.client.Transport)
	}
	if _, err := transport.DialContext(context.Background(), "tcp", "127.0.0.1:80"); err == nil || !strings.Contains(err.Error(), "private or local") {
		t.Fatalf("governed dial accepted loopback: %v", err)
	}
	_, err = NewWeb(&http.Client{Transport: rejectingRoundTripper{}}, "https://api.tavily.com", "test-key", "basic", "basic", false)
	if err == nil || !strings.Contains(err.Error(), "governed dialing") {
		t.Fatalf("custom transport bypass accepted: %v", err)
	}
	_, err = NewWeb(&http.Client{Transport: &http.Transport{}}, "https://api.tavily.com", "test-key", "basic", "basic", false)
	if err == nil || !strings.Contains(err.Error(), "custom transport") {
		t.Fatalf("custom HTTP transport bypass accepted: %v", err)
	}
}

func TestWebRejectsCrossOriginRedirectBeforeCredentialForwarding(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("credential-bearing redirect reached another origin")
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Location", target.URL)
		writer.WriteHeader(http.StatusFound)
	}))
	defer redirect.Close()
	web, err := NewWeb(redirect.Client(), redirect.URL, "test-key", "basic", "basic", true)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := web.Prepare(context.Background(), json.RawMessage(`{"operation":"search","query":"redirect"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := web.Execute(context.Background(), prepared); err == nil || !strings.Contains(err.Error(), "changed origin") {
		t.Fatalf("cross-origin redirect result = %v", err)
	}
}

type rejectingRoundTripper struct{}

func (rejectingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("not used")
}

func ExampleWeb_Descriptor() {
	web, _ := NewWeb(nil, "https://api.tavily.com", "test-key", "basic", "basic", false)
	fmt.Println(web.Descriptor().ID, web.Descriptor().Version)
	// Output: builtin.web 0.2.0
}
