package builtin

import (
	"context"
	"encoding/json"
	"testing"

	assistanttask "github.com/z-chenhao/eri/internal/task"
)

type fakeTaskService struct {
	canceled string
	retried  string
}

func (*fakeTaskService) List(context.Context, int) ([]assistanttask.Record, error) {
	return []assistanttask.Record{{ID: "task-1", Status: "running", Objective: "prepare report"}}, nil
}
func (*fakeTaskService) Inspect(context.Context, string) (assistanttask.Record, error) {
	return assistanttask.Record{ID: "task-1", Status: "running"}, nil
}
func (f *fakeTaskService) Cancel(_ context.Context, id string) (assistanttask.CancelResult, error) {
	f.canceled = id
	return assistanttask.CancelResult{TaskID: id, Status: "running", Effect: "cancel_requested"}, nil
}
func (f *fakeTaskService) Retry(_ context.Context, id string) (assistanttask.RetryResult, error) {
	f.retried = id
	return assistanttask.RetryResult{SourceTaskID: id, TaskID: "retry-task", Status: "queued", Checkpoint: "task_start"}, nil
}

func TestTasksToolListsAndRequestsSoftCancellation(t *testing.T) {
	service := &fakeTaskService{}
	candidate, err := NewTasks(service)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := candidate.Prepare(context.Background(), json.RawMessage(`{"operation":"cancel","task_id":"task-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	result, err := candidate.Execute(context.Background(), prepared)
	if err != nil || service.canceled != "task-1" || !json.Valid(result.Output) || result.Receipt == "" {
		t.Fatalf("result=%s receipt=%q canceled=%q err=%v", result.Output, result.Receipt, service.canceled, err)
	}
}

func TestTasksToolRequestsSafeRetry(t *testing.T) {
	service := &fakeTaskService{}
	candidate, err := NewTasks(service)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := candidate.Prepare(context.Background(), json.RawMessage(`{"operation":"retry","task_id":"failed-task"}`))
	if err != nil {
		t.Fatal(err)
	}
	result, err := candidate.Execute(context.Background(), prepared)
	if err != nil || service.retried != "failed-task" || !json.Valid(result.Output) {
		t.Fatalf("result=%s retried=%q err=%v", result.Output, service.retried, err)
	}
}
