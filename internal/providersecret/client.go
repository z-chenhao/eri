package providersecret

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

var ErrModelUnavailable = errors.New("selected DeepSeek model is unavailable")

// Client talks to the Broker over its private Unix socket.
type Client struct {
	http *http.Client
}

func NewClient(socketPath string) *Client {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "unix", socketPath)
	}}
	return &Client{http: &http.Client{Transport: transport, Timeout: 45 * time.Second}}
}

func (c *Client) Health(ctx context.Context) error {
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://eri-provider-broker/health", nil)
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("provider broker returned HTTP %d", response.StatusCode)
	}
	return nil
}

func (c *Client) ConfigureDeepSeek(ctx context.Context, baseURL, apiKey, model string) ([]string, error) {
	body, _ := json.Marshal(map[string]string{"base_url": baseURL, "api_key": apiKey, "model": model})
	request, _ := http.NewRequestWithContext(ctx, http.MethodPut, "http://eri-provider-broker/v1/deepseek/credential", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(response.Body, 64*1024))
		if response.StatusCode == http.StatusUnprocessableEntity {
			return nil, ErrModelUnavailable
		}
		return nil, fmt.Errorf("DeepSeek credential validation failed")
	}
	var result struct {
		Models []string `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 2*1024*1024)).Decode(&result); err != nil {
		return nil, err
	}
	return result.Models, nil
}

func (c *Client) Configured(ctx context.Context) (bool, error) {
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://eri-provider-broker/v1/deepseek/status", nil)
	response, err := c.http.Do(request)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	var result struct {
		Configured bool `json:"configured"`
	}
	if response.StatusCode != http.StatusOK {
		return false, fmt.Errorf("provider broker returned HTTP %d", response.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64*1024)).Decode(&result); err != nil {
		return false, err
	}
	return result.Configured, nil
}

func (c *Client) DeleteDeepSeek(ctx context.Context) error {
	request, _ := http.NewRequestWithContext(ctx, http.MethodDelete, "http://eri-provider-broker/v1/deepseek/credential", nil)
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("provider broker returned HTTP %d", response.StatusCode)
	}
	return nil
}
