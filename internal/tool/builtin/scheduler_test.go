package builtin

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/z-chenhao/eri/internal/scheduler"
)

type schedulerTestService struct {
	updatedTaskID       string
	updatedCommitmentID string
	updatedRequest      scheduler.CreateRequest
}

func (*schedulerTestService) Create(context.Context, string, scheduler.CreateRequest) (scheduler.Commitment, error) {
	return scheduler.Commitment{}, nil
}

func (s *schedulerTestService) Update(_ context.Context, taskID, commitmentID string, request scheduler.CreateRequest) (scheduler.Commitment, error) {
	s.updatedTaskID = taskID
	s.updatedCommitmentID = commitmentID
	s.updatedRequest = request
	return scheduler.Commitment{ID: commitmentID, Schedule: request.Schedule, Version: 2}, nil
}

func (*schedulerTestService) List(context.Context, int) ([]scheduler.Commitment, error) {
	return nil, nil
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
		"commitment_id":"commitment-1",
		"message":"Use the corrected monitoring scope",
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
	if service.updatedRequest.Message != "Use the corrected monitoring scope" || service.updatedRequest.Schedule.IntervalSeconds != 3600 {
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
		"message":"Corrected scope",
		"schedule":{"type":"interval","interval_seconds":3600},
		"importance":"normal",
		"delivery_route":"origin_channel"
	}`)); err == nil {
		t.Fatal("update without commitment_id was accepted")
	}
}
