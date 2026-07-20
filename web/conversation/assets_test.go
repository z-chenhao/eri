package conversation

import (
	"bytes"
	"image/png"
	"strings"
	"testing"
)

func TestConversationWorkspaceCombinesNarrowConversationWithSafeRunObservation(t *testing.T) {
	t.Parallel()
	body, err := Assets.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := strings.ToLower(string(body))
	for _, required := range []string{
		"profile-header", "presence", "timeline", "composer", "search", "fileinput", "attachmentbutton",
		"run-workspace", "runtime-toggle", "execution canvas", "run overview", "agent loop", "topologybands", "memory", "recorded dependencies",
		"/brand/eri-mark.png", "/brand/eri-favicon-32.png", "manifest.webmanifest",
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("conversation surface missing %q", required)
		}
	}
	for _, forbidden := range []string{"sidebar", "task dashboard", "settings", "memory page", "plugin store", "new chat", "chain of thought", "private prompt"} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("conversation surface contains forbidden product concept %q", forbidden)
		}
	}
	behavior, err := Assets.ReadFile("observation.js")
	if err != nil {
		t.Fatal(err)
	}
	javascript := strings.ToLower(string(behavior))
	for _, required := range []string{"/api/v1/activity", "/api/v1/traces/", "depends_on", "iteration_id", "focusedloopid", "turns folded", "retrieved_count", "applied_count", "sent_to_external_model", "step.exchange", "call exchange", "json.stringify(value, null, 2)"} {
		if !strings.Contains(javascript, required) {
			t.Fatalf("conversation observation missing %q", required)
		}
	}
	if strings.Contains(javascript, "steps[index - 1]") {
		t.Fatal("conversation observation must not synthesize a linear topology")
	}
	for _, required := range []string{"canvaspan.x", "pointermove", "runtime intake", "capabilities & safety", "cursory += layout.height", "selectedstepid"} {
		if !strings.Contains(javascript, required) {
			t.Fatalf("execution canvas missing %q", required)
		}
	}
	interaction, err := Assets.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	interactionJS := string(interaction)
	if strings.Contains(interactionJS, "body.append('channel'") || strings.Contains(interactionJS, `body.append("channel"`) {
		t.Fatal("Conversation must not submit a caller-controlled source channel")
	}
	if !strings.Contains(interactionJS, "body.append('text', text)") {
		t.Fatal("Conversation multipart request is missing the message text field")
	}
	if !strings.Contains(interactionJS, "/api/v1/conversation/connect") || !strings.Contains(interactionJS, "introduction_started") {
		t.Fatal("Conversation must request the one-time model-generated introduction before enabling chat")
	}
	if !strings.Contains(interactionJS, "function renderMarkdown") || strings.Contains(interactionJS, ".innerHTML") {
		t.Fatal("assistant Markdown must be rendered through the safe DOM renderer")
	}
	style, err := Assets.ReadFile("app.css")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(style), ".identity { display: flex; align-items: center;") {
		t.Fatal("Eri name and presence must share one header row")
	}
	observationStyle, err := Assets.ReadFile("observation.css")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(observationStyle), ".profile { display: flex; align-items: center;") {
		t.Fatal("Eri portrait, name, and presence must share one header row")
	}
	if strings.Contains(string(observationStyle), ".profile { display: grid;") {
		t.Fatal("observation styles must not stack the Eri profile vertically")
	}
	for _, required := range []string{"height: clamp(560px", "min-height: 560px", ".step-exchange pre"} {
		if !strings.Contains(string(observationStyle), required) {
			t.Fatalf("execution canvas detail styles missing %q", required)
		}
	}
	assertTransparentBrandAsset(t, "brand/eri-mark.png")
	for _, path := range []string{"brand/eri-icon-192.png", "brand/eri-icon-512.png", "brand/eri-favicon-32.png", "manifest.webmanifest"} {
		if _, err := Assets.ReadFile(path); err != nil {
			t.Fatalf("conversation brand asset %q: %v", path, err)
		}
	}
}

func assertTransparentBrandAsset(t *testing.T, path string) {
	t.Helper()
	body, err := Assets.ReadFile(path)
	if err != nil {
		t.Fatalf("read brand asset %q: %v", path, err)
	}
	asset, err := png.Decode(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("decode brand asset %q: %v", path, err)
	}
	bounds := asset.Bounds()
	_, _, _, cornerAlpha := asset.At(bounds.Min.X, bounds.Min.Y).RGBA()
	_, _, _, centerAlpha := asset.At(bounds.Min.X+bounds.Dx()/2, bounds.Min.Y+bounds.Dy()/2).RGBA()
	if cornerAlpha != 0 {
		t.Fatalf("brand asset %q corner alpha = %d, want transparent", path, cornerAlpha)
	}
	if centerAlpha == 0 {
		t.Fatalf("brand asset %q center must remain visible", path)
	}
}
