package observatory

import (
	"bytes"
	"image/png"
	"strings"
	"testing"
)

func TestObservatoryCombinesStabilityWithDirectedSystemAndRunTopologies(t *testing.T) {
	t.Parallel()
	body, err := Assets.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := strings.ToLower(string(body))
	for _, required := range []string{"health-overview", "run-health", "attention", "system-topology", "run-table", "system execution topology", "execution canvas", "run overview", "architecture aligned", "cancel safely", "retry safely", "export episode", "memory inspector", "stored ≠ retrieved ≠ injected ≠ applied", "self-evolution experiments", "unseen holdout", "/brand/eri-mark.png", "/brand/eri-favicon-32.png"} {
		if !strings.Contains(html, required) {
			t.Fatalf("observatory missing %q", required)
		}
	}
	if strings.Contains(html, "circular topology") {
		t.Fatal("observatory must not use a ring topology")
	}
	script, err := Assets.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	javascript := strings.ToLower(string(script))
	for _, required := range []string{"/api/v1/system/overview", "/api/v1/evolution/releases", "renderevolution", "offline_score", "holdout_signal_count", "renderhealth", "rendersystemtopology", "renderflow", "buildflowsteps", "calculateflowdepths", "focusedrunloopid", "agent_iteration", "runflowbands", "event.eriaggregateid", "event.time", "event.data", "selectedrunstepid", "runcanvaspan", "pointermove", "cursory"} {
		if !strings.Contains(javascript, required) {
			t.Fatalf("observatory behavior missing %q", required)
		}
	}
	if !strings.Contains(javascript, "detail.spans") || !strings.Contains(javascript, "span.depends_on") {
		t.Fatal("run inspector must render the backend causal RunSpan graph")
	}
	if strings.Contains(javascript, "steps[index - 1]") {
		t.Fatal("observatory must not synthesize a linear run topology")
	}
	if strings.Contains(javascript, ".innerhtml") {
		t.Fatal("observatory must build governed content with DOM nodes")
	}
	for _, obsolete := range []string{"event.aggregate_id", "event.aggregate_type", "event.created_at", "event.payload"} {
		if strings.Contains(javascript, obsolete) {
			t.Fatalf("observatory must not read obsolete event field %q", obsolete)
		}
	}
	memoryScript, err := Assets.ReadFile("memory.js")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(memoryScript)), ".innerhtml") {
		t.Fatal("memory inspector must build governed content with DOM nodes")
	}
	for _, required := range []string{"/api/v1/memory", "source_ref", "applied", "applied_count", "sent_to_external_model", "eri:observatory-step-selected"} {
		if !strings.Contains(strings.ToLower(string(memoryScript)), required) {
			t.Fatalf("memory inspector behavior missing %q", required)
		}
	}
	for _, path := range []string{"brand/eri-mark.png", "brand/eri-favicon-32.png"} {
		body, err := Assets.ReadFile(path)
		if err != nil {
			t.Fatalf("read observatory brand asset %q: %v", path, err)
		}
		asset, err := png.Decode(bytes.NewReader(body))
		if err != nil {
			t.Fatalf("decode observatory brand asset %q: %v", path, err)
		}
		bounds := asset.Bounds()
		_, _, _, alpha := asset.At(bounds.Min.X, bounds.Min.Y).RGBA()
		if alpha != 0 {
			t.Fatalf("observatory brand asset %q corner alpha = %d, want transparent", path, alpha)
		}
	}
}
