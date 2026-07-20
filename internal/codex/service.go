// Package codex integrates the user's local Codex installation as one
// registered out-of-process subagent provider. It never delivers to the user
// directly; its result returns to primary Eri as untrusted task evidence.
package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/z-chenhao/eri/internal/content"
	"github.com/z-chenhao/eri/internal/observability"
	"github.com/z-chenhao/eri/internal/policy"
	"github.com/z-chenhao/eri/internal/runtime"
	"github.com/z-chenhao/eri/internal/secret"
	"github.com/z-chenhao/eri/internal/subagent"
)

type Mode = subagent.AccessMode

const (
	ReadOnly       Mode = subagent.ReadOnly
	WorkspaceWrite Mode = subagent.WorkspaceWrite
)

type DelegationRequest struct {
	DelegationID string
	TaskID       string
	RunID        string
	Objective    string
	Context      string
	Mode         Mode
}

type Result = subagent.Result

type Repository interface {
	QueueSubagentRun(context.Context, subagent.Run) (subagent.Run, bool, error)
	LoadSubagentRun(context.Context, string) (subagent.Run, bool, error)
	MarkSubagentRunStarting(context.Context, string) error
	MarkSubagentRunRunning(context.Context, string, string) error
	SubagentRunCancellationRequested(context.Context, string) (bool, error)
	CompleteSubagentRun(context.Context, string, string, string, content.Ref) (bool, error)
}

type ContentStore interface {
	Put(context.Context, []byte, content.Metadata) (content.Ref, error)
	Get(context.Context, content.Ref) ([]byte, error)
	Delete(context.Context, content.Ref) error
}

type RunRequest struct {
	ID          string
	Prompt      string
	Mode        Mode
	Workspace   string
	PID         int
	StartedAt   time.Time
	ResultLimit int64
}

type Runner interface {
	Run(context.Context, RunRequest, func(int) error, func(context.Context) (bool, error)) (Result, error)
	Recover(context.Context, RunRequest, func(context.Context) (bool, error)) (Result, error)
}

type artifactCleaner interface {
	EraseArtifacts(context.Context) error
}

var (
	ErrCanceled = errors.New("Codex delegation canceled")
	ErrTimeout  = errors.New("Codex delegation timed out")
	ErrUnknown  = errors.New("Codex delegation process outcome is unknown")
)

type Service struct {
	repository Repository
	content    ContentStore
	runner     Runner
	workspace  string
}

func NewService(repository Repository, contentStore ContentStore, runner Runner, workspace string) (*Service, error) {
	if repository == nil || contentStore == nil || runner == nil {
		return nil, fmt.Errorf("Codex repository, content store and runner are required")
	}
	if strings.TrimSpace(workspace) == "" {
		return nil, fmt.Errorf("Codex workspace is required")
	}
	return &Service{repository: repository, content: contentStore, runner: runner, workspace: workspace}, nil
}

func (*Service) Descriptor() subagent.ProviderDescriptor {
	return subagent.ProviderDescriptor{
		ID:             "codex",
		SupportedRoles: []string{"engineering_team"},
		Execution:      subagent.Background,
		Capabilities: []subagent.Capability{
			{ID: "workspace_analysis", Description: "Inspect repository code, documentation, tests, and local project evidence."},
			{ID: "workspace_implementation", Description: "Make reversible workspace changes when workspace_write is explicitly authorized."},
			{ID: "verification", Description: "Run bounded local checks and return structured evidence."},
		},
		AccessModes: []subagent.AccessMode{subagent.ReadOnly, subagent.WorkspaceWrite}, DefaultAccess: subagent.ReadOnly,
		SendsDataExternally: true,
		Boundaries: []subagent.Boundary{
			{ID: "workspace_only", Description: "Write authority, when granted, is limited to reversible changes inside the configured workspace."},
			{ID: "no_user_contact", Description: "Cannot ask, notify, or deliver to the user."},
			{ID: "no_authority_escalation", Description: "Cannot approve actions or expand the parent task's authority."},
			{ID: "no_recursive_delegation", Description: "Cannot delegate to another agent."},
			{ID: "no_memory_or_delivery", Description: "Cannot write Eri Memory or bypass Eval, Outbox, and Receipt."},
			{ID: "no_git_or_external_side_effects", Description: "Cannot commit, push, deploy, change credentials, or perform external communication."},
		},
	}
}

