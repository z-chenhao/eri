package secret

import "testing"

func TestLooksLikeCredentialIsHighConfidence(t *testing.T) {
	for _, body := range []string{
		"sk-" + "abcdefghijklmnopqrstuvwxyz123456", "Authorization: Bearer abcdefghijklmnopqrstuvwxyz.1234",
		"password=correct-horse-battery-staple", `{"password":"correct-horse-battery-staple"}`,
		`{"refresh_token":"abcdefghijklmnopqrstuvwxyz"}`, "-----BEGIN PRIVATE KEY-----",
	} {
		if !LooksLikeCredential([]byte(body)) {
			t.Fatalf("credential not detected")
		}
	}
	for _, body := range []string{
		"Explain prompt tokens and output tokens", "The user enters passwords every time", "API keys must not be stored",
	} {
		if LooksLikeCredential([]byte(body)) {
			t.Fatalf("ordinary discussion rejected: %q", body)
		}
	}
}
