// Package tool owns Eri's single invocation gateway. Every built-in and
// plugin tool passes through the same validation, policy, intent and receipt
// boundary.
package tool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/z-chenhao/eri/internal/content"
	"github.com/z-chenhao/eri/internal/identifier"
	"github.com/z-chenhao/eri/internal/policy"
	"github.com/z-chenhao/eri/internal/secret"
)

type Source string

const (
	BuiltIn Source = "builtin"
	Plugin  Source = "plugin"
)

type Descriptor struct {
	ID                     string               `json:"id"`
	Version                string               `json:"version"`
	Purpose                string               `json:"purpose"`
	InputSchema            map[string]any       `json:"input_schema"`
	OutputSchema           map[string]any       `json:"output_schema"`
	AllowedEffects         []policy.EffectClass `json:"allowed_effects"`
	PermissionRequirements []string             `json:"permission_requirements"`
	SendsDataExternally    bool                 `json:"sends_data_externally"`
	Timeout                time.Duration        `json:"timeout"`
	CostPolicy             string               `json:"cost_policy"`
	Idempotency            string               `json:"idempotency"`
	Reconciliation         string               `json:"reconciliation"`
	Source                 Source               `json:"source"`
}

type Prepared struct {
	Input               json.RawMessage
	Action              policy.Action
	TaskID              string
	RunID               string
	InvocationID        string
	SourceInteractionID string
}

type Result struct {
	Output           json.RawMessage `json:"output"`
	Source           string          `json:"source"`
	ExternalObjectID string          `json:"external_object_id,omitempty"`
	Receipt          string          `json:"receipt"`
	FreshAt          time.Time       `json:"fresh_at"`
	Uncertainty      []string        `json:"uncertainty"`
	Attachments      []Attachment    `json:"attachments,omitempty"`
	Deferred         *Deferred       `json:"deferred,omitempty"`
}

// Deferred identifies a durable result that will arrive through the Runtime
// after the current model turn. It is a control fact, never proof of completion.
type Deferred struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Type       string `json:"type,omitempty"`
	ProviderID string `json:"-"`
}

type Attachment struct {
	ID         string      `json:"id"`
	Name       string      `json:"name"`
	MediaType  string      `json:"media_type"`
	SizeBytes  int64       `json:"size_bytes"`
	ContentRef content.Ref `json:"content_ref"`
}

type Tool interface {
	Descriptor() Descriptor
	Prepare(context.Context, json.RawMessage) (Prepared, error)
	Execute(context.Context, Prepared) (Result, error)
}

// Reconciler is implemented by tools that can query the external system after
// dispatch returned an ambiguous outcome. It must inspect; it must never issue
// the original mutation again.
type Reconciler interface {
	Reconcile(context.Context, ReconcileRequest) (ReconcileResult, error)
}

type ReconcileRequest struct {
	Intent  Intent
	Payload json.RawMessage
}

type ReconcileResult struct {
	Status    IntentStatus
	Result    Result
	ErrorCode string
	Retry     bool
}

// ErrStaleTaskInput prevents a tool effect from being planned against an Agent
// turn that no longer includes the newest user message.
var ErrStaleTaskInput = errors.New("task input changed")

// ErrStaleConversationContext prevents a new effect from being planned after
// another Task advanced the authoritative Conversation.
var ErrStaleConversationContext = errors.New("conversation context changed")

type IntentStatus string

const (
	IntentPlanned     IntentStatus = "planned"
	IntentAuthorized  IntentStatus = "authorized"
	IntentDispatched  IntentStatus = "dispatched"
	IntentConfirmed   IntentStatus = "confirmed"
	IntentFailed      IntentStatus = "failed"
	IntentUnknown     IntentStatus = "unknown"
	IntentCompensated IntentStatus = "compensated"
)

type Intent struct {
	ID                        string
	TaskID                    string
	RunID                     string
	InvocationID              string
	ToolCallID                string
	BasisInputSequence        int64
	BasisConversationSequence int64
	ParentIntentID            string
	ToolID                    string
	ToolVersion               string
	Effect                    policy.EffectClass
	Target                    string
	ParametersHash            string
	PayloadRef                content.Ref
	IdempotencyKey            string
	Control                   policy.ControlLevel
	ApprovalID                string
	GrantID                   string
	ReconciliationStrategy    string
	Status                    IntentStatus
	ResultRef                 content.Ref
	ErrorCode                 string
	CreatedAt                 time.Time
	UpdatedAt                 time.Time
}

