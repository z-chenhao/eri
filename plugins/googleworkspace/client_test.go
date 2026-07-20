package googleworkspace

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGoogleClientRejectsUnsafeBaseURLs(t *testing.T) {
	for _, candidate := range []string{"http://localhost:8080", "https://user:secret@google.example", "file:///tmp/google"} {
		if _, err := newGoogleClient(nil, candidate, "https://gmail.googleapis.com"); err == nil {
			t.Fatalf("unsafe Google API base accepted: %s", candidate)
		}
	}
}

func TestGoogleClientDoesNotForwardTokenAcrossOrigins(t *testing.T) {
	targetCalled := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		targetCalled = true
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	client, err := newGoogleClient(source.Client(), source.URL, source.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.doJSON(context.Background(), "provider-token", http.MethodGet, source.URL+"/calendar", nil, nil)
	if err == nil || targetCalled {
		t.Fatalf("cross-origin Google redirect err=%v target_called=%v", err, targetCalled)
	}
}
