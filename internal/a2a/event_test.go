package a2a

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/z-chenhao/eri/internal/eventlog"
)

type artifactResolver struct{}

func (artifactResolver) ResolveA2AArtifact(context.Context, string) (Artifact, bool, error) {
	return Artifact{ArtifactID: "artifact-1", Parts: []Part{{Text: "final result", MediaType: "text/plain"}}}, true, nil
}

func TestProjectUsesA2AStatusAndArtifactWrappers(t *testing.T) {
	projector := Projector{Artifacts: artifactResolver{}}
	scope := Context{ContextID: "conversation-1", TaskID: "task-1"}
	status, err := projector.Project(context.Background(), eventlog.Event{Type: "task.started", Time: time.Now()}, scope)
	if err != nil || len(status) != 1 || status[0].StatusUpdate.Status.State != TaskWorking {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	artifact, err := projector.Project(context.Background(), eventlog.Event{
		Type: "delivery.sent", Time: time.Now(), Data: map[string]any{"artifact_id": "artifact-1"},
	}, scope)
	if err != nil || len(artifact) != 1 || artifact[0].ArtifactUpdate.Artifact.Parts[0].Text != "final result" || !artifact[0].ArtifactUpdate.LastChunk {
		t.Fatalf("artifact=%+v err=%v", artifact, err)
	}
	encoded, err := json.Marshal(artifact[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) == "" || encoded[0] != '{' || !strings.Contains(string(encoded), `"index":0`) {
		t.Fatalf("invalid A2A JSON: %s", encoded)
	}
}
