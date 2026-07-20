// Package providersecret isolates cloud-model credentials from Eri Core and
// persists them only in the operating-system credential store.
package providersecret

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os/exec"
	"os/user"
	"runtime"
	"strings"
	"time"

	"github.com/z-chenhao/eri/internal/keychain"
)

const keychainService = "io.github.z-chenhao.eri.provider.deepseek"

type credential struct {
	APIKey  string `json:"api_key"`
	BaseURL string `json:"base_url"`
}

type Store interface {
	Load(context.Context) (credential, bool, error)
	Save(context.Context, credential) error
	Delete(context.Context) error
}

type KeychainStore struct {
	account string
}

func NewKeychainStore() (*KeychainStore, error) {
	if runtime.GOOS != "darwin" {
		return nil, fmt.Errorf("cloud credential persistence currently requires macOS Keychain")
	}
	current, err := user.Current()
	if err != nil {
		return nil, err
	}
	return &KeychainStore{account: current.Username}, nil
}

func (s *KeychainStore) Load(ctx context.Context) (credential, bool, error) {
	command := exec.CommandContext(ctx, "/usr/bin/security", "find-generic-password", "-a", s.account, "-s", keychainService, "-w")
	body, err := command.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 44 {
			return credential{}, false, nil
		}
		return credential{}, false, fmt.Errorf("read model credential from macOS Keychain: %w", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(body)))
	if err != nil {
		return credential{}, false, fmt.Errorf("decode model credential: %w", err)
	}
	var value credential
	if err := json.Unmarshal(decoded, &value); err != nil || strings.TrimSpace(value.APIKey) == "" {
		return credential{}, false, fmt.Errorf("model credential in Keychain is invalid")
	}
	if _, err := safeProviderURL(value.BaseURL); err != nil {
		return credential{}, false, fmt.Errorf("model credential origin is invalid")
	}
	return value, true, nil
}

func (s *KeychainStore) Save(ctx context.Context, value credential) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	encoded := base64.StdEncoding.EncodeToString(body)
	if err := keychain.AddGenericPassword(ctx, s.account, keychainService, encoded); err != nil {
		return fmt.Errorf("store model credential in macOS Keychain: %w", err)
	}
	return nil
}

func (s *KeychainStore) Delete(ctx context.Context) error {
	command := exec.CommandContext(ctx, "/usr/bin/security", "delete-generic-password", "-a", s.account, "-s", keychainService)
	if err := command.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 44 {
			return nil
		}
		return fmt.Errorf("delete model credential from macOS Keychain: %w", err)
	}
	return nil
}

type Broker struct {
	store  Store
	http   *http.Client
	logger *slog.Logger
}

func NewBroker(store Store, logger *slog.Logger) (*Broker, error) {
	if store == nil {
		return nil, fmt.Errorf("provider secret store is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Broker{store: store, http: &http.Client{Timeout: 30 * time.Second}, logger: logger}, nil
}

func (b *Broker) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", b.health)
	mux.HandleFunc("GET /v1/deepseek/status", b.status)
	mux.HandleFunc("PUT /v1/deepseek/credential", b.configure)
	mux.HandleFunc("DELETE /v1/deepseek/credential", b.disconnect)
	mux.HandleFunc("POST /chat/completions", b.proxyChat)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		mux.ServeHTTP(w, r)
	})
}

func (b *Broker) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (b *Broker) status(w http.ResponseWriter, r *http.Request) {
	value, found, err := b.store.Load(r.Context())
	if err != nil {
		b.internalError(w, "read provider credential status", err)
		return
	}
	response := map[string]any{"configured": found}
	if found {
		response["provider"] = "deepseek"
		response["origin"] = value.BaseURL
	}
	writeJSON(w, http.StatusOK, response)
}

