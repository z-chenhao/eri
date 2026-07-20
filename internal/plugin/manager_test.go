package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	pluginmcp "github.com/z-chenhao/eri/internal/plugin/mcp"
	"github.com/z-chenhao/eri/internal/policy"
	"github.com/z-chenhao/eri/internal/tool"
)

func TestManagerInstallsVersionedManifestAndDetectsPermissionExpansion(t *testing.T) {
	workspace := t.TempDir()
	host := &fakePluginHost{}
	gateway := &fakePluginGateway{}
	manager, err := NewManager(filepath.Join(t.TempDir(), "plugins"), workspace, host)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.BindGateway(gateway); err != nil {
		t.Fatal(err)
	}
	firstPath := filepath.Join(workspace, "calendar-v1.json")
	writeManifest(t, firstPath, testManifest("1.0.0", false, nil))
	firstPlan, err := manager.PrepareInstall(context.Background(), firstPath)
	if err != nil || firstPlan.PermissionExpanded {
		t.Fatalf("first plan=%+v err=%v", firstPlan, err)
	}
	record, err := manager.Install(context.Background(), firstPlan)
	if err != nil {
		t.Fatal(err)
	}
	if record.Version != "1.0.0" || host.last.Version != "1.0.0" || gateway.namespace != "calendar" {
		t.Fatalf("record=%+v host=%+v gateway=%q", record, host.last, gateway.namespace)
	}
	if _, err := os.Stat(filepath.Join(manager.root, "calendar", "versions", "1.0.0", "manifest.json")); err != nil {
		t.Fatal(err)
	}
	exported, err := manager.ExportUserData(context.Background())
	if err != nil || len(exported["plugins/calendar/active.json"]) == 0 {
		t.Fatalf("plugin export=%v err=%v", exported, err)
	}
	restartedHost := &fakePluginHost{}
	restarted, err := NewManager(manager.root, workspace, restartedHost)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.LoadInstalled(context.Background()); err != nil || restartedHost.last.Version != "1.0.0" {
		t.Fatalf("restart load spec=%+v err=%v", restartedHost.last, err)
	}

	secondPath := filepath.Join(workspace, "calendar-v2.json")
	writeManifest(t, secondPath, testManifest("2.0.0", true, map[string]string{"CALENDAR_REGION": "us"}))
	secondPlan, err := manager.PrepareInstall(context.Background(), secondPath)
	if err != nil || !secondPlan.PermissionExpanded || secondPlan.PreviousVersion != "1.0.0" {
		t.Fatalf("second plan=%+v err=%v", secondPlan, err)
	}
	if err := manager.EraseUserData(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(manager.root, "calendar", "active.json")); !errors.Is(err, os.ErrNotExist) || host.removed != "calendar" {
		t.Fatalf("plugin registry survived erase, stat=%v removed=%q", err, host.removed)
	}
}

