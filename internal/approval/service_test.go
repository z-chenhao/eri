package approval

import (
	"context"
	"testing"
)

type fakeRepository struct {
	decision Decision
}

func (f *fakeRepository) ResolveApproval(_ context.Context, id string, decision Decision) (Result, error) {
	f.decision = decision
	return Result{ApprovalID: id, TaskID: "task", Status: string(decision) + "d"}, nil
}

func TestServiceAcceptsOnlyExplicitDecisions(t *testing.T) {
	repository := &fakeRepository{}
	service := NewService(repository)
	if _, err := service.Decide(context.Background(), "approval", Approve); err != nil {
		t.Fatal(err)
	}
	if repository.decision != Approve {
		t.Fatalf("decision = %q", repository.decision)
	}
	if _, err := service.Decide(context.Background(), "approval", Decision("maybe")); err == nil {
		t.Fatal("ambiguous decision was accepted")
	}
}
