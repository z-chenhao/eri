// Package eventlog defines safe, durable execution facts. Its JSON envelope is
// CloudEvents 1.0 compatible; Eri-specific correlation remains in extension
// attributes and domain data.
package eventlog

import (
	"strings"
	"time"
)

const (
	SpecVersion = "1.0"
	Source      = "eri://runtime/event-spine"
)

// Event is metadata about a committed fact. Private bodies remain in the
// governed content store and are referenced rather than copied into Data.
// The eri* JSON fields are CloudEvents extension attributes.
type Event struct {
	SpecVersion     string         `json:"specversion"`
	ID              string         `json:"id"`
	Source          string         `json:"source"`
	Type            string         `json:"type"`
	Subject         string         `json:"subject,omitempty"`
	Time            time.Time      `json:"time"`
	DataContentType string         `json:"datacontenttype"`
	Data            map[string]any `json:"data,omitempty"`
	Sequence        int64          `json:"erisequence"`
	AggregateType   string         `json:"eriaggregatetype"`
	AggregateID     string         `json:"eriaggregateid"`
	Visibility      string         `json:"erivisibility"`
}

// Normalize fills the stable CloudEvents context derived from the durable
// event record. It is safe to call repeatedly after loading from storage.
func Normalize(event *Event) {
	if event.SpecVersion == "" {
		event.SpecVersion = SpecVersion
	}
	if event.Source == "" {
		event.Source = Source
	}
	if event.Subject == "" && event.AggregateType != "" && event.AggregateID != "" {
		event.Subject = strings.Trim(event.AggregateType, "/") + "/" + strings.Trim(event.AggregateID, "/")
	}
	if event.DataContentType == "" {
		event.DataContentType = "application/json"
	}
	if event.Visibility == "" {
		event.Visibility = "developer"
	}
}