func (b *Broker) configure(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	var input struct {
		APIKey  string `json:"api_key"`
		BaseURL string `json:"base_url"`
		Model   string `json:"model"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || strings.TrimSpace(input.APIKey) == "" {
		writeError(w, http.StatusBadRequest, "A DeepSeek API key is required.")
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "Credential request is invalid.")
		return
	}
	baseURL, err := safeProviderURL(input.BaseURL)
	if err != nil {
		writeError(w, http.StatusBadRequest, "The DeepSeek endpoint is invalid.")
		return
	}
	models, err := b.validateCredential(r.Context(), baseURL, strings.TrimSpace(input.APIKey))
	if err != nil {
		b.logger.Warn("provider credential validation failed", "component", "provider_secret_broker", "provider", "deepseek", "status", "rejected")
		writeError(w, http.StatusBadGateway, "DeepSeek rejected the credential or could not be reached.")
		return
	}
	if !containsModel(models, strings.TrimSpace(input.Model)) {
		writeError(w, http.StatusUnprocessableEntity, "The selected model is not available to this DeepSeek account.")
		return
	}
	if err := b.store.Save(r.Context(), credential{APIKey: strings.TrimSpace(input.APIKey), BaseURL: baseURL}); err != nil {
		b.internalError(w, "store provider credential", err)
		return
	}
	b.logger.Info("provider credential configured", "component", "provider_secret_broker", "provider", "deepseek", "origin", baseURL)
	writeJSON(w, http.StatusOK, map[string]any{"configured": true, "models": models})
}

func (b *Broker) disconnect(w http.ResponseWriter, r *http.Request) {
	if err := b.store.Delete(r.Context()); err != nil {
		b.internalError(w, "delete provider credential", err)
		return
	}
	b.logger.Info("provider credential removed", "component", "provider_secret_broker", "provider", "deepseek")
	writeJSON(w, http.StatusOK, map[string]bool{"configured": false})
}

func (b *Broker) validateCredential(ctx context.Context, baseURL, apiKey string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	response, err := b.doProviderRequest(req, baseURL)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(response.Body, 64*1024))
		return nil, fmt.Errorf("provider returned HTTP %d", response.StatusCode)
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 2*1024*1024)).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode provider model inventory: %w", err)
	}
	models := make([]string, 0, len(body.Data))
	for _, model := range body.Data {
		if name := strings.TrimSpace(model.ID); name != "" {
			models = append(models, name)
		}
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("provider returned no available models")
	}
	return models, nil
}

func containsModel(models []string, selected string) bool {
	if selected == "" {
		return false
	}
	for _, model := range models {
		if strings.TrimSpace(model) == selected {
			return true
		}
	}
	return false
}

func (b *Broker) proxyChat(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	value, found, err := b.store.Load(r.Context())
	if err != nil {
		b.internalError(w, "load provider credential", err)
		return
	}
	if !found {
		writeError(w, http.StatusPreconditionFailed, "DeepSeek is not configured.")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16*1024*1024))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "Model request is too large.")
		return
	}
	request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, value.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		b.internalError(w, "create provider request", err)
		return
	}
	request.Header.Set("Authorization", "Bearer "+value.APIKey)
	request.Header.Set("Content-Type", "application/json")
	response, err := b.doProviderRequest(request, value.BaseURL)
	if err != nil {
		b.logger.Error("provider proxy call failed", "component", "provider_secret_broker", "provider", "deepseek", "duration_ms", time.Since(started).Milliseconds(), "error", safeError(err))
		writeError(w, http.StatusBadGateway, "DeepSeek could not be reached.")
		return
	}
	defer response.Body.Close()
	b.logger.Info("provider proxy call finished", "component", "provider_secret_broker", "provider", "deepseek", "status_code", response.StatusCode, "duration_ms", time.Since(started).Milliseconds())
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		io.Copy(io.Discard, io.LimitReader(response.Body, 64*1024))
		writeError(w, response.StatusCode, fmt.Sprintf("DeepSeek returned HTTP %d.", response.StatusCode))
		return
	}
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 8*1024*1024+1))
	if err != nil {
		writeError(w, http.StatusBadGateway, "DeepSeek returned an unreadable response.")
		return
	}
	if len(responseBody) > 8*1024*1024 {
		writeError(w, http.StatusBadGateway, "DeepSeek response exceeded the safe size limit.")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(responseBody)
}

func (b *Broker) doProviderRequest(request *http.Request, baseURL string) (*http.Response, error) {
	origin, _ := url.Parse(baseURL)
	client := *b.http
	client.CheckRedirect = func(next *http.Request, _ []*http.Request) error {
		if subtle.ConstantTimeCompare([]byte(next.URL.Scheme+"://"+next.URL.Host), []byte(origin.Scheme+"://"+origin.Host)) != 1 {
			return fmt.Errorf("provider redirect left configured origin")
		}
		return nil
	}
	return client.Do(request)
}

func (b *Broker) internalError(w http.ResponseWriter, operation string, err error) {
	b.logger.Error(operation+" failed", "component", "provider_secret_broker", "error", safeError(err))
	writeError(w, http.StatusInternalServerError, "Provider credential operation failed.")
}

func safeProviderURL(value string) (string, error) {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(value), "/"))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("provider URL must be an HTTPS origin")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return "", fmt.Errorf("provider URL must not include a path")
	}
	return parsed.Scheme + "://" + parsed.Host, nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"message": message}})
}

func safeError(err error) string {
	if err == nil {
		return ""
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, err.Error())
}
