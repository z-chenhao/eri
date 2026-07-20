package codex

import (
	"context"
	"testing"
	"time"

	"github.com/z-chenhao/eri/internal/content"
	"github.com/z-chenhao/eri/internal/runtime"
	"github.com/z-chenhao/eri/internal/subagent"
)

type serviceTestRepository struct {
	job            subagent.Run
	completed      string
	acceptedResult bool
}

func (r *serviceTestRepository) QueueSubagentRun(context.Context, subagent.Run) (subagent.Run, bool, error) {
	return r.job, false, nil
}
func (r *serviceTestRepository) LoadSubagentRun(context.Context, string) (subagent.Run, bool, error) {
	return r.job, true, nil
}
func (*serviceTestRepository) MarkSubagentRunStarting(context.Context, string) error { return nil }
func (*serviceTestRepository) MarkSubagentRunRunning(context.Context, string, string) error {
	return nil
}
func (*serviceTestRepository) SubagentRunCancellationRequested(context.Context, string) (bool, error) {
	return false, nil
}
func (r *serviceTestRepository) CompleteSubagentRun(_ context.Context, _ string, status, _ string, _ content.Ref) (bool, error) {
	r.completed = status
	return r.acceptedResult, nil
}

type serviceTestContent struct {
	body    []byte
	deleted int
}

func (s *serviceTestContent) Put(_ context.Context, body []byte, _ content.Metadata) (content.Ref, error) {
	s.body = append([]byte(nil), body...)
	return content.Ref{ObjectID: "0123456789abcdef0123456789abcdef", Version: 1}, nil
}
func (s *serviceTestContent) Get(context.Context, content.Ref) ([]byte, error) {
	return append([]byte(nil), s.body...), nil
}
func (s *serviceTestContent) Delete(context.Context, content.Ref) error {
	s.deleted++
	return nil
}

type serviceTestRunner struct{ runs int }

func (r *serviceTestRunner) Run(context.Context, RunRequest, func(int) error, func(context.Context) (bool, error)) (Result, error) {
	r.runs++
	return Result{}, nil
}
func (*serviceTestRunner) Recover(context.Context, RunRequest, func(context.Context) (bool, error)) (Result, error) {
	return Result{}, ErrUnknown
}

func TestServiceDoesNotReplayStartingRunWithoutRuntimeID(t *testing.T) {
	repository := &serviceTestRepository{job: subagent.Run{
		ID: "delegation-1", RoleID: "engineering_team", ProviderID: "codex", ParentTaskID: "task-1", ParentRunID: "run-1",
		Access: subagent.WorkspaceWrite, Status: "starting", RequestRef: content.Ref{ObjectID: "request", Version: 1},
	}}
	contentStore := &serviceTestContent{body: []byte("prompt")}
	runner := &serviceTestRunner{}
	service, err := NewService(repository, contentStore, runner, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := service.HandleRun(context.Background(), runtime.OutboxItem{AggregateID: "delegation-1"}); err != nil {
		t.Fatal(err)
	}
	if runner.runs != 0 || repository.completed != "unknown" {
		t.Fatalf("runs=%d terminal=%q", runner.runs, repository.completed)
	}
}

func TestServiceDeletesUnacceptedDuplicateResult(t *testing.T) {
	repository := &serviceTestRepository{acceptedResult: false}
	contentStore := &serviceTestContent{}
	service, err := NewService(repository, contentStore, &serviceTestRunner{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	job := subagent.Run{ID: "delegation-2", RoleID: "engineering_team", ProviderID: "codex", Access: subagent.ReadOnly, CreatedAt: time.Now()}
	if err := service.finish(context.Background(), job, Result{Summary: "done"}, "completed", ""); err != nil {
		t.Fatal(err)
	}
	if contentStore.deleted != 1 {
		t.Fatalf("deleted duplicate results = %d", contentStore.deleted)
	}
}
