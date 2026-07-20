// Package subagent owns Eri's private delegation contract. The primary Eri
// assigns work to stable human-readable roles; Runtime binds each role to an
// installed provider and records the actual provider for audit and recovery.
package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/z-chenhao/eri/internal/content"
	"github.com/z-chenhao/eri/internal/policy"
)

type ExecutionMode string

const (
	Foreground ExecutionMode = "foreground"
	Background ExecutionMode = "background"
)

type AccessMode string

const (
	ReadOnly       AccessMode = "read_only"
	WorkspaceWrite AccessMode = "workspace_write"
)

type Capability struct {
	ID          string `json:"id"`
	Description string `json:"description"`
}

type Boundary struct {
	ID          string `json:"id"`
	Description string `json:"description"`
}

// RoleDescriptor is the only job description exposed to Eri's model. It uses
// ordinary workplace language and deliberately contains no provider or runtime
// terminology.
type RoleDescriptor struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	WhenToUse   string `json:"when_to_use"`
}

// DefaultRoles are stable product roles. Installations may bind them to
// different providers without changing Eri's model-facing routing contract.
func DefaultRoles() []RoleDescriptor {
	return []RoleDescriptor{
		{
			ID:          "intern",
			Name:        "Intern",
			Description: "Handles clear, low-risk, routine work that takes time, such as gathering, organizing, comparing, checking, and summarizing information.",
			WhenToUse:   "Use for mechanical information work. Do not assign project changes, code or data engineering, user communication, or decisions requiring specialist judgment.",
		},
		{
			ID:          "engineering_team",
			Name:        "Engineering team",
			Description: "Handles work involving projects, code, or data: investigation, analysis, implementation, debugging, and verification, from small jobs to complex engineering tasks.",
			WhenToUse:   "Use whenever the work requires project, code, structured-data, or engineering expertise.",
		},
	}
}

// ProviderDescriptor is Runtime-only. It declares the authority and execution
// behavior of one installed implementation such as Eri native or Codex.
type ProviderDescriptor struct {
	ID                  string        `json:"id"`
	SupportedRoles      []string      `json:"supported_roles"`
	Execution           ExecutionMode `json:"execution"`
	Capabilities        []Capability  `json:"capabilities"`
	AccessModes         []AccessMode  `json:"access_modes"`
	DefaultAccess       AccessMode    `json:"default_access"`
	SendsDataExternally bool          `json:"sends_data_externally"`
	Boundaries          []Boundary    `json:"boundaries"`
}

type Binding struct {
	RoleID     string
	ProviderID string
}

// Request is the frozen assignment passed between delegate, Registry and a
// Provider. ProviderID is resolved once during Prepare and must never be
// silently rebound during Execute or reconciliation.
type Request struct {
	DelegationID string     `json:"delegation_id,omitempty"`
	TaskID       string     `json:"task_id,omitempty"`
	RunID        string     `json:"run_id,omitempty"`
	Objective    string     `json:"objective"`
	Context      string     `json:"context,omitempty"`
	RoleID       string     `json:"role_id"`
	ProviderID   string     `json:"provider_id,omitempty"`
	Access       AccessMode `json:"access,omitempty"`
}

// Ticket is the model-visible durable deferred control fact. The actual
// provider remains Runtime audit data and is intentionally not serialized.
type Ticket struct {
	DelegationID string        `json:"delegation_id"`
	RoleID       string        `json:"assignee"`
	ProviderID   string        `json:"-"`
	Status       string        `json:"status"`
	Execution    ExecutionMode `json:"-"`
	Access       AccessMode    `json:"access"`
}

// Result is the common evidence contract returned to primary Eri. Providers
// may leave irrelevant lists empty, but cannot replace the terminal shape.
type Result struct {
	DelegationID  string   `json:"delegation_id"`
	RoleID        string   `json:"assignee"`
	ProviderID    string   `json:"-"`
	Status        string   `json:"status"`
	Summary       string   `json:"summary"`
	Evidence      []string `json:"evidence"`
	Changes       []string `json:"changes"`
	Tests         []string `json:"tests"`
	RemainingRisk []string `json:"remaining_risks"`
	ErrorCode     string   `json:"error_code,omitempty"`
}

type Outcome struct {
	Ticket           *Ticket
	Result           *Result
	ExternalObjectID string
	Receipt          string
	FreshAt          time.Time
	Deferred         bool
}

func (o Outcome) Payload() (json.RawMessage, error) {
	if (o.Ticket == nil) == (o.Result == nil) {
		return nil, fmt.Errorf("subagent outcome must contain exactly one ticket or result")
	}
	if o.Ticket != nil {
		return json.Marshal(o.Ticket)
	}
	return json.Marshal(o.Result)
}

