package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/z-chenhao/eri/internal/scheduler"
)

type schedulerTestService struct {
	updatedTaskID       string
	updatedCommitmentID string
	updatedRequest      scheduler.CreateRequest
}

func (*schedulerTestService) Create(_ context.Context, _ string, request scheduler.CreateRequest) (scheduler.Commitment, error) {
	route := request.DeliveryRoute
	if route == "" {
		route = scheduler.DeliveryRouteOrigin
	}
	return scheduler.Commitment{ID: "commitment-created", Task: request.Task, Schedule: request.Schedule, DeliveryRoute: route, Status: "active"}, nil
}

func (s *schedulerTestService) Update(_ context.Context, taskID, commitmentID string, request scheduler.CreateRequest) (scheduler.Commitment, error) {
	s.updatedTaskID = taskID
	s.updatedCommitmentID = commitmentID
	s.updatedRequest = request
	return scheduler.Commitment{ID: commitmentID, Task: request.Task, Schedule: request.Schedule, DeliveryRoute: request.DeliveryRoute, Version: 2}, nil
}

func (*schedulerTestService) List(context.Context, int) ([]scheduler.Commitment, error) {
	return []scheduler.Commitment{{ID: "commitment-listed", Task: "Check whether the book was read", DeliveryRoute: scheduler.DeliveryRouteRecent, Status: "active"}}, nil
}

func TestSchedulerOutputsAssignmentWithoutRuntimeRouting(t *testing.T) {
	commitments, err := NewScheduler(&schedulerTestService{})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name      string
		raw       string
		want      string
		wantRoute string
	}{
		{name: "create", raw: `{"operation":"create","task":"Remind the user to read the book","schedule":{"type":"once","after_seconds":60}}`, want: "Remind the user to read the book", wantRoute: scheduler.DeliveryRouteOrigin},
		{name: "update", raw: `{"operation":"update","id":"commitment-1","task":"Use the corrected book assignment","schedule":{"type":"once","after_seconds":120},"importance":"normal","delivery_route":"origin_channel"}`, want: "Use the corrected book assignment", wantRoute: scheduler.DeliveryRouteOrigin},
		{name: "list", raw: `{"operation":"list"}`, want: "Check whether the book was read", wantRoute: scheduler.DeliveryRouteRecent},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			prepared, err := commitments.Prepare(context.Background(), json.RawMessage(test.raw))
			if err != nil {
				t.Fatal(err)
			}
			prepared.TaskID = "source-task"
			result, err := commitments.Execute(context.Background(), prepared)
			if err != nil {
				t.Fatal(err)
			}
			body := string(result.Output)
			if !strings.Contains(body, test.want) || !strings.Contains(body, test.wantRoute) || strings.Contains(body, "source_task_id") || strings.Contains(body, "task_ref") || strings.Contains(body, "conversation_id") {
				t.Fatalf("model-facing schedule output=%s", body)
			}
		})
	}
}

func (*schedulerTestService) SetStatus(context.Context, string, string) error { return nil }

func TestSchedulerUpdateTargetsExistingCommitment(t *testing.T) {
	service := &schedulerTestService{}
	commitments, err := NewScheduler(service)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := commitments.Prepare(context.Background(), json.RawMessage(`{
		"operation":"update",
		"id":"commitment-1",
		"task":"Use the corrected monitoring scope",
		"schedule":{"type":"interval","interval_seconds":3600},
		"importance":"normal",
		"delivery_route":"origin_channel"
	}`))
	if err != nil {
		t.Fatal(err)
	}
	prepared.TaskID = "clarification-task"
	if _, err := commitments.Execute(context.Background(), prepared); err != nil {
		t.Fatal(err)
	}
	if service.updatedTaskID != "clarification-task" || service.updatedCommitmentID != "commitment-1" {
		t.Fatalf("update target task=%q commitment=%q", service.updatedTaskID, service.updatedCommitmentID)
	}
	if service.updatedRequest.Task != "Use the corrected monitoring scope" || service.updatedRequest.Schedule.IntervalSeconds != 3600 {
		t.Fatalf("update request = %+v", service.updatedRequest)
	}
}

func TestSchedulerUpdateRequiresCommitmentID(t *testing.T) {
	commitments, err := NewScheduler(&schedulerTestService{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := commitments.Prepare(context.Background(), json.RawMessage(`{
		"operation":"update",
		"task":"Corrected scope",
		"schedule":{"type":"interval","interval_seconds":3600},
		"importance":"normal",
		"delivery_route":"origin_channel"
	}`)); err == nil {
		t.Fatal("update without id was accepted")
	}
}