type IntentStore interface {
	PlanIntent(context.Context, Intent) (Intent, bool, error)
	TransitionIntent(context.Context, string, IntentStatus, IntentStatus, string, string, string, content.Ref) error
	LoadIntentByID(context.Context, string) (Intent, bool, error)
	RecordReconciliationAttempt(context.Context, string, string, string) error
}

type ContentStore interface {
	Put(context.Context, []byte, content.Metadata) (content.Ref, error)
	Get(context.Context, content.Ref) ([]byte, error)
	Delete(context.Context, content.Ref) error
}

type Grant struct {
	ID             string
	ApprovalID     string
	TaskID         string
	ToolID         string
	ToolVersion    string
	Effect         policy.EffectClass
	Target         string
	ParametersHash string
	Control        policy.ControlLevel
	ExpiresAt      time.Time
}

type Request struct {
	TaskID                    string
	RunID                     string
	InvocationID              string
	SourceInteractionID       string
	ToolCallID                string
	BasisInputSequence        int64
	BasisConversationSequence int64
	ParentIntentID            string
	ToolID                    string
	Input                     json.RawMessage
	MinimumControl            policy.ControlLevel
	Grant                     *Grant
	Scope                     *CapabilityScope
}

// CapabilityScope is a Runtime-enforced ceiling for a restricted Agent Loop.
// It is checked after Tool.Prepare has classified the concrete action and
// before any effect payload or Intent is persisted.
type CapabilityScope struct {
	AllowedToolIDs map[string]struct{}
	AllowedEffects map[policy.EffectClass]struct{}
	AllowApproval  bool
}

type Outcome struct {
	Intent           Intent
	Control          policy.ControlLevel
	ApprovalRequired bool
	Result           Result
	Replayed         bool
}

type Gateway struct {
	mu      sync.RWMutex
	store   IntentStore
	content ContentStore
	tools   map[string]Tool
	now     func() time.Time
}

func NewGateway(store IntentStore, contentStore ContentStore, available ...Tool) (*Gateway, error) {
	if store == nil {
		return nil, fmt.Errorf("intent store is required")
	}
	if contentStore == nil {
		return nil, fmt.Errorf("encrypted result store is required")
	}
	gateway := &Gateway{store: store, content: contentStore, tools: make(map[string]Tool), now: time.Now}
	for _, candidate := range available {
		if candidate == nil {
			return nil, fmt.Errorf("tool is nil")
		}
		descriptor := candidate.Descriptor()
		if descriptor.ID == "" || descriptor.Version == "" || descriptor.Purpose == "" || descriptor.Source == "" {
			return nil, fmt.Errorf("tool descriptor requires id, version, purpose and source")
		}
		if _, exists := gateway.tools[descriptor.ID]; exists {
			return nil, fmt.Errorf("duplicate tool id %q", descriptor.ID)
		}
		gateway.tools[descriptor.ID] = candidate
	}
	return gateway, nil
}

func (g *Gateway) Descriptors() []Descriptor {
	g.mu.RLock()
	defer g.mu.RUnlock()
	descriptors := make([]Descriptor, 0, len(g.tools))
	for _, candidate := range g.tools {
		descriptors = append(descriptors, candidate.Descriptor())
	}
	sort.Slice(descriptors, func(i, j int) bool { return descriptors[i].ID < descriptors[j].ID })
	return descriptors
}

