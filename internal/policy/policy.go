// Package policy owns Eri's deterministic authorization floor. Model
// judgments may raise a control level but can never lower this result.
package policy

import "fmt"

type EffectClass string

const (
	ReadOnly      EffectClass = "read_only"
	Reversible    EffectClass = "reversible"
	Communication EffectClass = "communication"
	Destructive   EffectClass = "destructive"
	Financial     EffectClass = "financial"
	Privileged    EffectClass = "privileged"
)

type ControlLevel string

const (
	Auto            ControlLevel = "auto"
	NotifyAfter     ControlLevel = "notify_after"
	OrdinaryConfirm ControlLevel = "ordinary_confirm"
	StrongApproval  ControlLevel = "strong_approval"
	Deny            ControlLevel = "deny"
)

type Action struct {
	Effect              EffectClass
	Target              string
	SendsDataExternally bool
	ContainsSecret      bool
	OverwritesExisting  bool
	Bulk                bool
	Irreversible        bool
	SignificantCost     bool
}

type Assessment struct {
	Control ControlLevel `json:"control"`
	Reasons []string     `json:"reasons"`
}

func Floor(action Action) (Assessment, error) {
	if action.Target == "" {
		return Assessment{Control: Deny, Reasons: []string{"target is not explicit"}}, nil
	}
	if action.ContainsSecret && action.SendsDataExternally {
		return Assessment{Control: StrongApproval, Reasons: []string{"sensitive data would leave the local device"}}, nil
	}
	if action.SignificantCost {
		return Assessment{Control: StrongApproval, Reasons: []string{"the action can incur significant cost"}}, nil
	}
	if action.Bulk || action.Irreversible {
		return Assessment{Control: StrongApproval, Reasons: []string{"the action is bulk or irreversible"}}, nil
	}
	switch action.Effect {
	case ReadOnly:
		if action.SendsDataExternally {
			return Assessment{Control: NotifyAfter, Reasons: []string{"a low-risk read request leaves the local device"}}, nil
		}
		return Assessment{Control: Auto}, nil
	case Reversible:
		if action.OverwritesExisting {
			return Assessment{Control: OrdinaryConfirm, Reasons: []string{"the action replaces existing user data"}}, nil
		}
		return Assessment{Control: Auto}, nil
	case Communication:
		return Assessment{Control: OrdinaryConfirm, Reasons: []string{"the action represents the user to another party"}}, nil
	case Destructive, Financial, Privileged:
		return Assessment{Control: StrongApproval, Reasons: []string{fmt.Sprintf("%s actions require strong approval", action.Effect)}}, nil
	default:
		return Assessment{}, fmt.Errorf("unknown effect class %q", action.Effect)
	}
}

func Rank(level ControlLevel) int {
	switch level {
	case Auto:
		return 0
	case NotifyAfter:
		return 1
	case OrdinaryConfirm:
		return 2
	case StrongApproval:
		return 3
	case Deny:
		return 4
	default:
		return 5
	}
}

func Max(a, b ControlLevel) ControlLevel {
	if Rank(a) >= Rank(b) {
		return a
	}
	return b
}
