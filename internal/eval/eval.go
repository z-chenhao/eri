// Package eval evaluates immutable artifact candidates before delivery.
package eval

import (
	"fmt"
	"strings"
	"time"
)

type Result string

const (
	Pass     Result = "pass"
	Repair   Result = "repair"
	Hold     Result = "hold"
	Escalate Result = "escalate"
)

// Record is the evaluation result for one exact artifact version.
type Record struct {
	ID         string    `json:"id"`
	ArtifactID string    `json:"artifact_id"`
	Tier       string    `json:"tier"`
	Evaluator  string    `json:"evaluator"`
	Result     Result    `json:"result"`
	Findings   []string  `json:"findings"`
	CreatedAt  time.Time `json:"created_at"`
}

type Decision struct {
	Result   Result   `json:"result"`
	Tier     string   `json:"tier"`
	Findings []string `json:"findings"`
}

func (d Decision) Validate() error {
	switch d.Result {
	case Pass, Repair, Hold, Escalate:
	default:
		return fmt.Errorf("unsupported result %q", d.Result)
	}
	switch d.Tier {
	case "routine", "substantive", "external", "high_stakes":
	default:
		return fmt.Errorf("unsupported tier %q", d.Tier)
	}
	if d.Result != Pass && len(d.Findings) == 0 {
		return fmt.Errorf("%s requires at least one finding", d.Result)
	}
	if len(d.Findings) > 20 {
		return fmt.Errorf("findings exceed limit")
	}
	for _, finding := range d.Findings {
		if strings.TrimSpace(finding) == "" || len([]byte(finding)) > 2048 {
			return fmt.Errorf("finding must be non-empty and at most 2 KiB")
		}
	}
	return nil
}

// Routine applies non-semantic deterministic delivery gates.
// Holistic semantic quality is evaluated by the model-backed Judge.
func Routine(body string) (Result, []string) {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return Repair, []string{"artifact body is empty"}
	}
	if len([]byte(body)) > 256*1024 {
		return Hold, []string{"artifact exceeds the routine message size limit"}
	}
	if strings.ContainsRune(body, '\x00') {
		return Hold, []string{"artifact contains a NUL byte"}
	}
	return Pass, nil
}
