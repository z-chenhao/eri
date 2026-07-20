// Package plugin owns Eri's installed extension manifests and permission
// change boundary. Runtime protocols such as MCP remain adapters below it.
package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	pluginmcp "github.com/z-chenhao/eri/internal/plugin/mcp"
	"github.com/z-chenhao/eri/internal/policy"
	"github.com/z-chenhao/eri/internal/tool"
)

const maxManifestBytes = 1024 * 1024
const maxPluginExportBytes = 16 * 1024 * 1024

var (
	manifestID      = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
	manifestVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$`)
)

type Manifest struct {
	SchemaVersion int         `json:"schema_version"`
	ID            string      `json:"id"`
	Name          string      `json:"name"`
	Version       string      `json:"version"`
	Protocol      string      `json:"protocol"`
	Runtime       Runtime     `json:"runtime"`
	Permissions   Permissions `json:"permissions"`
	Auth          *Auth       `json:"auth,omitempty"`
}

// Auth declares how an out-of-process plugin obtains task-bound credentials.
// It contains only public metadata and an environment-variable name for an
// external broker endpoint; tokens and capability handles are never stored in
// the manifest or passed through model arguments.
type Auth struct {
	Mode                          string              `json:"mode"`
	Provider                      string              `json:"provider"`
	Scopes                        []string            `json:"scopes"`
	ToolScopes                    map[string][]string `json:"tool_scopes,omitempty"`
	PublicTools                   []string            `json:"public_tools,omitempty"`
	BrokerEndpointEnvironment     string              `json:"broker_endpoint_environment"`
	RedemptionEndpointEnvironment string              `json:"redemption_endpoint_environment"`
}

type Runtime struct {
	Command     string            `json:"command"`
	Arguments   []string          `json:"arguments,omitempty"`
	Environment map[string]string `json:"environment,omitempty"`
}

type Permissions struct {
	DefaultEffect       policy.EffectClass            `json:"default_effect"`
	ToolEffects         map[string]policy.EffectClass `json:"tool_effects,omitempty"`
	SendsDataExternally bool                          `json:"sends_data_externally,omitempty"`
	NetworkDomains      []string                      `json:"network_domains,omitempty"`
	DataCategories      []string                      `json:"data_categories,omitempty"`
}

type InstallPlan struct {
	Manifest           Manifest `json:"manifest"`
	ManifestSHA256     string   `json:"manifest_sha256"`
	SourcePath         string   `json:"source_path"`
	PermissionExpanded bool     `json:"permission_expanded"`
	PreviousVersion    string   `json:"previous_version,omitempty"`
}

type Record struct {
	ID                 string      `json:"id"`
	Name               string      `json:"name"`
	Version            string      `json:"version"`
	Protocol           string      `json:"protocol"`
	ManifestSHA256     string      `json:"manifest_sha256"`
	Permissions        Permissions `json:"permissions"`
	Auth               *Auth       `json:"auth,omitempty"`
	PermissionExpanded bool        `json:"permission_expanded,omitempty"`
	InstalledAt        time.Time   `json:"installed_at"`
}

type Host interface {
	ReloadServer(context.Context, pluginmcp.ServerSpec) ([]tool.Tool, error)
	RemoveServer(string) error
}

type Gateway interface {
	ReplacePluginTools(string, []tool.Tool) error
}

type Manager struct {
	mu        sync.Mutex
	root      string
	workspace string
	host      Host
	gateway   Gateway
	now       func() time.Time
}

func NewManager(root, workspace string, host Host) (*Manager, error) {
	if host == nil {
		return nil, fmt.Errorf("plugin host is required")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	absWorkspace, err := filepath.Abs(workspace)
	if err != nil {
		return nil, err
	}
	if resolved, resolveErr := filepath.EvalSymlinks(absWorkspace); resolveErr == nil {
		absWorkspace = resolved
	}
	if err := os.MkdirAll(absRoot, 0o700); err != nil {
		return nil, fmt.Errorf("create plugin root: %w", err)
	}
	return &Manager{root: absRoot, workspace: absWorkspace, host: host, now: time.Now}, nil
}

func (m *Manager) BindGateway(gateway Gateway) error {
	if gateway == nil {
		return fmt.Errorf("plugin gateway is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gateway = gateway
	return nil
}

func (m *Manager) LoadInstalled(ctx context.Context) error {
	records, err := m.List(ctx)
	if err != nil {
		return err
	}
	for _, record := range records {
		manifest, _, err := m.readActive(record.ID)
		if err != nil {
			return err
		}
		if _, err := m.host.ReloadServer(ctx, manifest.serverSpec()); err != nil {
			return fmt.Errorf("start installed plugin %s: %w", record.ID, err)
		}
	}
	return nil
}

func (m *Manager) PrepareInstall(_ context.Context, sourcePath string) (InstallPlan, error) {
	resolved, err := m.resolveWorkspaceFile(sourcePath)
	if err != nil {
		return InstallPlan{}, err
	}
	manifest, canonical, err := readManifest(resolved)
	if err != nil {
		return InstallPlan{}, err
	}
	plan := InstallPlan{Manifest: manifest, ManifestSHA256: digest(canonical), SourcePath: resolved}
	if previous, _, err := m.readActive(manifest.ID); err == nil {
		plan.PreviousVersion = previous.Version
		plan.PermissionExpanded = manifestExpanded(previous, manifest)
	} else if !errors.Is(err, os.ErrNotExist) {
		return InstallPlan{}, err
	}
	return plan, nil
}

func (m *Manager) Install(ctx context.Context, plan InstallPlan) (Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.gateway == nil {
		return Record{}, fmt.Errorf("plugin gateway is not bound")
	}
	if err := validateManifest(plan.Manifest); err != nil {
		return Record{}, err
	}
	canonical, err := json.Marshal(plan.Manifest)
	if err != nil {
		return Record{}, err
	}
	if digest(canonical) != plan.ManifestSHA256 {
		return Record{}, fmt.Errorf("plugin manifest changed after approval planning")
	}
	activePath := m.activePath(plan.Manifest.ID)
	previousBody, previousErr := os.ReadFile(activePath)
	if previousErr != nil && !errors.Is(previousErr, os.ErrNotExist) {
		return Record{}, previousErr
	}
	versionDirectory := filepath.Join(m.root, plan.Manifest.ID, "versions", plan.Manifest.Version)
	if err := os.MkdirAll(versionDirectory, 0o700); err != nil {
		return Record{}, err
	}
	if err := atomicWrite(filepath.Join(versionDirectory, "manifest.json"), append(canonical, '\n')); err != nil {
		return Record{}, err
	}
	if err := atomicWrite(activePath, append(canonical, '\n')); err != nil {
		return Record{}, err
	}
	newTools, err := m.host.ReloadServer(ctx, plan.Manifest.serverSpec())
	if err == nil {
		err = m.gateway.ReplacePluginTools(plan.Manifest.ID, newTools)
	}
	if err != nil {
		m.rollback(ctx, plan.Manifest.ID, previousBody, previousErr)
		return Record{}, fmt.Errorf("activate plugin %s: %w", plan.Manifest.ID, err)
	}
	return recordFor(plan.Manifest, plan.ManifestSHA256, plan.PermissionExpanded, m.now().UTC()), nil
}

func (m *Manager) rollback(ctx context.Context, id string, previous []byte, previousErr error) {
	if errors.Is(previousErr, os.ErrNotExist) {
		_ = os.Remove(m.activePath(id))
		_ = m.host.RemoveServer(id)
		_ = m.gateway.ReplacePluginTools(id, nil)
		return
	}
	_ = atomicWrite(m.activePath(id), previous)
	manifest, _, err := readManifest(m.activePath(id))
	if err != nil {
		return
	}
	tools, err := m.host.ReloadServer(ctx, manifest.serverSpec())
	if err == nil {
		_ = m.gateway.ReplacePluginTools(id, tools)
	}
}

func (m *Manager) List(_ context.Context) ([]Record, error) {
	entries, err := os.ReadDir(m.root)
	if err != nil {
		return nil, err
	}
	records := make([]Record, 0)
	for _, entry := range entries {
		if !entry.IsDir() || !manifestID.MatchString(entry.Name()) {
			continue
		}
		manifest, body, err := m.readActive(entry.Name())
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		records = append(records, recordFor(manifest, digest(body), false, time.Time{}))
	}
	sort.Slice(records, func(i, j int) bool { return records[i].ID < records[j].ID })
	return records, nil
}

// ExportUserData contributes installed-version manifests and the active
// selection to the user's portable Eri export. It never includes environment
// values or a child process environment.
func (m *Manager) ExportUserData(ctx context.Context) (map[string][]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	total := 0
	err := filepath.WalkDir(m.root, func(filename string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() > maxManifestBytes {
			return fmt.Errorf("plugin registry contains an unsupported file %s", filename)
		}
		relative, err := filepath.Rel(m.root, filename)
		if err != nil {
			return err
		}
		body, err := os.ReadFile(filename)
		if err != nil {
			return err
		}
		total += len(body)
		if total > maxPluginExportBytes {
			return fmt.Errorf("plugin registry export exceeds 16 MiB")
		}
		files[filepath.ToSlash(filepath.Join("plugins", relative))] = body
		return nil
	})
	return files, err
}

// EraseUserData deactivates every extension before removing installed
// manifests, so a clean post-erasure conversation cannot keep using an old
// user-authorized capability from memory.
func (m *Manager) EraseUserData(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	records, err := m.List(ctx)
	if err != nil {
		return err
	}
	for _, record := range records {
		if err := m.host.RemoveServer(record.ID); err != nil {
			return fmt.Errorf("stop plugin %s: %w", record.ID, err)
		}
		if m.gateway != nil {
			if err := m.gateway.ReplacePluginTools(record.ID, nil); err != nil {
				return err
			}
		}
	}
	entries, err := os.ReadDir(m.root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		target := filepath.Join(m.root, entry.Name())
		relative, err := filepath.Rel(m.root, target)
		if err != nil || relative == "." || strings.HasPrefix(relative, "..") {
			return fmt.Errorf("refusing unsafe plugin erase target")
		}
		if err := os.RemoveAll(target); err != nil {
			return err
		}
	}
	return os.Chmod(m.root, 0o700)
}

func (m *Manager) readActive(id string) (Manifest, []byte, error) {
	return readManifest(m.activePath(id))
}

func (m *Manager) activePath(id string) string { return filepath.Join(m.root, id, "active.json") }

func (m *Manager) resolveWorkspaceFile(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("manifest_path is required")
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(m.workspace, value)
	}
	abs, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(m.workspace, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("plugin manifest must be inside the configured workspace")
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxManifestBytes {
		return "", fmt.Errorf("plugin manifest must be a regular file no larger than 1 MiB")
	}
	return resolved, nil
}

func readManifest(path string) (Manifest, []byte, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, nil, err
	}
	if len(body) > maxManifestBytes {
		return Manifest{}, nil, fmt.Errorf("plugin manifest exceeds 1 MiB")
	}
	var manifest Manifest
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, nil, fmt.Errorf("decode plugin manifest: %w", err)
	}
	if manifest.SchemaVersion == 0 {
		manifest.SchemaVersion = 1
	}
	if manifest.Name == "" {
		manifest.Name = manifest.ID
	}
	if manifest.Protocol == "" {
		manifest.Protocol = "mcp_stdio"
	}
	if manifest.Permissions.DefaultEffect == "" {
		manifest.Permissions.DefaultEffect = policy.Privileged
	}
	if err := validateManifest(manifest); err != nil {
		return Manifest{}, nil, err
	}
	canonical, err := json.Marshal(manifest)
	return manifest, canonical, err
}

func validateManifest(manifest Manifest) error {
	if manifest.SchemaVersion != 1 || !manifestID.MatchString(manifest.ID) || !manifestVersion.MatchString(manifest.Version) {
		return fmt.Errorf("plugin manifest requires schema_version 1, a safe id and semantic version")
	}
	if strings.TrimSpace(manifest.Name) == "" || manifest.Protocol != "mcp_stdio" || strings.TrimSpace(manifest.Runtime.Command) == "" {
		return fmt.Errorf("plugin manifest requires name, mcp_stdio protocol and runtime command")
	}
	if !validEffect(manifest.Permissions.DefaultEffect) {
		return fmt.Errorf("plugin manifest has invalid default effect")
	}
	for name, effect := range manifest.Permissions.ToolEffects {
		if strings.TrimSpace(name) == "" || !validEffect(effect) {
			return fmt.Errorf("plugin manifest has invalid tool effect for %q", name)
		}
	}
	for name, value := range manifest.Runtime.Environment {
		if err := pluginmcp.ValidateEnvironmentEntry(name, value); err != nil {
			return err
		}
	}
	if manifest.Auth != nil {
		if manifest.Auth.Mode != "external_broker" || !manifestID.MatchString(manifest.Auth.Provider) {
			return fmt.Errorf("plugin auth requires external_broker mode and a safe provider id")
		}
		if !safeEnvironmentName(manifest.Auth.BrokerEndpointEnvironment) || !safeEnvironmentName(manifest.Auth.RedemptionEndpointEnvironment) || manifest.Auth.BrokerEndpointEnvironment == manifest.Auth.RedemptionEndpointEnvironment {
			return fmt.Errorf("plugin auth requires distinct issuer and redemption endpoint environment variable names")
		}
		for name := range manifest.Runtime.Environment {
			if name == manifest.Auth.BrokerEndpointEnvironment || name == manifest.Auth.RedemptionEndpointEnvironment {
				return fmt.Errorf("plugin runtime cannot inherit an auth broker endpoint directly")
			}
		}
		if len(manifest.Auth.Scopes) == 0 || len(manifest.Auth.Scopes) > 32 {
			return fmt.Errorf("plugin auth requires between 1 and 32 scopes")
		}
		for _, scope := range manifest.Auth.Scopes {
			if strings.TrimSpace(scope) == "" || len(scope) > 512 || strings.ContainsAny(scope, "\r\n\x00") {
				return fmt.Errorf("plugin auth contains an invalid scope")
			}
		}
		allowed := make(map[string]struct{}, len(manifest.Auth.Scopes))
		for _, scope := range manifest.Auth.Scopes {
			allowed[scope] = struct{}{}
		}
		for toolName, scopes := range manifest.Auth.ToolScopes {
			if strings.TrimSpace(toolName) == "" || len(scopes) == 0 || len(scopes) > len(manifest.Auth.Scopes) {
				return fmt.Errorf("plugin auth contains invalid tool scopes for %q", toolName)
			}
			for _, scope := range scopes {
				if _, ok := allowed[scope]; !ok {
					return fmt.Errorf("plugin auth tool %q requests a scope outside the declared maximum", toolName)
				}
			}
		}
		public := make(map[string]struct{}, len(manifest.Auth.PublicTools))
		for _, toolName := range manifest.Auth.PublicTools {
			if strings.TrimSpace(toolName) == "" {
				return fmt.Errorf("plugin auth contains an invalid public tool")
			}
			if _, duplicate := public[toolName]; duplicate {
				return fmt.Errorf("plugin auth contains a duplicate public tool")
			}
			if _, scoped := manifest.Auth.ToolScopes[toolName]; scoped {
				return fmt.Errorf("plugin auth tool %q cannot be both public and scoped", toolName)
			}
			public[toolName] = struct{}{}
		}
	}
	return nil
}

func (m Manifest) serverSpec() pluginmcp.ServerSpec {
	return pluginmcp.ServerSpec{
		ID: m.ID, Version: m.Version, Command: m.Runtime.Command, Arguments: m.Runtime.Arguments,
		Environment: m.Runtime.Environment, DefaultEffect: m.Permissions.DefaultEffect,
		ToolEffects: m.Permissions.ToolEffects, External: m.Permissions.SendsDataExternally,
		Auth: authSpec(m.Auth),
	}
}

func authSpec(auth *Auth) *pluginmcp.AuthSpec {
	if auth == nil {
		return nil
	}
	toolScopes := make(map[string][]string, len(auth.ToolScopes))
	for name, scopes := range auth.ToolScopes {
		toolScopes[name] = append([]string(nil), scopes...)
	}
	return &pluginmcp.AuthSpec{
		Mode: auth.Mode, Provider: auth.Provider, Scopes: append([]string(nil), auth.Scopes...), ToolScopes: toolScopes,
		PublicTools: append([]string(nil), auth.PublicTools...), BrokerEndpointEnvironment: auth.BrokerEndpointEnvironment,
		RedemptionEndpointEnvironment: auth.RedemptionEndpointEnvironment,
	}
}

func manifestExpanded(previous, next Manifest) bool {
	if !previous.Permissions.SendsDataExternally && next.Permissions.SendsDataExternally {
		return true
	}
	if authExpanded(previous.Auth, next.Auth) {
		return true
	}
	if environmentExpanded(previous.Runtime.Environment, next.Runtime.Environment) ||
		!subset(next.Permissions.NetworkDomains, previous.Permissions.NetworkDomains) ||
		!subset(next.Permissions.DataCategories, previous.Permissions.DataCategories) {
		return true
	}
	for name, nextEffect := range next.Permissions.ToolEffects {
		previousEffect, existed := previous.Permissions.ToolEffects[name]
		if !existed {
			previousEffect = previous.Permissions.DefaultEffect
		}
		if effectRank(nextEffect) > effectRank(previousEffect) {
			return true
		}
	}
	return effectRank(next.Permissions.DefaultEffect) > effectRank(previous.Permissions.DefaultEffect)
}

func environmentExpanded(previous, next map[string]string) bool {
	for name, value := range next {
		if previous[name] != value {
			return true
		}
	}
	return false
}

func authExpanded(previous, next *Auth) bool {
	if next == nil {
		return false
	}
	if previous == nil || previous.Mode != next.Mode || previous.Provider != next.Provider || previous.BrokerEndpointEnvironment != next.BrokerEndpointEnvironment || previous.RedemptionEndpointEnvironment != next.RedemptionEndpointEnvironment {
		return true
	}
	if !subset(next.Scopes, previous.Scopes) {
		return true
	}
	toolNames := make(map[string]struct{}, len(previous.ToolScopes)+len(next.ToolScopes))
	for name := range previous.ToolScopes {
		toolNames[name] = struct{}{}
	}
	for name := range next.ToolScopes {
		toolNames[name] = struct{}{}
	}
	for name := range toolNames {
		previousScopes := previous.Scopes
		if scoped, ok := previous.ToolScopes[name]; ok {
			previousScopes = scoped
		}
		nextScopes := next.Scopes
		if scoped, ok := next.ToolScopes[name]; ok {
			nextScopes = scoped
		}
		if !subset(nextScopes, previousScopes) {
			return true
		}
	}
	return false
}

func subset(next, previous []string) bool {
	allowed := make(map[string]struct{}, len(previous))
	for _, value := range previous {
		allowed[value] = struct{}{}
	}
	for _, value := range next {
		if _, ok := allowed[value]; !ok {
			return false
		}
	}
	return true
}

func effectRank(effect policy.EffectClass) int {
	switch effect {
	case policy.ReadOnly:
		return 0
	case policy.Reversible:
		return 1
	case policy.Communication:
		return 2
	case policy.Destructive, policy.Financial, policy.Privileged:
		return 3
	default:
		return 4
	}
}

func validEffect(effect policy.EffectClass) bool { return effectRank(effect) < 4 }

func safeEnvironmentName(name string) bool {
	if name == "" || strings.Contains(name, "=") {
		return false
	}
	for index, char := range name {
		if (char >= 'A' && char <= 'Z') || char == '_' || (index > 0 && char >= '0' && char <= '9') {
			continue
		}
		return false
	}
	return true
}

func atomicWrite(path string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".pending-*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(body); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func digest(body []byte) string {
	hash := sha256.Sum256(body)
	return hex.EncodeToString(hash[:])
}

func recordFor(manifest Manifest, hash string, expanded bool, installedAt time.Time) Record {
	return Record{
		ID: manifest.ID, Name: manifest.Name, Version: manifest.Version, Protocol: manifest.Protocol,
		ManifestSHA256: hash, Permissions: manifest.Permissions, Auth: manifest.Auth, PermissionExpanded: expanded, InstalledAt: installedAt,
	}
}
