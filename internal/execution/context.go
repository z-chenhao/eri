// Package execution defines Eri's stable, provider-neutral execution facts.
// It contains data shared across the Agent Loop, durable runtime and read-only
// projections; it does not contain orchestration behavior.
package execution

import "time"

const (
	TriggerEventCommitmentDue = "commitment.due"
	TriggerStateOccurred      = "occurred"
	TaskPhaseFulfillment      = "fulfillment"
)

// ModelCapabilities are provider facts that affect safe request assembly.
// Providers report them before Eri sends any user context.
type ModelCapabilities struct {
	Text              bool   `json:"text"`
	Image             bool   `json:"image"`
	StructuredOutput  bool   `json:"structured_output"`
	ToolCalling       bool   `json:"tool_calling"`
	ParallelToolCalls bool   `json:"parallel_tool_calls"`
	Streaming         bool   `json:"streaming"`
	Usage             bool   `json:"usage"`
	Cancellation      bool   `json:"cancellation"`
	ContextTokens     int    `json:"context_tokens"`
	MaxOutputTokens   int    `json:"max_output_tokens"`
	DataResidency     string `json:"data_residency"`
}

type ExternalData struct {
	MessageIDs          []string `json:"message_ids,omitempty"`
	MemoryIDs           []string `json:"memory_ids,omitempty"`
	SkillIDs            []string `json:"skill_ids,omitempty"`
	Categories          []string `json:"categories,omitempty"`
	ContextCheckpointID string   `json:"context_checkpoint_id,omitempty"`
}

type Compression struct {
	Applied                bool     `json:"applied"`
	CheckpointID           string   `json:"checkpoint_id,omitempty"`
	SummarizedCount        int      `json:"summarized_count,omitempty"`
	SummarizedMessageIDs   []string `json:"summarized_message_ids,omitempty"`
	FirstKeptInteractionID string   `json:"first_kept_interaction_id,omitempty"`
	TokensBefore           int      `json:"tokens_before"`
	TokensAfter            int      `json:"tokens_after,omitempty"`
}

type RuntimeCompaction struct {
	TokensBefore       int `json:"before_tokens"`
	TokensAfter        int `json:"after_tokens"`
	SummarizedMessages int `json:"summarized_messages"`
}

// TaskCapsule identifies the durable Runtime task represented in one model
// invocation. The objective body remains in the governed Content Store; this
// manifest records only safe provenance and scheduling facts.
type TaskCapsule struct {
	TaskID              string    `json:"task_id"`
	SourceInteractionID string    `json:"source_interaction_id"`
	SourceKind          string    `json:"source_kind"`
	SourceRole          string    `json:"source_role"`
	TriggerChannel      string    `json:"trigger_channel"`
	TriggerEvent        string    `json:"trigger_event,omitempty"`
	TriggerState        string    `json:"trigger_state,omitempty"`
	ExecutionPhase      string    `json:"execution_phase,omitempty"`
	CommitmentID        string    `json:"commitment_id,omitempty"`
	ScheduledFor        time.Time `json:"scheduled_for,omitempty"`
}

// ContextManifest is the durable declaration of context assembled for one
// invocation. IDs are stored instead of secret or private payload bodies.
type ContextManifest struct {
	IdentityID              string              `json:"identity_id"`
	SoulVersion             string              `json:"soul_version"`
	MemoryRetrievalID       string              `json:"memory_retrieval_id,omitempty"`
	RetrievedMemoryIDs      []string            `json:"retrieved_memory_ids,omitempty"`
	MemoryIDs               []string            `json:"memory"`
	AppliedMemoryIDs        []string            `json:"applied_memory_ids,omitempty"`
	MemoryChecked           bool                `json:"memory_checked"`
	SkillIDs                []string            `json:"skills"`
	ToolIDs                 []string            `json:"tools"`
	ExternalDataSent        bool                `json:"external_data_sent"`
	ResponseProfile         string              `json:"response_profile"`
	SourceChannel           string              `json:"source_channel,omitempty"`
	RuntimeObservedAt       time.Time           `json:"runtime_observed_at,omitempty"`
	RuntimeTimezone         string              `json:"runtime_timezone,omitempty"`
	EvolutionReleaseID      string              `json:"evolution_release_id,omitempty"`
	EvolutionReleaseVersion int                 `json:"evolution_release_version,omitempty"`
	ProviderCapabilities    ModelCapabilities   `json:"provider_capabilities"`
	MessageIDs              []string            `json:"message_ids,omitempty"`
	ConversationSequence    int64               `json:"conversation_sequence,omitempty"`
	AttachmentIDs           []string            `json:"attachment_ids,omitempty"`
	ContextWindowTokens     int                 `json:"context_window_tokens,omitempty"`
	OutputReservedTokens    int                 `json:"output_reserved_tokens,omitempty"`
	ContextInputLimitTokens int                 `json:"context_input_limit_tokens,omitempty"`
	EstimatedInputTokens    int                 `json:"estimated_input_tokens,omitempty"`
	ExternalData            *ExternalData       `json:"external_data,omitempty"`
	Compression             Compression         `json:"compression"`
	RuntimeCompactions      []RuntimeCompaction `json:"runtime_compactions,omitempty"`
	ExternalMemoryIDs       []string            `json:"external_memory_ids,omitempty"`
	CurrentTask             *TaskCapsule        `json:"current_task,omitempty"`
}