type InspectionStatus string

const (
	InspectionConfirmed InspectionStatus = "confirmed"
	InspectionFailed    InspectionStatus = "failed"
	InspectionUnknown   InspectionStatus = "unknown"
)

type Inspection struct {
	Status    InspectionStatus
	Outcome   Outcome
	ErrorCode string
	Retry     bool
}

// Provider is the integration surface for one installed implementation. Role
// descriptions and bindings remain owned by Registry, never by providers.
type Provider interface {
	Descriptor() ProviderDescriptor
	Prepare(context.Context, Request) (Request, policy.Action, error)
	Invoke(context.Context, Request) (Outcome, error)
	Inspect(context.Context, string) (Inspection, error)
}

type Registry struct {
	roles     map[string]RoleDescriptor
	providers map[string]Provider
	bindings  map[string]string
	ordered   []RoleDescriptor
}

var descriptorID = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

func NewRegistry(roles []RoleDescriptor, bindings []Binding, providers ...Provider) (*Registry, error) {
	registry := &Registry{
		roles: make(map[string]RoleDescriptor, len(roles)), providers: make(map[string]Provider, len(providers)),
		bindings: make(map[string]string, len(bindings)),
	}
	for _, role := range roles {
		if err := validateRole(role); err != nil {
			return nil, err
		}
		if _, exists := registry.roles[role.ID]; exists {
			return nil, fmt.Errorf("subagent role %q is already declared", role.ID)
		}
		registry.roles[role.ID] = role
	}
	for _, provider := range providers {
		if provider == nil {
			return nil, fmt.Errorf("subagent provider is required")
		}
		descriptor := provider.Descriptor()
		if err := validateProvider(descriptor); err != nil {
			return nil, err
		}
		if _, exists := registry.providers[descriptor.ID]; exists {
			return nil, fmt.Errorf("subagent provider %q is already registered", descriptor.ID)
		}
		registry.providers[descriptor.ID] = provider
	}
	for _, binding := range bindings {
		if _, found := registry.roles[binding.RoleID]; !found {
			return nil, fmt.Errorf("subagent role %q is not declared", binding.RoleID)
		}
		provider, found := registry.providers[binding.ProviderID]
		if !found {
			// A role whose configured provider is absent is not advertised. This is
			// explicit unavailability, never an implicit fallback.
			continue
		}
		if !supportsRole(provider.Descriptor(), binding.RoleID) {
			return nil, fmt.Errorf("subagent provider %q does not support role %q", binding.ProviderID, binding.RoleID)
		}
		if _, exists := registry.bindings[binding.RoleID]; exists {
			return nil, fmt.Errorf("subagent role %q is bound more than once", binding.RoleID)
		}
		registry.bindings[binding.RoleID] = binding.ProviderID
		registry.ordered = append(registry.ordered, registry.roles[binding.RoleID])
	}
	if len(registry.bindings) == 0 {
		return nil, fmt.Errorf("at least one available subagent role binding is required")
	}
	sort.Slice(registry.ordered, func(i, j int) bool { return registry.ordered[i].ID < registry.ordered[j].ID })
	return registry, nil
}

func (r *Registry) Descriptors() []RoleDescriptor { return append([]RoleDescriptor(nil), r.ordered...) }

func (r *Registry) Roles() []string {
	result := make([]string, 0, len(r.ordered))
	for _, role := range r.ordered {
		result = append(result, role.ID)
	}
	return result
}

func (r *Registry) AccessModes() []string {
	seen := map[AccessMode]struct{}{}
	for _, providerID := range r.bindings {
		for _, mode := range r.providers[providerID].Descriptor().AccessModes {
			seen[mode] = struct{}{}
		}
	}
	result := make([]string, 0, len(seen))
	for mode := range seen {
		result = append(result, string(mode))
	}
	sort.Strings(result)
	return result
}

// RoutingGuide contains natural job descriptions only.
func (r *Registry) RoutingGuide() string {
	lines := make([]string, 0, len(r.ordered))
	for _, role := range r.ordered {
		lines = append(lines, fmt.Sprintf("%s (%s): %s %s", role.ID, role.Name, role.Description, role.WhenToUse))
	}
	return strings.Join(lines, " ")
}

