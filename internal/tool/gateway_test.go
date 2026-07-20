package tool

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/z-chenhao/eri/internal/content"
	"github.com/z-chenhao/eri/internal/policy"
)

func TestGatewayPersistsBeforeDispatchAndReplaysConfirmedResult(t *testing.T) {
	t.Parallel()
	store := newMemoryIntentStore()
	results := newMemoryContentStore()
	candidate := &fakeTool{effect: policy.ReadOnly, target: "notes.md"}
	gateway, err := NewGateway(store, results, candidate)
	if err != nil {
		t.Fatal(err)
	}
	request := Request{TaskID: "task", RunID: "run", InvocationID: "invocation", ToolCallID: "call-1", ToolID: "test.tool", Input: json.RawMessage(`{"query":"hello"}`)}
	first, err := gateway.Invoke(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Intent.Status != IntentConfirmed || first.Control != policy.Auto || candidate.calls != 1 {
		t.Fatalf("first outcome = %+v, calls = %d", first, candidate.calls)
	}
	if first.Intent.InvocationID != "invocation" || first.Intent.ToolCallID != "call-1" {
		t.Fatalf("native tool correlation was not preserved: %+v", first.Intent)
	}
	payload, err := results.Get(context.Background(), first.Intent.PayloadRef)
	if err != nil || string(payload) != `{"query":"hello"}` {
		t.Fatalf("persisted effect payload = %q err=%v", payload, err)
	}
	if candidate.lastPrepared.TaskID != "task" || candidate.lastPrepared.RunID != "run" || candidate.lastPrepared.InvocationID != first.Intent.ID {
		t.Fatalf("gateway runtime identity = %+v", candidate.lastPrepared)
	}
	if len(store.transitions) != 3 || store.transitions[0] != "planned->authorized" || store.transitions[1] != "authorized->dispatched" || store.transitions[2] != "dispatched->confirmed" {
		t.Fatalf("transitions = %#v", store.transitions)
	}
	second, err := gateway.Invoke(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Replayed || candidate.calls != 1 {
		t.Fatalf("duplicate invocation executed again: outcome=%+v calls=%d", second, candidate.calls)
	}
}

func TestGatewaySeparatesIdenticalChildCallsByParentDelegation(t *testing.T) {
	t.Parallel()
	store := newMemoryIntentStore()
	results := newMemoryContentStore()
	candidate := &fakeTool{effect: policy.ReadOnly, target: "research"}
	gateway, err := NewGateway(store, results, candidate)
	if err != nil {
		t.Fatal(err)
	}
	base := Request{TaskID: "task", RunID: "run", ToolID: "test.tool", Input: json.RawMessage(`{"query":"same"}`)}
	base.ParentIntentID = "delegation-a"
	first, err := gateway.Invoke(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	base.ParentIntentID = "delegation-b"
	second, err := gateway.Invoke(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	if first.Intent.ID == second.Intent.ID || first.Intent.IdempotencyKey == second.Intent.IdempotencyKey || candidate.calls != 2 {
		t.Fatalf("first=%+v second=%+v calls=%d", first.Intent, second.Intent, candidate.calls)
	}
}

func TestGatewayRequiresExactGrantBeforeOverwrite(t *testing.T) {
	t.Parallel()
	store := newMemoryIntentStore()
	results := newMemoryContentStore()
	candidate := &fakeTool{effect: policy.Reversible, target: "plan.md", overwrite: true}
	gateway, err := NewGateway(store, results, candidate)
	if err != nil {
		t.Fatal(err)
	}
	gateway.now = func() time.Time { return time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC) }
	request := Request{TaskID: "task", RunID: "run", ToolID: "test.tool", Input: json.RawMessage(`{"content":"new"}`)}
	pending, err := gateway.Invoke(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !pending.ApprovalRequired || pending.Control != policy.OrdinaryConfirm || candidate.calls != 0 {
		t.Fatalf("pending = %+v, calls = %d", pending, candidate.calls)
	}
	persisted := store.byKey[pending.Intent.IdempotencyKey]
	persisted.ApprovalID = "expected-approval"
	store.byKey[pending.Intent.IdempotencyKey] = persisted
	request.Grant = &Grant{
		ID: "grant", ApprovalID: "approval", TaskID: "task", ToolID: "test.tool", ToolVersion: "1.0.0",
		Effect: policy.Reversible, Target: "wrong.md", ParametersHash: pending.Intent.ParametersHash,
		Control: policy.StrongApproval, ExpiresAt: gateway.now().Add(time.Minute),
	}
	stillPending, err := gateway.Invoke(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !stillPending.ApprovalRequired || candidate.calls != 0 {
		t.Fatalf("mismatched grant dispatched: %+v", stillPending)
	}
	request.Grant.Target = pending.Intent.Target
	stillPending, err = gateway.Invoke(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !stillPending.ApprovalRequired || candidate.calls != 0 {
		t.Fatalf("grant from another approval dispatched: %+v", stillPending)
	}
	request.Grant.ApprovalID = "expected-approval"
	approved, err := gateway.Invoke(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if approved.Intent.Status != IntentConfirmed || candidate.calls != 1 {
		t.Fatalf("approved = %+v, calls = %d", approved, candidate.calls)
	}
	stored := store.byKey[pending.Intent.IdempotencyKey]
	if stored.ApprovalID != "expected-approval" || stored.GrantID != "grant" {
		t.Fatalf("authorization binding not persisted: %+v", stored)
	}
}

func TestGatewayEnforcesRestrictedScopeBeforePersistingIntent(t *testing.T) {
	store := newMemoryIntentStore()
	results := newMemoryContentStore()
	candidate := &fakeTool{effect: policy.Reversible, target: "plan.md"}
	gateway, err := NewGateway(store, results, candidate)
	if err != nil {
		t.Fatal(err)
	}
	scope := &CapabilityScope{
		AllowedToolIDs: map[string]struct{}{"test.tool": {}},
		AllowedEffects: map[policy.EffectClass]struct{}{policy.ReadOnly: {}},
		AllowApproval:  false,
	}
	_, err = gateway.Invoke(context.Background(), Request{
		TaskID: "task", RunID: "run", ToolID: "test.tool", Input: json.RawMessage(`{"content":"new"}`), Scope: scope,
	})
	if err == nil {
		t.Fatal("restricted agent write was accepted")
	}
	if len(store.byKey) != 0 || candidate.calls != 0 {
		t.Fatalf("scope violation reached persistence or dispatch: intents=%d calls=%d", len(store.byKey), candidate.calls)
	}
	candidate.effect = policy.ReadOnly
	outcome, err := gateway.Invoke(context.Background(), Request{
		TaskID: "task", RunID: "run", ToolID: "test.tool", Input: json.RawMessage(`{"query":"safe"}`), Scope: scope,
	})
	if err != nil || outcome.Intent.Status != IntentConfirmed || candidate.calls != 1 {
		t.Fatalf("read-only mixed tool action outcome=%+v err=%v calls=%d", outcome, err, candidate.calls)
	}
}

func TestGatewayDoesNotBlindlyRetryUnknownEffect(t *testing.T) {
	t.Parallel()
	store := newMemoryIntentStore()
	results := newMemoryContentStore()
	candidate := &fakeTool{effect: policy.ReadOnly, target: "remote", executeErr: &ExecutionError{Code: "timeout_after_send", Unknown: true, Err: errors.New("timeout")}}
	gateway, err := NewGateway(store, results, candidate)
	if err != nil {
		t.Fatal(err)
	}
	request := Request{TaskID: "task", RunID: "run", ToolID: "test.tool", Input: json.RawMessage(`{}`)}
	first, err := gateway.Invoke(context.Background(), request)
	if err == nil || first.Intent.Status != IntentUnknown || candidate.calls != 1 {
		t.Fatalf("first = %+v, err = %v, calls = %d", first, err, candidate.calls)
	}
	second, err := gateway.Invoke(context.Background(), request)
	if err == nil || second.Intent.Status != IntentUnknown || candidate.calls != 1 {
		t.Fatalf("unknown effect retried: second=%+v err=%v calls=%d", second, err, candidate.calls)
	}
}

func TestGatewayReconcilesUnknownEffectWithoutRepeatingMutation(t *testing.T) {
	store := newMemoryIntentStore()
	results := newMemoryContentStore()
	candidate := &fakeTool{
		effect: policy.Reversible, target: "remote-calendar",
		executeErr: &ExecutionError{Code: "timeout_after_send", Unknown: true, Err: errors.New("timeout")},
		reconcileResult: ReconcileResult{
			Status: IntentConfirmed,
			Result: Result{Output: json.RawMessage(`{"event_id":"event-1"}`), Receipt: "provider:event-1"},
		},
	}
	gateway, err := NewGateway(store, results, candidate)
	if err != nil {
		t.Fatal(err)
	}
	request := Request{TaskID: "task", RunID: "run", ToolID: "test.tool", Input: json.RawMessage(`{"title":"match"}`)}
	unknown, err := gateway.Invoke(context.Background(), request)
	if err == nil || unknown.Intent.Status != IntentUnknown || candidate.calls != 1 {
		t.Fatalf("unknown=%+v err=%v calls=%d", unknown, err, candidate.calls)
	}
	if err := gateway.Reconcile(context.Background(), unknown.Intent.ID, 0); err != nil {
		t.Fatal(err)
	}
	replayed, err := gateway.Invoke(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.Intent.Status != IntentConfirmed || candidate.calls != 1 || candidate.reconcileCalls != 1 {
		t.Fatalf("replayed=%+v execute=%d reconcile=%d", replayed, candidate.calls, candidate.reconcileCalls)
	}
}

func TestGatewayAtomicallyReplacesOnePluginNamespace(t *testing.T) {
	store := newMemoryIntentStore()
	results := newMemoryContentStore()
	gateway, err := NewGateway(store, results, &fakeTool{effect: policy.ReadOnly, target: "local"})
	if err != nil {
		t.Fatal(err)
	}
	pluginV1 := &fakePluginTool{id: "mcp.calendar.list", version: "1.0.0"}
	if err := gateway.ReplacePluginTools("calendar", []Tool{pluginV1}); err != nil {
		t.Fatal(err)
	}
	pluginV2 := &fakePluginTool{id: "mcp.calendar.search", version: "2.0.0"}
	if err := gateway.ReplacePluginTools("calendar", []Tool{pluginV2}); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, descriptor := range gateway.Descriptors() {
		ids[descriptor.ID] = true
	}
	if ids[pluginV1.id] || !ids[pluginV2.id] || !ids["test.tool"] {
		t.Fatalf("descriptors=%+v", ids)
	}
}

type fakePluginTool struct{ id, version string }

func (f *fakePluginTool) Descriptor() Descriptor {
	return Descriptor{ID: f.id, Version: f.version, Purpose: "plugin", Source: Plugin}
}
func (f *fakePluginTool) Prepare(context.Context, json.RawMessage) (Prepared, error) {
	return Prepared{}, nil
}
func (f *fakePluginTool) Execute(context.Context, Prepared) (Result, error) { return Result{}, nil }

type fakeTool struct {
	effect          policy.EffectClass
	target          string
	overwrite       bool
	executeErr      error
	reconcileResult ReconcileResult
	reconcileErr    error
	reconcileCalls  int
	calls           int
	lastPrepared    Prepared
}

func (f *fakeTool) Reconcile(_ context.Context, request ReconcileRequest) (ReconcileResult, error) {
	f.reconcileCalls++
	if request.Intent.ParametersHash == "" || len(request.Payload) == 0 {
		return ReconcileResult{}, errors.New("missing reconciliation evidence")
	}
	return f.reconcileResult, f.reconcileErr
}

func (f *fakeTool) Descriptor() Descriptor {
	return Descriptor{
		ID: "test.tool", Version: "1.0.0", Purpose: "test",
		AllowedEffects: []policy.EffectClass{policy.ReadOnly, policy.Reversible},
		Reconciliation: "test", Source: BuiltIn,
	}
}

func (f *fakeTool) Prepare(_ context.Context, raw json.RawMessage) (Prepared, error) {
	if !json.Valid(raw) {
		return Prepared{}, errors.New("invalid json")
	}
	return Prepared{Input: raw, Action: policy.Action{Effect: f.effect, Target: f.target, OverwritesExisting: f.overwrite}}, nil
}

func (f *fakeTool) Execute(_ context.Context, prepared Prepared) (Result, error) {
	f.calls++
	f.lastPrepared = prepared
	if f.executeErr != nil {
		return Result{}, f.executeErr
	}
	return Result{Output: json.RawMessage(`{"ok":true}`), Receipt: "receipt", FreshAt: time.Now().UTC()}, nil
}

type memoryIntentStore struct {
	byKey           map[string]Intent
	keyByID         map[string]string
	transitions     []string
	reconciliations []string
}

func (s *memoryIntentStore) LoadIntentByID(_ context.Context, id string) (Intent, bool, error) {
	key, found := s.keyByID[id]
	if !found {
		return Intent{}, false, nil
	}
	return s.byKey[key], true, nil
}

func (s *memoryIntentStore) RecordReconciliationAttempt(_ context.Context, id, outcome, errorCode string) error {
	s.reconciliations = append(s.reconciliations, id+":"+outcome+":"+errorCode)
	return nil
}

func newMemoryIntentStore() *memoryIntentStore {
	return &memoryIntentStore{byKey: make(map[string]Intent), keyByID: make(map[string]string)}
}

func (s *memoryIntentStore) PlanIntent(_ context.Context, intent Intent) (Intent, bool, error) {
	if existing, found := s.byKey[intent.IdempotencyKey]; found {
		return existing, false, nil
	}
	s.byKey[intent.IdempotencyKey] = intent
	s.keyByID[intent.ID] = intent.IdempotencyKey
	return intent, true, nil
}

func (s *memoryIntentStore) TransitionIntent(_ context.Context, id string, from, to IntentStatus, errorCode, approvalID, grantID string, result content.Ref) error {
	key := s.keyByID[id]
	intent := s.byKey[key]
	if intent.Status != from {
		return errors.New("wrong state")
	}
	intent.Status = to
	intent.ErrorCode = errorCode
	if approvalID != "" {
		intent.ApprovalID = approvalID
	}
	if grantID != "" {
		intent.GrantID = grantID
	}
	if result.ObjectID != "" {
		intent.ResultRef = result
	}
	s.byKey[key] = intent
	s.transitions = append(s.transitions, string(from)+"->"+string(to))
	return nil
}

type memoryContentStore struct {
	values map[string][]byte
}

func newMemoryContentStore() *memoryContentStore {
	return &memoryContentStore{values: make(map[string][]byte)}
}

func (s *memoryContentStore) Put(_ context.Context, body []byte, metadata content.Metadata) (content.Ref, error) {
	digest := sha256.Sum256(body)
	id := fmt.Sprintf("%x", digest[:])
	s.values[id] = append([]byte(nil), body...)
	return content.Ref{ObjectID: id, Version: 1, ContentHash: id, MediaType: metadata.MediaType, SizeBytes: int64(len(body)), EncryptionDomain: metadata.EncryptionDomain, PrivacyClass: metadata.PrivacyClass, RetentionPolicy: metadata.RetentionPolicy}, nil
}

func (s *memoryContentStore) Get(_ context.Context, ref content.Ref) ([]byte, error) {
	body, found := s.values[ref.ObjectID]
	if !found {
		return nil, errors.New("missing content")
	}
	return append([]byte(nil), body...), nil
}

func (s *memoryContentStore) Delete(_ context.Context, ref content.Ref) error {
	delete(s.values, ref.ObjectID)
	return nil
}
