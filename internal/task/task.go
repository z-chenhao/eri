// Package task exposes Eri's durable commitments for assistant-level
// inspection and cancellation. It is not a user-facing task dashboard.
package task

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/z-chenhao/eri/internal/content"
)

type Record struct {
	ID           string      `json:"id"`
	Status       string      `json:"status"`
	WaitReason   string      `json:"wait_reason,omitempty"`
	ErrorCode    string      `json:"error_code,omitempty"`
	CancelAsked  bool        `json:"cancel_requested"`
	ObjectiveRef content.Ref `json:"-"`
	Objective    string      `json:"objective,omitempty"`
	CreatedAt    time.Time   `json:"created_at"`
	UpdatedAt    time.Time   `json:"updated_at"`
}

type CancelResult struct {
	TaskID string `json:"task_id"`
	Status string `json:"status"`
	Effect string `json:"effect"`
}

type RetryResult struct {
	SourceTaskID string `json:"source_task_id"`
	TaskID       string `json:"task_id"`
	Status       string `json:"status"`
	Checkpoint   string `json:"checkpoint"`
}

type Repository interface {
	ListTasks(context.Context, int) ([]Record, error)
	LoadTask(context.Context, string) (Record, bool, error)
	RequestTaskCancel(context.Context, string) (CancelResult, error)
	RetryTask(context.Context, string) (RetryResult, error)
}

type ContentStore interface {
	Get(context.Context, content.Ref) ([]byte, error)
}

type Service struct {
	repository Repository
	content    ContentStore
}

func NewService(repository Repository, contentStore ContentStore) *Service {
	return &Service{repository: repository, content: contentStore}
}

func (s *Service) List(ctx context.Context, limit int) ([]Record, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	records, err := s.repository.ListTasks(ctx, limit)
	if err != nil {
		return nil, err
	}
	for index := range records {
		if err := s.resolveObjective(ctx, &records[index]); err != nil {
			return nil, err
		}
	}
	return records, nil
}

func (s *Service) Inspect(ctx context.Context, id string) (Record, error) {
	record, found, err := s.repository.LoadTask(ctx, id)
	if err != nil {
		return Record{}, err
	}
	if !found {
		return Record{}, fmt.Errorf("task not found")
	}
	if err := s.resolveObjective(ctx, &record); err != nil {
		return Record{}, err
	}
	return record, nil
}

func (s *Service) Cancel(ctx context.Context, id string) (CancelResult, error) {
	if strings.TrimSpace(id) == "" {
		return CancelResult{}, fmt.Errorf("task id is required")
	}
	return s.repository.RequestTaskCancel(ctx, id)
}

func (s *Service) Retry(ctx context.Context, id string) (RetryResult, error) {
	if strings.TrimSpace(id) == "" {
		return RetryResult{}, fmt.Errorf("task id is required")
	}
	return s.repository.RetryTask(ctx, id)
}

func (s *Service) resolveObjective(ctx context.Context, record *Record) error {
	body, err := s.content.Get(ctx, record.ObjectiveRef)
	if err != nil {
		return fmt.Errorf("read task objective: %w", err)
	}
	objective := strings.TrimSpace(string(body))
	if runes := []rune(objective); len(runes) > 240 {
		objective = string(runes[:240]) + "…"
	}
	record.Objective = objective
	return nil
}
