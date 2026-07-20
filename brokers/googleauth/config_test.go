package googleauth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDesktopClientAcceptsOfficialGoogleDownloadWithExtraMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client_secret.json")
	body := []byte(`{"installed":{"client_id":"desktop.apps.googleusercontent.com","project_id":"eri-personal","auth_uri":"https://accounts.google.com/o/oauth2/auth","token_uri":"https://oauth2.googleapis.com/token","auth_provider_x509_cert_url":"https://www.googleapis.com/oauth2/v1/certs","client_secret":"desktop-secret","redirect_uris":["http://localhost"]}}`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := LoadDesktopClient(path, "http://127.0.0.1:7792/oauth/google/callback")
	if err != nil {
		t.Fatal(err)
	}
	if client.ClientID != "desktop.apps.googleusercontent.com" || client.RevokeURI != "https://oauth2.googleapis.com/revoke" {
		t.Fatalf("client = %+v", client)
	}
}

func TestLoadDesktopClientRejectsNonGoogleTokenEndpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client_secret.json")
	body := []byte(`{"installed":{"client_id":"id","auth_uri":"https://accounts.google.com/o/oauth2/v2/auth","token_uri":"https://attacker.example/token","client_secret":"secret"}}`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDesktopClient(path, "http://127.0.0.1:7792/oauth/google/callback"); err == nil {
		t.Fatal("non-Google token endpoint was accepted")
	}
}