// RegisterBuiltIn adds a composition-time capability after the gateway exists.
// It is used for capabilities such as delegation that themselves need the
// already-constructed gateway. Runtime plugin replacement remains separately
// namespaced and atomic.
func (g *Gateway) RegisterBuiltIn(candidate Tool) error {
	if candidate == nil {
		return fmt.Errorf("tool is nil")
	}
	descriptor := candidate.Descriptor()
	if descriptor.Source != BuiltIn || descriptor.ID == "" || descriptor.Version == "" || descriptor.Purpose == "" {
		return fmt.Errorf("built-in tool descriptor is incomplete")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, exists := g.tools[descriptor.ID]; exists {
		return fmt.Errorf("duplicate tool id %q", descriptor.ID)
	}
	g.tools[descriptor.ID] = candidate
	return nil
}

// ReplacePluginTools atomically refreshes one out-of-process plugin's model
// surface. In-flight calls keep their already-resolved Tool instance; future
// turns see only the new version.
func (g *Gateway) ReplacePluginTools(namespace string, available []Tool) error {
	prefix := "mcp." + strings.TrimSpace(namespace) + "."
	if namespace == "" {
		return fmt.Errorf("plugin namespace is required")
	}
	replacements := make(map[string]Tool, len(available))
	for _, candidate := range available {
		if candidate == nil {
			return fmt.Errorf("plugin tool is nil")
		}
		descriptor := candidate.Descriptor()
		if descriptor.Source != Plugin || !strings.HasPrefix(descriptor.ID, prefix) {
			return fmt.Errorf("tool %q does not belong to plugin %q", descriptor.ID, namespace)
		}
		if descriptor.Version == "" || descriptor.Purpose == "" {
			return fmt.Errorf("plugin tool %q has an incomplete descriptor", descriptor.ID)
		}
		if _, duplicate := replacements[descriptor.ID]; duplicate {
			return fmt.Errorf("duplicate plugin tool %q", descriptor.ID)
		}
		replacements[descriptor.ID] = candidate
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for id := range g.tools {
		if strings.HasPrefix(id, prefix) {
			delete(g.tools, id)
		}
	}
	for id, candidate := range replacements {
		if _, exists := g.tools[id]; exists {
			return fmt.Errorf("plugin tool %q conflicts with an existing tool", id)
		}
		g.tools[id] = candidate
	}
	return nil
}

func (g *Gateway) Invoke(ctx context.Context, request Request) (Outcome, error) {
	if request.TaskID == "" || request.RunID == "" {
		return Outcome{}, fmt.Errorf("task and run are required")
	}
	g.mu.RLock()
	candidate, found := g.tools[request.ToolID]
	g.mu.RUnlock()
	if !found {
		return Outcome{}, fmt.Errorf("tool %q is not available", request.ToolID)
	}
	if request.Scope != nil {
		if _, allowed := request.Scope.AllowedToolIDs[request.ToolID]; !allowed {
			return Outcome{}, fmt.Errorf("tool %q is outside this agent's capability scope", request.ToolID)
		}
	}
	if secret.LooksLikeCredential(request.Input) {
		return Outcome{}, fmt.Errorf("tool input appears to contain a credential; use an ephemeral authentication surface")
	}
	descriptor := candidate.Descriptor()
	prepared, err := candidate.Prepare(ctx, request.Input)
	if err != nil {
		return Outcome{}, fmt.Errorf("validate %s input: %w", descriptor.ID, err)
	}
	prepared.SourceInteractionID = request.SourceInteractionID
	if !allowsEffect(descriptor.AllowedEffects, prepared.Action.Effect) {
		return Outcome{}, fmt.Errorf("tool %s does not declare effect %q", descriptor.ID, prepared.Action.Effect)
	}
	if request.Scope != nil {
		if _, allowed := request.Scope.AllowedEffects[prepared.Action.Effect]; !allowed {
			return Outcome{}, fmt.Errorf("tool %s action %q exceeds this agent's effect ceiling", descriptor.ID, prepared.Action.Effect)
		}
	}
	prepared.Action.SendsDataExternally = prepared.Action.SendsDataExternally || descriptor.SendsDataExternally
	assessment, err := policy.Floor(prepared.Action)
	if err != nil {
		return Outcome{}, err
	}
	if request.MinimumControl != "" {
		assessment.Control = policy.Max(assessment.Control, request.MinimumControl)
	}
	if request.Scope != nil && !request.Scope.AllowApproval &&
		(assessment.Control == policy.OrdinaryConfirm || assessment.Control == policy.StrongApproval) {
		return Outcome{}, fmt.Errorf("tool %s requires approval unavailable to this agent", descriptor.ID)
	}
	parametersHash := hashBytes(prepared.Input)
	now := g.now().UTC()
	intentID, err := identifier.New()
	if err != nil {
		return Outcome{}, err
	}
	payloadRef, err := g.content.Put(ctx, prepared.Input, content.Metadata{
		MediaType: "application/json", EncryptionDomain: "effect_payload", PrivacyClass: "private",
		RetentionPolicy: "until_task_complete", ProvenanceRef: intentID,
	})
	if err != nil {
		return Outcome{}, fmt.Errorf("encrypt effect payload: %w", err)
	}
	intent := Intent{
		ID: intentID, TaskID: request.TaskID, RunID: request.RunID, InvocationID: request.InvocationID,
		ToolCallID: request.ToolCallID, BasisInputSequence: request.BasisInputSequence,
		BasisConversationSequence: request.BasisConversationSequence, ParentIntentID: request.ParentIntentID,
		ToolID: descriptor.ID, ToolVersion: descriptor.Version,
		Effect: prepared.Action.Effect, Target: prepared.Action.Target,
		ParametersHash: parametersHash, PayloadRef: payloadRef,
		IdempotencyKey: hashBytes([]byte(request.TaskID + "\x00" + request.RunID + "\x00" + request.ParentIntentID + "\x00" + descriptor.ID + "\x00" + descriptor.Version + "\x00" + parametersHash)),
		Control:        assessment.Control, ReconciliationStrategy: descriptor.Reconciliation,
		Status: IntentPlanned, CreatedAt: now, UpdatedAt: now,
	}
	persisted, created, err := g.store.PlanIntent(ctx, intent)
	if err != nil {
		_ = g.content.Delete(context.Background(), payloadRef)
		return Outcome{}, fmt.Errorf("persist effect intent: %w", err)
	}
	if !created {
		_ = g.content.Delete(context.Background(), payloadRef)
		if persisted.Status == IntentAuthorized && (persisted.GrantID == "" || validGrant(request.Grant, persisted, now)) {
			return g.executeAuthorized(ctx, persisted, persisted.Control, candidate, prepared, descriptor)
		}
		if persisted.Status != IntentPlanned || (persisted.Control != policy.OrdinaryConfirm && persisted.Control != policy.StrongApproval) || !validGrant(request.Grant, persisted, now) {
			return g.replayOutcome(ctx, persisted)
		}
		intent = persisted
	} else {
		intent = persisted
	}
	if assessment.Control == policy.Deny {
		if err := g.store.TransitionIntent(ctx, intent.ID, IntentPlanned, IntentFailed, "policy_denied", "", "", content.Ref{}); err != nil {
			return Outcome{}, err
		}
		intent.Status = IntentFailed
		intent.ErrorCode = "policy_denied"
		return Outcome{Intent: intent, Control: assessment.Control}, fmt.Errorf("policy denied tool invocation")
	}
	if assessment.Control == policy.OrdinaryConfirm || assessment.Control == policy.StrongApproval {
		if !validGrant(request.Grant, intent, now) {
			return Outcome{Intent: intent, Control: assessment.Control, ApprovalRequired: true}, nil
		}
		intent.ApprovalID = request.Grant.ApprovalID
		intent.GrantID = request.Grant.ID
	}
	approvalID := ""
	if request.Grant != nil {
		approvalID = request.Grant.ApprovalID
	}
	if err := g.store.TransitionIntent(ctx, intent.ID, IntentPlanned, IntentAuthorized, "", approvalID, intent.GrantID, content.Ref{}); err != nil {
		return Outcome{}, err
	}
	intent.Status = IntentAuthorized
	intent.GrantID = grantID(request.Grant)
	return g.executeAuthorized(ctx, intent, assessment.Control, candidate, prepared, descriptor)
}

func (g *Gateway) executeAuthorized(ctx context.Context, intent Intent, control policy.ControlLevel, candidate Tool, prepared Prepared, descriptor Descriptor) (Outcome, error) {
	if err := g.store.TransitionIntent(ctx, intent.ID, IntentAuthorized, IntentDispatched, "", intent.ApprovalID, intent.GrantID, content.Ref{}); err != nil {
		return Outcome{}, err
	}
	intent.Status = IntentDispatched
	// Runtime identity is supplied by the gateway, never trusted from model
	// arguments. Tools that schedule task-bound work can use these fields
	// without exposing internal IDs in their public JSON schema.
	prepared.TaskID = intent.TaskID
	prepared.RunID = intent.RunID
	prepared.InvocationID = intent.ID
	executionContext := ctx
	if descriptor.Timeout > 0 {
		var cancel context.CancelFunc
		executionContext, cancel = context.WithTimeout(ctx, descriptor.Timeout)
		defer cancel()
	}
	result, executeErr := candidate.Execute(executionContext, prepared)
	if executeErr != nil {
		status := IntentFailed
		code := "tool_failed"
		var uncertain *ExecutionError
		if errors.As(executeErr, &uncertain) {
			if uncertain.Code != "" {
				code = uncertain.Code
			}
			if uncertain.Unknown {
				status = IntentUnknown
			}
		}
		if err := g.store.TransitionIntent(ctx, intent.ID, IntentDispatched, status, code, intent.ApprovalID, intent.GrantID, content.Ref{}); err != nil {
			return Outcome{}, err
		}
		intent.Status = status
		intent.ErrorCode = code
		return Outcome{Intent: intent, Control: control}, executeErr
	}
	if result.Receipt == "" {
		return Outcome{}, g.failMissingReceipt(ctx, intent)
	}
	if result.FreshAt.IsZero() {
		result.FreshAt = g.now().UTC()
	}
	result.Source = descriptor.SourceName()
	encodedResult, err := json.Marshal(result)
	if err != nil {
		return Outcome{}, err
	}
	resultRef, err := g.content.Put(ctx, encodedResult, content.Metadata{
		MediaType: "application/json", EncryptionDomain: "tool-result", PrivacyClass: "private",
		RetentionPolicy: "user_owned", ProvenanceRef: intent.ID,
	})
	if err != nil {
		return Outcome{}, fmt.Errorf("encrypt tool result: %w", err)
	}
	if err := g.store.TransitionIntent(ctx, intent.ID, IntentDispatched, IntentConfirmed, "", intent.ApprovalID, intent.GrantID, resultRef); err != nil {
		return Outcome{}, err
	}
	intent.Status = IntentConfirmed
	intent.ResultRef = resultRef
	return Outcome{Intent: intent, Control: control, Result: result}, nil
}

// Reconcile advances an authorized-but-not-dispatched intent or inspects an
// ambiguous dispatched intent. attempt is the durable outbox attempt count;
// after bounded automatic inspection Eri leaves the intent unknown for manual
// review instead of spending resources forever.
func (g *Gateway) Reconcile(ctx context.Context, intentID string, attempt int) error {
	intent, found, err := g.store.LoadIntentByID(ctx, intentID)
	if err != nil || !found {
		return err
	}
	if intent.Status == IntentConfirmed || intent.Status == IntentFailed || intent.Status == IntentCompensated {
		return nil
	}
	g.mu.RLock()
	candidate, available := g.tools[intent.ToolID]
	g.mu.RUnlock()
	if !available || candidate.Descriptor().Version != intent.ToolVersion {
		return g.store.RecordReconciliationAttempt(ctx, intent.ID, "manual", "tool_version_unavailable")
	}
	payload, err := g.content.Get(ctx, intent.PayloadRef)
	if err != nil {
		return g.store.RecordReconciliationAttempt(ctx, intent.ID, "manual", "effect_payload_unavailable")
	}
	if hashBytes(payload) != intent.ParametersHash {
		return g.store.RecordReconciliationAttempt(ctx, intent.ID, "manual", "effect_payload_hash_mismatch")
	}
	if intent.Status == IntentAuthorized {
		if intent.GrantID != "" {
			return g.store.RecordReconciliationAttempt(ctx, intent.ID, "manual", "approved_grant_requires_revalidation")
		}
		prepared, err := candidate.Prepare(ctx, payload)
		if err != nil || prepared.Action.Target != intent.Target || prepared.Action.Effect != intent.Effect {
			return g.store.RecordReconciliationAttempt(ctx, intent.ID, "manual", "effect_payload_no_longer_valid")
		}
		_, err = g.executeAuthorized(ctx, intent, intent.Control, candidate, prepared, candidate.Descriptor())
		return err
	}
	if intent.Status != IntentDispatched && intent.Status != IntentUnknown {
		return g.store.RecordReconciliationAttempt(ctx, intent.ID, "manual", "intent_not_reconcilable")
	}
	if attempt >= 7 {
		return g.store.RecordReconciliationAttempt(ctx, intent.ID, "manual", "automatic_reconciliation_exhausted")
	}
	reconciler, ok := candidate.(Reconciler)
	if !ok {
		return g.store.RecordReconciliationAttempt(ctx, intent.ID, "manual", "tool_has_no_reconciler")
	}
	result, reconcileErr := reconciler.Reconcile(ctx, ReconcileRequest{Intent: intent, Payload: append(json.RawMessage(nil), payload...)})
	if reconcileErr != nil {
		_ = g.store.RecordReconciliationAttempt(ctx, intent.ID, "retry", "reconciler_unavailable")
		return reconcileErr
	}
	switch result.Status {
	case IntentConfirmed:
		if result.Result.Receipt == "" {
			return g.store.RecordReconciliationAttempt(ctx, intent.ID, "manual", "reconciler_missing_receipt")
		}
		if result.Result.FreshAt.IsZero() {
			result.Result.FreshAt = g.now().UTC()
		}
		result.Result.Source = candidate.Descriptor().SourceName()
		encoded, err := json.Marshal(result.Result)
		if err != nil {
			return err
		}
		ref, err := g.content.Put(ctx, encoded, content.Metadata{
			MediaType: "application/json", EncryptionDomain: "tool-result", PrivacyClass: "private",
			RetentionPolicy: "user_owned", ProvenanceRef: intent.ID,
		})
		if err != nil {
			return err
		}
		return g.store.TransitionIntent(ctx, intent.ID, intent.Status, IntentConfirmed, "", intent.ApprovalID, intent.GrantID, ref)
	case IntentFailed:
		code := result.ErrorCode
		if code == "" {
			code = "reconciled_not_executed"
		}
		return g.store.TransitionIntent(ctx, intent.ID, intent.Status, IntentFailed, code, intent.ApprovalID, intent.GrantID, content.Ref{})
	case IntentUnknown:
		if result.Retry {
			_ = g.store.RecordReconciliationAttempt(ctx, intent.ID, "retry", result.ErrorCode)
			return &ExecutionError{Code: "reconciliation_pending", Unknown: true}
		}
		return g.store.RecordReconciliationAttempt(ctx, intent.ID, "manual", result.ErrorCode)
	default:
		return g.store.RecordReconciliationAttempt(ctx, intent.ID, "manual", "invalid_reconciliation_result")
	}
}

func (d Descriptor) SourceName() string { return string(d.Source) + ":" + d.ID + "@" + d.Version }

type ExecutionError struct {
	Code    string
	Unknown bool
	Err     error
}

func (e *ExecutionError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return e.Code
}

func (e *ExecutionError) Unwrap() error { return e.Err }

func (g *Gateway) failMissingReceipt(ctx context.Context, intent Intent) error {
	err := g.store.TransitionIntent(ctx, intent.ID, IntentDispatched, IntentFailed, "missing_receipt", intent.ApprovalID, intent.GrantID, content.Ref{})
	if err != nil {
		return err
	}
	return fmt.Errorf("tool %s returned no receipt", intent.ToolID)
}

func (g *Gateway) replayOutcome(ctx context.Context, intent Intent) (Outcome, error) {
	outcome := Outcome{Intent: intent, Control: intent.Control, Replayed: true}
	switch intent.Status {
	case IntentConfirmed:
		encoded, err := g.content.Get(ctx, intent.ResultRef)
		if err != nil {
			return Outcome{}, fmt.Errorf("read encrypted tool result: %w", err)
		}
		if err := json.Unmarshal(encoded, &outcome.Result); err != nil {
			return Outcome{}, fmt.Errorf("decode persisted tool result: %w", err)
		}
		return outcome, nil
	case IntentPlanned:
		outcome.ApprovalRequired = intent.Control == policy.OrdinaryConfirm || intent.Control == policy.StrongApproval
		return outcome, nil
	case IntentAuthorized:
		return outcome, fmt.Errorf("authorized intent %s requires recovery dispatch", intent.ID)
	case IntentDispatched, IntentUnknown:
		return outcome, &ExecutionError{Code: "effect_unknown", Unknown: true, Err: fmt.Errorf("effect %s requires reconciliation", intent.ID)}
	case IntentFailed:
		return outcome, fmt.Errorf("effect %s previously failed: %s", intent.ID, intent.ErrorCode)
	default:
		return outcome, fmt.Errorf("effect %s has unsupported state %s", intent.ID, intent.Status)
	}
}

func validGrant(grant *Grant, intent Intent, now time.Time) bool {
	if grant == nil || grant.ID == "" || grant.ApprovalID == "" || !grant.ExpiresAt.After(now) {
		return false
	}
	if intent.ApprovalID != "" && grant.ApprovalID != intent.ApprovalID {
		return false
	}
	return grant.TaskID == intent.TaskID && grant.ToolID == intent.ToolID && grant.ToolVersion == intent.ToolVersion &&
		grant.Effect == intent.Effect && grant.Target == intent.Target && grant.ParametersHash == intent.ParametersHash &&
		policy.Rank(grant.Control) >= policy.Rank(intent.Control)
}

func grantID(grant *Grant) string {
	if grant == nil {
		return ""
	}
	return grant.ID
}

func allowsEffect(allowed []policy.EffectClass, effect policy.EffectClass) bool {
	for _, candidate := range allowed {
		if candidate == effect {
			return true
		}
	}
	return false
}

func hashBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
