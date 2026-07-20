package googleauth

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

type googleClientFile struct {
	Installed *struct {
		ClientID     string   `json:"client_id"`
		ClientSecret string   `json:"client_secret"`
		AuthURI      string   `json:"auth_uri"`
		TokenURI     string   `json:"token_uri"`
		RedirectURIs []string `json:"redirect_uris"`
	} `json:"installed"`
}

func LoadDesktopClient(path, redirectURI string) (OAuthClient, error) {
	file, err := os.Open(path)
	if err != nil {
		return OAuthClient{}, fmt.Errorf("open Google OAuth client file: %w", err)
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, 1024*1024+1))
	if err != nil {
		return OAuthClient{}, fmt.Errorf("read Google OAuth client file: %w", err)
	}
	if len(body) > 1024*1024 {
		return OAuthClient{}, fmt.Errorf("Google OAuth client file exceeds 1 MiB")
	}
	var document googleClientFile
	if err := json.Unmarshal(body, &document); err != nil {
		return OAuthClient{}, fmt.Errorf("decode Google OAuth client file: %w", err)
	}
	if document.Installed == nil || strings.TrimSpace(document.Installed.ClientID) == "" || strings.TrimSpace(document.Installed.ClientSecret) == "" {
		return OAuthClient{}, fmt.Errorf("Google OAuth client file must contain Desktop app credentials")
	}
	if document.Installed.AuthURI != "https://accounts.google.com/o/oauth2/auth" && document.Installed.AuthURI != "https://accounts.google.com/o/oauth2/v2/auth" {
		return OAuthClient{}, fmt.Errorf("Google OAuth authorization endpoint is not official")
	}
	if document.Installed.TokenURI != "https://oauth2.googleapis.com/token" {
		return OAuthClient{}, fmt.Errorf("Google OAuth token endpoint is not official")
	}
	if !strings.HasPrefix(redirectURI, "http://127.0.0.1:") {
		return OAuthClient{}, fmt.Errorf("Google Desktop OAuth redirect must use numeric loopback")
	}
	return OAuthClient{
		ClientID: document.Installed.ClientID, ClientSecret: document.Installed.ClientSecret,
		AuthURI: document.Installed.AuthURI, TokenURI: document.Installed.TokenURI,
		RevokeURI: "https://oauth2.googleapis.com/revoke", RedirectURI: redirectURI,
	}, nil
}
