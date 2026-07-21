package eval

import "testing"

func TestDecisionValidationKeepsPassAndFindingsConsistent(t *testing.T) {
	tests := []struct {
		name     string
		decision Decision
		valid    bool
	}{
		{name: "clean pass", decision: Decision{Result: Pass, Tier: "routine"}, valid: true},
		{name: "pass with violation", decision: Decision{Result: Pass, Tier: "routine", Findings: []string{"candidate is ungrounded"}}},
		{name: "repair with finding", decision: Decision{Result: Repair, Tier: "routine", Findings: []string{"candidate is ungrounded"}}, valid: true},
		{name: "repair without finding", decision: Decision{Result: Repair, Tier: "routine"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.decision.Validate()
			if test.valid && err != nil {
				t.Fatalf("valid decision rejected: %v", err)
			}
			if !test.valid && err == nil {
				t.Fatal("inconsistent decision was accepted")
			}
		})
	}
}

func TestRoutine(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		want Result
	}{
		{name: "ordinary reply", body: "I have organized it.", want: Pass},
		{name: "empty", body: " \n", want: Repair},
		{name: "invalid binary", body: "a\x00b", want: Hold},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, _ := Routine(tt.body)
			if got != tt.want {
				t.Fatalf("Routine() = %q, want %q", got, tt.want)
			}
		})
	}
}
