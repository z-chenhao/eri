// Package observability exposes read-only developer projections. These views
// are rebuildable and never participate in task correctness.
package observability

import (
	"context"
	"encoding/json"
	"time"

	"github.com/z-chenhao/eri/internal/content"
	"github.com/z-chenhao/eri/internal/eventlog"
	"github.com/z-chenhao/eri/internal/execution"
	"github.com/z-chenhao/eri/internal/memory"
)

type RunSummary struct {
	ID           string    `json:"id"`
	TaskID       string    `json:"task_id"`
	Status       string    `json:"status"`
	SoulVersion  string    `json:"soul_version"`
	StartedAt    time.Time `json:"started_at"`
	EndedAt      time.Time `json:"ended_at,omitempty"`
	ModelCalls   int       `json:"model_calls"`
	ToolCalls    int       `json:"tool_calls"`
	InputTokens  int       `json:"input_tokens"`
	OutputTokens int       `json:"output_tokens"`
	Errors       int       `json:"errors"`
}

type ModelExecution struct {
	ID              string                    `json:"id"`
	Status          string                    `json:"status"`
	Target          string                    `json:"target"`
	ContextManifest execution.ContextManifest `json:"context_manifest"`
	Usage           map[string]any            `json:"usage"`
	ErrorCode       string                    `json:"error_code,omitempty"`
	CreatedAt       time.Time                 `json:"created_at"`
	UpdatedAt       time.Time                 `json:"updated_at"`
}

type Effect struct {
	ID             string        `json:"id"`
	InvocationID   string        `json:"invocation_id"`
	ToolCallID     string        `json:"tool_call_id"`
	ParentIntentID string        `json:"parent_intent_id,omitempty"`
	ToolID         string        `json:"tool_id"`
	Effect         string        `json:"effect"`
	Target         string        `json:"target"`
	Control        string        `json:"control"`
	ApprovalID     string        `json:"approval_id,omitempty"`
	Status         string        `json:"status"`
	ErrorCode      string        `json:"error_code,omitempty"`
	CreatedAt      time.Time     `json:"created_at"`
	UpdatedAt      time.Time     `json:"updated_at"`
	PayloadRef     content.Ref   `json:"-"`
	ResultRef      content.Ref   `json:"-"`
	Exchange       *CallExchange `json:"-"`
}

type Artifact struct {
	ID               string      `json:"id"`
	Version          int         `json:"version"`
	Kind             string      `json:"kind"`
	Status           string      `json:"status"`
	EvalID           string      `json:"eval_id,omitempty"`
	Eval             string      `json:"eval"`
	EvalTier         string      `json:"eval_tier"`
	EvalEvaluator    string      `json:"eval_evaluator"`
	EvalFindings     []string    `json:"eval_findings"`
	EvalFindingsRef  content.Ref `json:"-"`
	TraceRef         content.Ref `json:"-"`
	EvalFindingCount int         `json:"eval_finding_count"`
	DeliveryID       string      `json:"delivery_id,omitempty"`
	Delivery         string      `json:"delivery"`
	Receipt          string      `json:"receipt"`
}

type RunDetail struct {
	Run        RunSummary       `json:"run"`
	Model      ModelExecution   `json:"model"`
	Effects    []Effect         `json:"effects"`
	Artifacts  []Artifact       `json:"artifacts"`
	Events     []eventlog.Event `json:"events"`
	Spans      []RunSpan        `json:"spans"`
	loopTrace  persistedRunTrace
	activeTurn *persistedActiveTurn
}

type Repository interface {
	ListRuns(context.Context, int) ([]RunSummary, error)
	LoadRun(context.Context, string) (RunDetail, bool, error)
	LoadActiveRunTrace(context.Context, string) (content.Ref, bool, error)
	InspectMemory(context.Context, int) ([]memory.Candidate, error)
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

func (s *Service) Runs(ctx context.Context, limit int) ([]RunSummary, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	return s.repository.ListRuns(ctx, limit)
}

func (s *Service) Run(ctx context.Context, id string) (RunDetail, bool, error) {
	detail, found, err := s.repository.LoadRun(ctx, id)
	if err != nil || !found {
		return detail, found, err
	}
	if s.content != nil {
		for index := range detail.Artifacts {
			ref := detail.Artifacts[index].EvalFindingsRef
			if ref.ObjectID == "" {
				continue
			}
			body, err := s.content.Get(ctx, ref)
			if err != nil {
				return RunDetail{}, false, err
			}
			if err := json.Unmarshal(body, &detail.Artifacts[index].EvalFindings); err != nil {
				return RunDetail{}, false, err
			}
		}
	}
	if err := s.hydrateLoopTrace(ctx, &detail); err != nil {
		return RunDetail{}, false, err
	}
	if err := s.hydrateEffectExchanges(ctx, &detail); err != nil {
		return RunDetail{}, false, err
	}
	detail.Spans, err = s.buildRunSpans(ctx, detail, memoryExposureDeveloper)
	if err != nil {
		return RunDetail{}, false, err
	}
	return detail, true, nil
}