func TestManagerRollsBackActiveManifestWhenReplacementIsUnhealthy(t *testing.T) {
	workspace := t.TempDir()
	host := &fakePluginHost{}
	gateway := &fakePluginGateway{}
	manager, err := NewManager(filepath.Join(t.TempDir(), "plugins"), workspace, host)
	if err != nil {
		t.Fatal(err)
	}
	_ = manager.BindGateway(gateway)
	one := filepath.Join(workspace, "one.json")
	writeManifest(t, one, testManifest("1.0.0", false, nil))
	plan, _ := manager.PrepareInstall(context.Background(), one)
	if _, err := manager.Install(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	two := filepath.Join(workspace, "two.json")
	writeManifest(t, two, testManifest("2.0.0", false, nil))
	plan, _ = manager.PrepareInstall(context.Background(), two)
	host.failVersion = "2.0.0"
	if _, err := manager.Install(context.Background(), plan); err == nil {
		t.Fatal("unhealthy upgrade unexpectedly activated")
	}
	active, _, err := manager.readActive("calendar")
	if err != nil || active.Version != "1.0.0" {
		t.Fatalf("active=%+v err=%v", active, err)
	}
}

func TestManifestRejectsCredentialValuesAndWorkspaceEscape(t *testing.T) {
	workspace := t.TempDir()
	manager, err := NewManager(filepath.Join(t.TempDir(), "plugins"), workspace, &fakePluginHost{})
	if err != nil {
		t.Fatal(err)
	}
	escaped := filepath.Join(t.TempDir(), "plugin.json")
	writeManifest(t, escaped, testManifest("1.0.0", false, nil))
	if _, err := manager.PrepareInstall(context.Background(), escaped); err == nil {
		t.Fatal("manifest outside workspace accepted")
	}
	unsafe := testManifest("1.0.0", false, map[string]string{"DEEPSEEK_API_KEY": "ordinary-value"})
	path := filepath.Join(workspace, "unsafe.json")
	writeManifest(t, path, unsafe)
	if _, err := manager.PrepareInstall(context.Background(), path); err == nil {
		t.Fatal("persisted credential value accepted")
	}
}

func TestManifestAuthDeclaresOnlyBrokerMetadataAndScopeExpansion(t *testing.T) {
	workspace := t.TempDir()
	manager, err := NewManager(filepath.Join(t.TempDir(), "plugins"), workspace, &fakePluginHost{})
	if err != nil {
		t.Fatal(err)
	}
	_ = manager.BindGateway(&fakePluginGateway{})
	first := testManifest("1.0.0", true, nil)
	first.Auth = &Auth{Mode: "external_broker", Provider: "google", Scopes: []string{"calendar.readonly"}, BrokerEndpointEnvironment: "ERI_GOOGLE_AUTH_BROKER", RedemptionEndpointEnvironment: "ERI_GOOGLE_AUTH_REDEMPTION_BROKER"}
	firstPath := filepath.Join(workspace, "auth-v1.json")
	writeManifest(t, firstPath, first)
	plan, err := manager.PrepareInstall(context.Background(), firstPath)
	if err != nil || plan.PermissionExpanded {
		t.Fatalf("initial auth plan=%+v err=%v", plan, err)
	}
	if _, err := manager.Install(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	second := first
	second.Version = "2.0.0"
	second.Auth = &Auth{Mode: "external_broker", Provider: "google", Scopes: []string{"calendar.readonly", "calendar.events"}, BrokerEndpointEnvironment: "ERI_GOOGLE_AUTH_BROKER", RedemptionEndpointEnvironment: "ERI_GOOGLE_AUTH_REDEMPTION_BROKER"}
	secondPath := filepath.Join(workspace, "auth-v2.json")
	writeManifest(t, secondPath, second)
	plan, err = manager.PrepareInstall(context.Background(), secondPath)
	if err != nil || !plan.PermissionExpanded {
		t.Fatalf("expanded auth plan=%+v err=%v", plan, err)
	}
	unsafe := first
	unsafe.Version = "3.0.0"
	unsafe.Auth.BrokerEndpointEnvironment = "BROKER=https://secret"
	unsafePath := filepath.Join(workspace, "auth-unsafe.json")
	writeManifest(t, unsafePath, unsafe)
	if _, err := manager.PrepareInstall(context.Background(), unsafePath); err == nil {
		t.Fatal("credential-like broker value was accepted")
	}
}

func TestManifestAuthToolScopesAreLeastPrivilegeAndExpansionIsDetected(t *testing.T) {
	read := "https://www.googleapis.com/auth/calendar.events.readonly"
	write := "https://www.googleapis.com/auth/calendar.events"
	base := &Auth{
		Mode: "external_broker", Provider: "google", Scopes: []string{read, write},
		ToolScopes:                    map[string][]string{"list_events": {read}, "create_event": {write}},
		BrokerEndpointEnvironment:     "ERI_GOOGLE_AUTH_BROKER",
		RedemptionEndpointEnvironment: "ERI_GOOGLE_AUTH_REDEMPTION_BROKER",
	}
	restricted := *base
	restricted.ToolScopes = map[string][]string{"list_events": {read}, "create_event": {write}}
	if authExpanded(base, &restricted) {
		t.Fatal("equivalent tool-scoped authorization was treated as expansion")
	}
	expanded := restricted
	expanded.ToolScopes = map[string][]string{"list_events": {read, write}, "create_event": {write}}
	if !authExpanded(base, &expanded) {
		t.Fatal("expanding one read tool to write scope was not detected")
	}
	removedRestriction := restricted
	removedRestriction.ToolScopes = map[string][]string{"create_event": {write}}
	if !authExpanded(base, &removedRestriction) {
		t.Fatal("removing a per-tool scope restriction was not detected")
	}
	invalid := testManifest("1.0.0", true, nil)
	invalid.Auth = &Auth{
		Mode: "external_broker", Provider: "google", Scopes: []string{read},
		ToolScopes: map[string][]string{"list_events": {write}}, BrokerEndpointEnvironment: "ERI_GOOGLE_AUTH_BROKER", RedemptionEndpointEnvironment: "ERI_GOOGLE_AUTH_REDEMPTION_BROKER",
	}
	if err := validateManifest(invalid); err == nil {
		t.Fatal("per-tool scope outside the manifest maximum was accepted")
	}
}

func TestOfficialGoogleWorkspaceManifestUsesPerToolScopes(t *testing.T) {
	manifest, _, err := readManifest(filepath.Join("..", "..", "plugins", "google-workspace.json"))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.ID != "google-workspace" || manifest.Auth == nil || manifest.Auth.Provider != "google" || len(manifest.Auth.ToolScopes) != 5 || len(manifest.Auth.PublicTools) != 3 {
		t.Fatalf("Google Workspace manifest = %+v", manifest)
	}
	for toolName, scopes := range manifest.Auth.ToolScopes {
		if len(scopes) != 1 {
			t.Fatalf("tool %s scopes = %v, want exactly one least-privilege scope", toolName, scopes)
		}
	}
}

func testManifest(version string, external bool, environment map[string]string) Manifest {
	return Manifest{
		SchemaVersion: 1, ID: "calendar", Name: "Reference Calendar", Version: version, Protocol: "mcp_stdio",
		Runtime: Runtime{Command: "/usr/bin/true", Environment: environment},
		Permissions: Permissions{
			DefaultEffect: policy.ReadOnly, ToolEffects: map[string]policy.EffectClass{"create_event": policy.Reversible},
			SendsDataExternally: external, NetworkDomains: []string{"calendar.example"}, DataCategories: []string{"calendar_events"},
		},
	}
}

func writeManifest(t *testing.T, path string, manifest Manifest) {
	t.Helper()
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

type fakePluginHost struct {
	last        pluginmcp.ServerSpec
	failVersion string
	removed     string
}

func (h *fakePluginHost) ReloadServer(_ context.Context, spec pluginmcp.ServerSpec) ([]tool.Tool, error) {
	if spec.Version == h.failVersion {
		return nil, errors.New("unhealthy")
	}
	h.last = spec
	return []tool.Tool{fakeInstalledTool{id: "mcp." + spec.ID + ".list_events", version: spec.Version}}, nil
}
func (h *fakePluginHost) RemoveServer(id string) error {
	h.removed = id
	return nil
}

type fakePluginGateway struct{ namespace string }

func (g *fakePluginGateway) ReplacePluginTools(namespace string, _ []tool.Tool) error {
	g.namespace = namespace
	return nil
}

type fakeInstalledTool struct{ id, version string }

func (f fakeInstalledTool) Descriptor() tool.Descriptor {
	return tool.Descriptor{ID: f.id, Version: f.version, Purpose: "test", Source: tool.Plugin}
}
func (f fakeInstalledTool) Prepare(context.Context, json.RawMessage) (tool.Prepared, error) {
	return tool.Prepared{}, nil
}
func (f fakeInstalledTool) Execute(context.Context, tool.Prepared) (tool.Result, error) {
	return tool.Result{}, nil
}