func (s *Service) Prepare(_ context.Context, request subagent.Request) (subagent.Request, policy.Action, error) {
	if request.Access == "" {
		request.Access = subagent.ReadOnly
	}
	action := policy.Action{
		Effect: policy.ReadOnly, Target: "subagent:codex:" + string(request.Access), SendsDataExternally: true,
	}
	switch request.Access {
	case subagent.ReadOnly:
	case subagent.WorkspaceWrite:
		action.Effect = policy.Reversible
		action.OverwritesExisting = true
	default:
		return subagent.Request{}, policy.Action{}, fmt.Errorf("Codex Provider access must be read_only or workspace_write")
	}
	return request, action, nil
}

func (s *Service) Invoke(ctx context.Context, request subagent.Request) (subagent.Outcome, error) {
	ticket, err := s.queue(ctx, DelegationRequest{
		DelegationID: request.DelegationID, TaskID: request.TaskID, RunID: request.RunID,
		Objective: request.Objective, Context: request.Context, Mode: request.Access,
	})
	if err != nil {
		return subagent.Outcome{}, err
	}
	return subagent.Outcome{
		Ticket: &ticket, ExternalObjectID: ticket.DelegationID,
		Receipt: "subagent:codex:" + ticket.DelegationID + ":queued", FreshAt: time.Now().UTC(), Deferred: true,
	}, nil
}

func (s *Service) Inspect(ctx context.Context, id string) (subagent.Inspection, error) {
	ticket, found, err := s.inspectTicket(ctx, id)
	if err != nil {
		return subagent.Inspection{Status: subagent.InspectionUnknown, ErrorCode: "codex_subagent_inspection_failed", Retry: true}, err
	}
	if !found {
		return subagent.Inspection{Status: subagent.InspectionFailed, ErrorCode: "codex_subagent_not_queued"}, nil
	}
	return subagent.Inspection{Status: subagent.InspectionConfirmed, Outcome: subagent.Outcome{
		Ticket: &ticket, ExternalObjectID: ticket.DelegationID,
		Receipt: "subagent:codex:" + ticket.DelegationID + ":queued", FreshAt: time.Now().UTC(), Deferred: true,
	}}, nil
}

func (s *Service) queue(ctx context.Context, request DelegationRequest) (subagent.Ticket, error) {
	request.Objective = strings.TrimSpace(request.Objective)
	request.Context = strings.TrimSpace(request.Context)
	if request.DelegationID == "" || request.TaskID == "" || request.RunID == "" || request.Objective == "" {
		return subagent.Ticket{}, fmt.Errorf("delegation id, task id, run id and objective are required")
	}
	if len([]byte(request.Objective)) > 32*1024 || len([]byte(request.Context)) > 64*1024 {
		return subagent.Ticket{}, fmt.Errorf("Codex delegation objective/context exceeds its bounded contract")
	}
	if request.Mode == "" {
		request.Mode = ReadOnly
	}
	if request.Mode != ReadOnly && request.Mode != WorkspaceWrite {
		return subagent.Ticket{}, fmt.Errorf("Codex delegation mode must be read_only or workspace_write")
	}
	prompt := subagentPrompt(request.Objective, request.Context, request.Mode)
	ref, err := s.content.Put(ctx, []byte(prompt), content.Metadata{
		MediaType: "text/plain; charset=utf-8", EncryptionDomain: "codex_delegation",
		PrivacyClass: "private", RetentionPolicy: "until_task_complete", ProvenanceRef: request.DelegationID,
	})
	if err != nil {
		return subagent.Ticket{}, fmt.Errorf("store Codex delegation prompt: %w", err)
	}
	job, created, err := s.repository.QueueSubagentRun(ctx, subagent.Run{
		ID: request.DelegationID, ParentTaskID: request.TaskID, ParentRunID: request.RunID,
		RoleID: "engineering_team", ProviderID: "codex", Access: request.Mode, Status: "queued", RequestRef: ref,
	})
	if err != nil {
		_ = s.content.Delete(context.Background(), ref)
		return subagent.Ticket{}, err
	}
	if !created {
		_ = s.content.Delete(context.Background(), ref)
	}
	return ticket(job), nil
}

