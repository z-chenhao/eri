package policy

import "testing"

func TestFloorNeverTreatsDangerousActionsAsAutonomous(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		action Action
		want   ControlLevel
	}{
		{name: "local read", action: Action{Effect: ReadOnly, Target: "notes.md"}, want: Auto},
		{name: "external data", action: Action{Effect: ReadOnly, Target: "search.example", SendsDataExternally: true}, want: NotifyAfter},
		{name: "secret egress", action: Action{Effect: ReadOnly, Target: "search.example", SendsDataExternally: true, ContainsSecret: true}, want: StrongApproval},
		{name: "new local file", action: Action{Effect: Reversible, Target: "draft.md"}, want: Auto},
		{name: "overwrite", action: Action{Effect: Reversible, Target: "draft.md", OverwritesExisting: true}, want: OrdinaryConfirm},
		{name: "delete", action: Action{Effect: Destructive, Target: "archive"}, want: StrongApproval},
		{name: "payment", action: Action{Effect: Financial, Target: "merchant"}, want: StrongApproval},
		{name: "permission", action: Action{Effect: Privileged, Target: "calendar"}, want: StrongApproval},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Floor(tt.action)
			if err != nil {
				t.Fatal(err)
			}
			if got.Control != tt.want {
				t.Fatalf("control = %q, want %q", got.Control, tt.want)
			}
		})
	}
}