func (r *Registry) Prepare(ctx context.Context, request Request) (Request, policy.Action, error) {
	request.RoleID = strings.TrimSpace(request.RoleID)
	providerID, found := r.bindings[request.RoleID]
	if !found {
		return Request{}, policy.Action{}, fmt.Errorf("assignee %q is unavailable", request.RoleID)
	}
	if request.ProviderID != "" && request.ProviderID != providerID {
		return Request{}, policy.Action{}, fmt.Errorf("assignee %q cannot be rebound during preparation", request.RoleID)
	}
	request.ProviderID = providerID
	request.Objective = strings.TrimSpace(request.Objective)
	request.Context = strings.TrimSpace(request.Context)
	if request.Objective == "" || len([]byte(request.Objective)) > 32*1024 || len([]byte(request.Context)) > 64*1024 {
		return Request{}, policy.Action{}, fmt.Errorf("subagent objective/context exceeds its bounded contract")
	}
	provider := r.providers[providerID]
	descriptor := provider.Descriptor()
	prepared, action, err := provider.Prepare(ctx, request)
	if err != nil {
		return Request{}, policy.Action{}, err
	}
	if prepared.RoleID != request.RoleID || prepared.ProviderID != providerID || strings.TrimSpace(prepared.Objective) == "" || !supportsAccess(descriptor, prepared.Access) {
		return Request{}, policy.Action{}, fmt.Errorf("provider %q returned an assignment outside its frozen binding", providerID)
	}
	if action.SendsDataExternally && !descriptor.SendsDataExternally {
		return Request{}, policy.Action{}, fmt.Errorf("provider %q attempted undeclared external data handling", providerID)
	}
	if prepared.Access == ReadOnly && action.Effect != policy.ReadOnly {
		return Request{}, policy.Action{}, fmt.Errorf("provider %q exceeded read-only access", providerID)
	}
	if prepared.Access == WorkspaceWrite && action.Effect != policy.Reversible {
		return Request{}, policy.Action{}, fmt.Errorf("provider %q returned an invalid workspace-write effect", providerID)
	}
	return prepared, action, nil
}

func (r *Registry) Invoke(ctx context.Context, request Request) (Outcome, error) {
	providerID, bound := r.bindings[request.RoleID]
	if !bound || request.ProviderID == "" || request.ProviderID != providerID {
		return Outcome{}, fmt.Errorf("assignee %q has no matching frozen provider", request.RoleID)
	}
	provider := r.providers[request.ProviderID]
	outcome, err := provider.Invoke(ctx, request)
	if err != nil {
		return Outcome{}, err
	}
	descriptor := provider.Descriptor()
	if descriptor.Execution == Foreground && outcome.Deferred {
		return Outcome{}, fmt.Errorf("foreground provider %q returned a deferred result", request.ProviderID)
	}
	if descriptor.Execution == Background && !outcome.Deferred {
		return Outcome{}, fmt.Errorf("background provider %q did not return a deferred result", request.ProviderID)
	}
	if _, err := outcome.Payload(); err != nil {
		return Outcome{}, err
	}
	if outcome.Deferred && (outcome.Ticket == nil || outcome.Ticket.RoleID != request.RoleID || outcome.Ticket.ProviderID != request.ProviderID || outcome.Ticket.DelegationID != request.DelegationID) {
		return Outcome{}, fmt.Errorf("provider %q returned a mismatched ticket", request.ProviderID)
	}
	if !outcome.Deferred && (outcome.Result == nil || outcome.Result.RoleID != request.RoleID || outcome.Result.ProviderID != request.ProviderID || outcome.Result.DelegationID != request.DelegationID) {
		return Outcome{}, fmt.Errorf("provider %q returned a mismatched result", request.ProviderID)
	}
	if outcome.Deferred && strings.TrimSpace(outcome.Ticket.Status) == "" {
		return Outcome{}, fmt.Errorf("background provider %q returned an empty ticket status", request.ProviderID)
	}
	if !outcome.Deferred && (strings.TrimSpace(outcome.Result.Status) == "" || strings.TrimSpace(outcome.Result.Summary) == "") {
		return Outcome{}, fmt.Errorf("foreground provider %q returned an incomplete terminal result", request.ProviderID)
	}
	return outcome, nil
}

func (r *Registry) Inspect(ctx context.Context, roleID, providerID, id string) (Inspection, error) {
	bound, found := r.bindings[roleID]
	if !found || providerID == "" || providerID != bound {
		return Inspection{Status: InspectionFailed, ErrorCode: "subagent_binding_unavailable"}, nil
	}
	provider := r.providers[providerID]
	inspection, err := provider.Inspect(ctx, id)
	if err != nil || inspection.Status != InspectionConfirmed {
		return inspection, err
	}
	if _, err := inspection.Outcome.Payload(); err != nil {
		return Inspection{}, err
	}
	if !inspection.Outcome.Deferred || inspection.Outcome.Ticket == nil ||
		inspection.Outcome.Ticket.RoleID != roleID || inspection.Outcome.Ticket.ProviderID != providerID || inspection.Outcome.Ticket.DelegationID != id {
		return Inspection{}, fmt.Errorf("provider %q returned a mismatched inspection", providerID)
	}
	return inspection, nil
}