func (s *Service) inspectTicket(ctx context.Context, id string) (subagent.Ticket, bool, error) {
	job, found, err := s.repository.LoadSubagentRun(ctx, id)
	if err != nil || !found {
		return subagent.Ticket{}, found, err
	}
	return ticket(job), true, nil
}

func (s *Service) ExportUserData(context.Context) (map[string][]byte, error) {
	// Durable delegation metadata is already in the main SQLite export. Codex
	// process artifacts are rebuildable runtime files and are not exported.
	return map[string][]byte{}, nil
}

func (s *Service) EraseUserData(ctx context.Context) error {
	if cleaner, ok := s.runner.(artifactCleaner); ok {
		return cleaner.EraseArtifacts(ctx)
	}
	return nil
}

func ticket(job subagent.Run) subagent.Ticket {
	return subagent.Ticket{
		DelegationID: job.ID, RoleID: job.RoleID, ProviderID: job.ProviderID, Status: job.Status,
		Execution: subagent.Background, Access: job.Access,
	}
}

func (s *Service) HandleRun(ctx context.Context, item runtime.OutboxItem) error {
	job, found, err := s.repository.LoadSubagentRun(ctx, item.AggregateID)
	if err != nil || !found {
		return err
	}
	if job.ProviderID != "codex" || job.RoleID != "engineering_team" {
		return fmt.Errorf("Codex provider cannot run assignment %q through provider %q", job.RoleID, job.ProviderID)
	}
	if canceled, cancelErr := s.repository.SubagentRunCancellationRequested(ctx, job.ID); cancelErr == nil && canceled {
		return s.finish(ctx, job, Result{}, "canceled", "user_canceled")
	}
	freshStart := false
	switch job.Status {
	case "completed", "failed", "unknown", "canceled":
		return nil
	case "queued":
		if err := s.repository.MarkSubagentRunStarting(ctx, job.ID); err != nil {
			return err
		}
		job.Status = "starting"
		freshStart = true
	case "starting":
		if job.RuntimeID == "" {
			return s.finish(ctx, job, Result{}, "unknown", "codex_process_outcome_unknown")
		}
	case "running":
	default:
		return fmt.Errorf("unsupported Codex delegation status %q", job.Status)
	}
	prompt, err := s.content.Get(ctx, job.RequestRef)
	if err != nil {
		return s.finish(ctx, job, Result{}, "failed", "codex_prompt_unavailable")
	}
	pid := 0
	if job.RuntimeID != "" {
		pid, err = strconv.Atoi(job.RuntimeID)
		if err != nil || pid <= 0 {
			return s.finish(ctx, job, Result{}, "unknown", "codex_runtime_id_invalid")
		}
	}
	run := RunRequest{
		ID: job.ID, Prompt: string(prompt), Mode: job.Access, Workspace: s.workspace,
		PID: pid, StartedAt: job.StartedAt, ResultLimit: 1024 * 1024,
	}
	canceled := func(checkCtx context.Context) (bool, error) {
		return s.repository.SubagentRunCancellationRequested(checkCtx, job.ID)
	}
	var result Result
	if freshStart {
		result, err = s.runner.Run(ctx, run, func(pid int) error {
			return s.repository.MarkSubagentRunRunning(context.Background(), job.ID, strconv.Itoa(pid))
		}, canceled)
	} else {
		result, err = s.runner.Recover(ctx, run, canceled)
	}
	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, ErrCanceled) {
		return s.finish(ctx, job, Result{}, "canceled", "user_canceled")
	}
	if errors.Is(err, ErrUnknown) {
		return s.finish(ctx, job, Result{}, "unknown", "codex_process_outcome_unknown")
	}
	if errors.Is(err, ErrTimeout) {
		return s.finish(ctx, job, Result{}, "failed", "codex_timeout")
	}
	if err != nil {
		return s.finish(ctx, job, Result{}, "failed", "codex_process_failed")
	}
	if result.Status == "blocked" {
		return s.finish(ctx, job, result, "failed", "codex_blocked")
	}
	return s.finish(ctx, job, result, "completed", "")
}

