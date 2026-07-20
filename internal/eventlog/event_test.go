package eventlog

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestEventSerializesAsCloudEventsCompatibleEnvelope(t *testing.T) {
	event := Event{
		ID: "event-1", Type: "task.started", AggregateType: "task", AggregateID: "task-1",
		Time: time.Date(2026, 7, 19, 1, 2, 3, 0, time.UTC), Data: map[string]any{"run_id": "run-1"}, Sequence: 7,
	}
	Normalize(&event)
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{`"specversion":"1.0"`, `"source":"eri://runtime/event-spine"`, `"type":"task.started"`, `"subject":"task/task-1"`, `"datacontenttype":"application/json"`, `"data":{"run_id":"run-1"}`} {
		if !strings.Contains(string(encoded), required) {
			t.Fatalf("CloudEvents attribute %s missing from %s", required, encoded)
		}
	}
	for _, obsolete := range []string{`"payload"`, `"created_at"`, `"aggregate_type"`} {
		if strings.Contains(string(encoded), obsolete) {
			t.Fatalf("obsolete custom envelope field %s remains in %s", obsolete, encoded)
		}
	}
}
