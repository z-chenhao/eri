package pluginv1

import "time"

const ResultMetadataKey = "eri"

// ResultMetadata carries provider-grounded receipt facts outside the model's
// ordinary structured output. Eri still treats the plugin as an untrusted
// boundary, but can preserve the external object identity for reconciliation.
// Credentials and capability handles are forbidden here.
type ResultMetadata struct {
	Receipt          string    `json:"receipt"`
	ExternalObjectID string    `json:"external_object_id,omitempty"`
	FreshAt          time.Time `json:"fresh_at,omitempty"`
	Uncertainty      []string  `json:"uncertainty,omitempty"`
}
