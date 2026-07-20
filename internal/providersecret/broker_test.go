package providersecret

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type memoryStore struct {
	mu    sync.Mutex
	value credential
	found bool
}

func (s *memoryStore) Load(context.Context) (credential, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.value, s.found, nil
}

func (s *memoryStore) Save(_ context.Context, value credential) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.value, s.found = value, true
	return nil
}

func (s *memoryStore) Delete(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.value, s.found = credential{}, false
	return nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestBrokerValidatesThenStoresCredentialAndAddsItOnlyAtEgress(t *testing.T) {
	store := &memoryStore{}
	broker, err := NewBroker(store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	const secret = "sk-test-provider-secret-value"
	var calls []string
	broker.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls = append(calls, request.Method+" "+request.URL.Path+" "+request.Header.Get("Authorization"))
		body := `{"data":[{"id":"deepseek-v4-flash"}]}`
		if request.URL.Path == "/chat/completions" {
			body = `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{}}`
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})

	configureBody, _ := json.Marshal(map[string]string{"api_key": secret, "base_url": "https://api.deepseek.com", "model": "deepseek-v4-flash"})
	configure := httptest.NewRequest(http.MethodPut, "/v1/deepseek/credential", bytes.NewReader(configureBody))
	configured := httptest.NewRecorder()
	broker.Handler().ServeHTTP(configured, configure)
	if configured.Code != http.StatusOK || !store.found {
		t.Fatalf("configure status=%d body=%s", configured.Code, configured.Body.String())
	}

	proxy := httptest.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader(`{"model":"deepseek-v4-flash"}`))
	proxied := httptest.NewRecorder()
	broker.Handler().ServeHTTP(proxied, proxy)
	if proxied.Code != http.StatusOK {
		t.Fatalf("proxy status=%d body=%s", proxied.Code, proxied.Body.String())
	}
	if len(calls) != 2 || calls[0] != "GET /models Bearer "+secret || calls[1] != "POST /chat/completions Bearer "+secret {
		t.Fatalf("provider calls = %#v", calls)
	}
	disconnect := httptest.NewRequest(http.MethodDelete, "/v1/deepseek/credential", nil)
	disconnected := httptest.NewRecorder()
	broker.Handler().ServeHTTP(disconnected, disconnect)
	if disconnected.Code != http.StatusOK || store.found {
		t.Fatalf("disconnect status=%d configured=%v", disconnected.Code, store.found)
	}
}

func TestBrokerNeverReturnsCredentialOrProviderErrorBody(t *testing.T) {
	store := &memoryStore{value: credential{APIKey: "sk-never-return-this-value", BaseURL: "https://api.deepseek.com"}, found: true}
	broker, _ := NewBroker(store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	broker.http.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("private provider body"))}, nil
	})
	request := httptest.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader(`{}`))
	response := httptest.NewRecorder()
	broker.Handler().ServeHTTP(response, request)
	body := response.Body.String()
	if strings.Contains(body, "sk-never-return") || strings.Contains(body, "private provider body") {
		t.Fatalf("broker response leaked protected data: %s", body)
	}
}

func TestBrokerRejectsUnavailableModelBeforeWritingCredential(t *testing.T) {
	store := &memoryStore{}
	broker, _ := NewBroker(store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	broker.http.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"data":[{"id":"deepseek-chat"}]}`))}, nil
	})
	body, _ := json.Marshal(map[string]string{
		"api_key": "sk-test-provider-secret-value", "base_url": "https://api.deepseek.com", "model": "deepseek-v4-flash",
	})
	request := httptest.NewRequest(http.MethodPut, "/v1/deepseek/credential", bytes.NewReader(body))
	response := httptest.NewRecorder()
	broker.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnprocessableEntity || store.found {
		t.Fatalf("status=%d credential_written=%v", response.Code, store.found)
	}
}

func TestSafeProviderURLRejectsCredentialBearingAndPathURLs(t *testing.T) {
	for _, candidate := range []string{"http://api.deepseek.com", "https://user:secret@api.deepseek.com", "https://api.deepseek.com/v1", "https://api.deepseek.com?key=x"} {
		if _, err := safeProviderURL(candidate); err == nil {
			t.Fatalf("unsafe provider URL %q accepted", candidate)
		}
	}
}