func (s *Service) finish(ctx context.Context, job subagent.Run, result Result, status, code string) error {
	result.DelegationID = job.ID
	result.RoleID = job.RoleID
	result.ProviderID = job.ProviderID
	result.Status = status
	result.ErrorCode = code
	if result.Evidence == nil {
		result.Evidence = []string{}
	}
	if result.Changes == nil {
		result.Changes = []string{}
	}
	if result.Tests == nil {
		result.Tests = []string{}
	}
	if result.RemainingRisk == nil {
		result.RemainingRisk = []string{}
	}
	if result.Summary == "" {
		switch status {
		case "canceled":
			result.Summary = "The delegated Codex task was canceled before a result was accepted."
		case "unknown":
			result.Summary = "The Codex process ended without a trustworthy completion signal; workspace effects may require inspection."
		default:
			result.Summary = "The delegated Codex task did not produce a usable result."
		}
	}
	body, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if secret.LooksLikeCredential(body) {
		result = Result{
			DelegationID: job.ID, RoleID: job.RoleID, ProviderID: job.ProviderID, Status: "failed", Summary: "The engineering result was withheld because it appeared to contain a credential.",
			Evidence: []string{}, Changes: []string{}, Tests: []string{}, RemainingRisk: []string{}, ErrorCode: "credential_in_codex_result",
		}
		body, _ = json.Marshal(result)
		status = "failed"
		code = result.ErrorCode
	}
	ref, err := s.content.Put(ctx, body, content.Metadata{
		MediaType: "application/json", EncryptionDomain: "codex_delegation",
		PrivacyClass: "private", RetentionPolicy: "user_owned", ProvenanceRef: job.ID,
	})
	if err != nil {
		return fmt.Errorf("store Codex result: %w", err)
	}
	accepted, err := s.repository.CompleteSubagentRun(ctx, job.ID, status, code, ref)
	if err != nil {
		_ = s.content.Delete(context.Background(), ref)
		return fmt.Errorf("commit Codex result (%s): %w", observability.SafeText(code, 80), err)
	}
	if !accepted {
		_ = s.content.Delete(context.Background(), ref)
	}
	return nil
}

func subagentPrompt(objective, scopedContext string, mode Mode) string {
	var body strings.Builder
	body.WriteString("You are a private Codex External Agent working for Eri. Complete only the delegated objective in the supplied workspace. Respect all applicable AGENTS.md instructions. Do not speak to the user, request interactive approval, delegate to another agent, change Eri's Soul, or claim an unverified effect. Keep credentials and private reasoning out of the result. Return only the required structured final result.\n\n")
	body.WriteString("Delegated objective:\n")
	body.WriteString(objective)
	if scopedContext != "" {
		body.WriteString("\n\nMinimum scoped context from primary Eri:\n")
		body.WriteString(scopedContext)
	}
	if mode == ReadOnly {
		body.WriteString("\n\nThis is a read-only delegation. Inspect and analyze, but do not modify workspace files.")
	} else {
		body.WriteString("\n\nThis delegation is approved only for reversible writes inside the configured workspace. Do not perform external communication, destructive actions, credential changes, privilege changes, git commit/push, or deployment.")
	}
	return body.String()
}
