package builtin

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/z-chenhao/eri/internal/feedback"
	"github.com/z-chenhao/eri/internal/policy"
)

func TestFeedbackToolUsesGatewayTaskAndSeparatesOutcomeValidation(t *testing.T) {
	service := &fakeFeedbackService{}
	candidate, err := NewFeedback(service)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := candidate.Prepare(context.Background(), json.RawMessage(`{"kind":"correction","statement":"The correct date is Friday."}`))
	if err != nil || prepared.Action.Effect != policy.Reversible || prepared.Action.Target != "delivery:latest" {
		t.Fatalf("prepared=%+v err=%v", prepared, err)
	}
	prepared.TaskID = "gateway-task"
	result, err := candidate.Execute(context.Background(), prepared)
	if err != nil || service.taskID != "gateway-task" || service.kind != feedback.Correction || len(result.Output) == 0 {
		t.Fatalf("result=%+v service=%+v err=%v", result, service, err)
	}
	for _, body := range []string{
		`{"kind":"correction","outcome":"failure","statement":"wrong"}`,
		`{"kind":"outcome","statement":"it happened"}`,
		`{"kind":"accepted","statement":" "}`,
	} {
		if _, err := candidate.Prepare(context.Background(), json.RawMessage(body)); err == nil {
			t.Fatalf("invalid feedback accepted: %s", body)
		}
	}
}

type fakeFeedbackService struct {
	taskID string
	kind   feedback.Kind
}

func (s *fakeFeedbackService) Capture(_ context.Context, taskID string, kind feedback.Kind, outcome feedback.OutcomeStatus, statement, deliveryID string) (feedback.Record, error) {
	s.taskID, s.kind = taskID, kind
	return feedback.Record{ID: "feedback", FeedbackTaskID: taskID, SourceTaskID: "source", ArtifactID: "artifact", DeliveryID: "delivery", Kind: kind, Outcome: outcome}, nil
}
