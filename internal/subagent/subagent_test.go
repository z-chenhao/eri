package subagent

import (
	"context"
	"strings"
	"testing"

	"github.com/z-chenhao/eri/internal/policy"
)

type testProvider struct{ descriptor ProviderDescriptor }

func (p testProvider) Descriptor() ProviderDescriptor { return p.descriptor }
func (p testProvider) Prepare(_ context.Context, request Request) (Request, policy.Action, error) {
	if request.Access == "" {
		request.Access = p.descriptor.DefaultAccess
	}
	return request, policy.Action{Effect: policy.ReadOnly, Target: "provider:" + p.descriptor.ID}, nil
}
func (testProvider) Invoke(context.Context, Request) (Outcome, error) { return Outcome{}, nil }
func (testProvider) Inspect(context.Context, string) (Inspection, error) {
	return Inspection{Status: InspectionUnknown}, nil
}

func testProviderDescriptor(id string) ProviderDescriptor {
	return ProviderDescriptor{
		ID: id, SupportedRoles: []string{"intern"}, Execution: Background,
		Capabilities: []Capability{{ID: "routine_work", Description: "Bounded work"}},
		AccessModes:  []AccessMode{ReadOnly}, DefaultAccess: ReadOnly,
		Boundaries: []Boundary{{ID: "no_delivery", Description: "Cannot contact the user"}},
	}
}

func TestRegistrySeparatesStableRoleFromFrozenProviderBinding(t *testing.T) {
	provider := testProvider{descriptor: testProviderDescriptor("eri_native")}
	registry, err := NewRegistry(DefaultRoles(), []Binding{{RoleID: "intern", ProviderID: "eri_native"}}, provider)
	if err != nil {
		t.Fatal(err)
	}
	prepared, action, err := registry.Prepare(context.Background(), Request{RoleID: "intern", Objective: "inspect evidence"})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.RoleID != "intern" || prepared.ProviderID != "eri_native" || prepared.Access != ReadOnly || action.Target != "provider:eri_native" {
		t.Fatalf("prepared=%+v action=%+v", prepared, action)
	}
	guide := registry.RoutingGuide()
	if !strings.Contains(guide, "intern") || strings.Contains(guide, "eri_native") || strings.Contains(strings.ToLower(guide), "provider") {
		t.Fatalf("model-facing routing guide leaked runtime details: %q", guide)
	}
}

func TestRegistryDoesNotAdvertiseRoleWhoseProviderIsUnavailable(t *testing.T) {
	provider := testProvider{descriptor: testProviderDescriptor("eri_native")}
	registry, err := NewRegistry(DefaultRoles(), []Binding{
		{RoleID: "intern", ProviderID: "eri_native"},
		{RoleID: "engineering_team", ProviderID: "codex"},
	}, provider)
	if err != nil {
		t.Fatal(err)
	}
	if got := registry.Roles(); len(got) != 1 || got[0] != "intern" {
		t.Fatalf("available roles = %v", got)
	}
}

func TestRegistryRejectsIncompleteProviderAuthority(t *testing.T) {
	_, err := NewRegistry(DefaultRoles(), []Binding{{RoleID: "intern", ProviderID: "broken"}}, testProvider{descriptor: ProviderDescriptor{ID: "broken", SupportedRoles: []string{"intern"}, Execution: Background}})
	if err == nil {
		t.Fatal("incomplete provider descriptor was accepted")
	}
}

func TestRegistryRejectsRoleProviderCapabilityMismatch(t *testing.T) {
	provider := testProvider{descriptor: testProviderDescriptor("eri_native")}
	_, err := NewRegistry(DefaultRoles(), []Binding{{RoleID: "engineering_team", ProviderID: "eri_native"}}, provider)
	if err == nil {
		t.Fatal("incompatible role/provider binding was accepted")
	}
}
