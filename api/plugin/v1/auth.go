// Package pluginv1 defines the stable, provider-neutral wire contracts used
// between Eri, an external authorization broker, and out-of-process plugins.
// Provider credentials are intentionally present only in RedemptionResponse,
// which is exchanged directly between the broker and the plugin process.
package pluginv1

import "time"

const (
	CapabilityIssuePath     = "/v1/capability-handles"
	CapabilityRedeemPath    = "/v1/capability-handles/redeem"
	AuthorizationStatusPath = "/v1/authorizations/status"
	AuthorizationStartPath  = "/v1/authorizations"
	AuthorizationRevokePath = "/v1/authorizations/google"
)

// InvocationBinding prevents a capability from moving across a plugin, Task,
// Run, or Effect Intent. InvocationID is the persisted Eri Effect Intent ID.
type InvocationBinding struct {
	PluginID     string `json:"plugin_id"`
	TaskID       string `json:"task_id"`
	RunID        string `json:"run_id"`
	InvocationID string `json:"invocation_id"`
}

// CapabilityHandleRequest is sent by Eri Core. It contains no provider
// credential and requests exactly the scopes needed by the selected tool.
type CapabilityHandleRequest struct {
	InvocationBinding
	Provider   string   `json:"provider"`
	Scopes     []string `json:"scopes"`
	MaxUses    int      `json:"max_uses"`
	TTLSeconds int      `json:"ttl_seconds"`
}

type CapabilityHandleResponse struct {
	Handle    string    `json:"handle"`
	ExpiresAt time.Time `json:"expires_at"`
}

// RedemptionRequest is sent by the plugin directly to the broker. Repeating
// the complete binding lets the broker reject a stolen or confused handle.
type RedemptionRequest struct {
	InvocationBinding
	Provider string   `json:"provider"`
	Scopes   []string `json:"scopes"`
	Handle   string   `json:"handle"`
}

// RedemptionResponse must never cross into Eri Core, MCP output, logs, Event,
// Memory, Episode, Dataset, or an exported manifest. It deliberately has no
// refresh-token, cookie, or session fields.
type RedemptionResponse struct {
	Provider    string    `json:"provider"`
	Scopes      []string  `json:"scopes"`
	TokenType   string    `json:"token_type"`
	AccessToken string    `json:"access_token"`
	ExpiresAt   time.Time `json:"expires_at"`
}

type AuthorizationStatus struct {
	Provider         string    `json:"provider"`
	Authorized       bool      `json:"authorized"`
	GrantedScopes    []string  `json:"granted_scopes,omitempty"`
	MissingScopes    []string  `json:"missing_scopes,omitempty"`
	AuthorizedAt     time.Time `json:"authorized_at,omitempty"`
	CredentialSource string    `json:"credential_source,omitempty"`
}

type AuthorizationStartRequest struct {
	Provider string   `json:"provider"`
	Scopes   []string `json:"scopes"`
}

type AuthorizationStartResponse struct {
	Provider         string    `json:"provider"`
	AuthorizationURL string    `json:"authorization_url"`
	ExpiresAt        time.Time `json:"expires_at"`
}

type AuthorizationRevokeResponse struct {
	Provider  string    `json:"provider"`
	Revoked   bool      `json:"revoked"`
	RevokedAt time.Time `json:"revoked_at"`
	Receipt   string    `json:"receipt"`
}
