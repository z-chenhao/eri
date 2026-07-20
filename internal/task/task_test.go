package task

import (
	"context"
	"strings"
	"testing"

	"github.com/z-chenhao/eri/internal/content"
)

type taskRepository struct {
	records   []Record
	load      Record
	found     bool
	listLimit int
	canceled  string
	retried   string
}

func (r *taskRepository) ListTasks(_ context.Context, limit int) ([]Record, error) {
	r.listLimit = limit
	return append([]Record(nil), r.records...), nil
}
func (r *taskRepository) LoadTask(context.Context, string) (Record, bool, error) {
	return r.load, r.found, nil
}
func (r *taskRepository) RequestTaskCancel(_ context.Context, id string) (CancelResult, error) {
	r.canceled = id
	return CancelResult{TaskID: id, Status: "running", Effect: "cancel_requested"}, nil
}
func (r *taskRepository) RetryTask(_ context.Context, id string) (RetryResult, error) {
	r.retried = id
	return RetryResult{SourceTaskID: id, TaskID: "retry", Status: "queued", Checkpoint: "task_start"}, nil
}

type taskContent map[string][]byte

func (c taskContent) Get(_ context.Context, ref content.Ref) ([]byte, error) {
	return append([]byte(nil), c[ref.ObjectID]...), nil
}

func TestListResolvesAndBoundsPrivateTaskObjectives(t *testing.T) {
	objective := strings.Repeat("\u754c", 250)
	repository := &taskRepository{records: []Record{{ID: "task", ObjectiveRef: content.Ref{ObjectID: "objective"}}}}
	service := NewService(repository, taskContent{"objective": []byte("  " + objective + "  ")})
	records, err := service.List(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if repository.listLimit != 20 || len(records) != 1 {
		t.Fatalf("limit=%d records=%+v", repository.listLimit, records)
	}
	runes := []rune(records[0].Objective)
	if len(runes) != 241 || runes[240] != '…' || strings.HasPrefix(records[0].Objective, " ") {
		t.Fatalf("resolved objective length=%d value=%q", len(runes), records[0].Objective)
	}
}

func TestTaskCommandsRejectMissingIdentifiersAndDelegateExactTargets(t *testing.T) {
	repository := &taskRepository{}
	service := NewService(repository, taskContent{})
	if _, err := service.Cancel(context.Background(), "  "); err == nil {
		t.Fatal("empty cancellation id was accepted")
	}
	if _, err := service.Retry(context.Background(), "  "); err == nil {
		t.Fatal("empty retry id was accepted")
	}
	if _, err := service.Cancel(context.Background(), "task-1"); err != nil || repository.canceled != "task-1" {
		t.Fatalf("cancel target=%q err=%v", repository.canceled, err)
	}
	if _, err := service.Retry(context.Background(), "task-2"); err != nil || repository.retried != "task-2" {
		t.Fatalf("retry target=%q err=%v", repository.retried, err)
	}
}

func TestInspectDistinguishesMissingTask(t *testing.T) {
	if _, err := NewService(&taskRepository{}, taskContent{}).Inspect(context.Background(), "missing"); err == nil {
		t.Fatal("missing task was returned as a valid record")
	}
}