func validateRole(role RoleDescriptor) error {
	if !descriptorID.MatchString(role.ID) || strings.TrimSpace(role.Name) == "" || strings.TrimSpace(role.Description) == "" || strings.TrimSpace(role.WhenToUse) == "" {
		return fmt.Errorf("subagent role id, name, description and when_to_use are required")
	}
	return nil
}

func validateProvider(descriptor ProviderDescriptor) error {
	if !descriptorID.MatchString(descriptor.ID) {
		return fmt.Errorf("subagent provider id is required")
	}
	if descriptor.Execution != Foreground && descriptor.Execution != Background {
		return fmt.Errorf("provider %q has invalid execution mode %q", descriptor.ID, descriptor.Execution)
	}
	if len(descriptor.SupportedRoles) == 0 || len(descriptor.Capabilities) == 0 || len(descriptor.AccessModes) == 0 || len(descriptor.Boundaries) == 0 {
		return fmt.Errorf("provider %q must declare supported roles, capabilities, access modes and boundaries", descriptor.ID)
	}
	seenRoles := map[string]struct{}{}
	for _, roleID := range descriptor.SupportedRoles {
		if !descriptorID.MatchString(roleID) {
			return fmt.Errorf("provider %q has an invalid supported role", descriptor.ID)
		}
		if _, duplicate := seenRoles[roleID]; duplicate {
			return fmt.Errorf("provider %q repeats supported role %q", descriptor.ID, roleID)
		}
		seenRoles[roleID] = struct{}{}
	}
	seenCapabilities := map[string]struct{}{}
	for _, capability := range descriptor.Capabilities {
		if !descriptorID.MatchString(capability.ID) || strings.TrimSpace(capability.Description) == "" {
			return fmt.Errorf("provider %q has an invalid capability descriptor", descriptor.ID)
		}
		if _, duplicate := seenCapabilities[capability.ID]; duplicate {
			return fmt.Errorf("provider %q repeats capability %q", descriptor.ID, capability.ID)
		}
		seenCapabilities[capability.ID] = struct{}{}
	}
	seenBoundaries := map[string]struct{}{}
	for _, boundary := range descriptor.Boundaries {
		if !descriptorID.MatchString(boundary.ID) || strings.TrimSpace(boundary.Description) == "" {
			return fmt.Errorf("provider %q has an invalid boundary descriptor", descriptor.ID)
		}
		if _, duplicate := seenBoundaries[boundary.ID]; duplicate {
			return fmt.Errorf("provider %q repeats boundary %q", descriptor.ID, boundary.ID)
		}
		seenBoundaries[boundary.ID] = struct{}{}
	}
	defaultFound := false
	seenAccess := map[AccessMode]struct{}{}
	for _, mode := range descriptor.AccessModes {
		if mode != ReadOnly && mode != WorkspaceWrite {
			return fmt.Errorf("provider %q has invalid access mode %q", descriptor.ID, mode)
		}
		if _, duplicate := seenAccess[mode]; duplicate {
			return fmt.Errorf("provider %q repeats access mode %q", descriptor.ID, mode)
		}
		seenAccess[mode] = struct{}{}
		defaultFound = defaultFound || mode == descriptor.DefaultAccess
	}
	if !defaultFound {
		return fmt.Errorf("provider %q default access is not supported", descriptor.ID)
	}
	return nil
}

func supportsAccess(descriptor ProviderDescriptor, requested AccessMode) bool {
	for _, mode := range descriptor.AccessModes {
		if requested == mode {
			return true
		}
	}
	return false
}

func supportsRole(descriptor ProviderDescriptor, roleID string) bool {
	for _, supported := range descriptor.SupportedRoles {
		if supported == roleID {
			return true
		}
	}
	return false
}

// Run is the durable provider-neutral record for a background subagent.
// RuntimeStateRef belongs to the subagent's own Agent Loop; ContinuationRef
// belongs to the paused primary Eri and the two must never be conflated.
type Run struct {
	ID                 string
	RoleID             string
	ProviderID         string
	ParentTaskID       string
	ParentRunID        string
	Access             AccessMode
	Status             string
	RequestRef         content.Ref
	ResultRef          content.Ref
	RuntimeStateRef    content.Ref
	ContinuationRef    content.Ref
	ProgressDeliveryID string
	RuntimeID          string
	ErrorCode          string
	StartedAt          time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
}
