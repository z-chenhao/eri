// Package skill discovers and progressively discloses packages that follow the
// open Agent Skills specification. A skill is context, not a runtime, workflow,
// tool, or agent.
package skill

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	maxSkillBytes    = 256 * 1024
	maxResourceBytes = 1024 * 1024
	maxSkills        = 256
)

var standardName = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
var explicitMention = regexp.MustCompile(`(?:^|[[:space:]])\$([a-z0-9]+(?:-[a-z0-9]+)*)`)

// Metadata is the portable SKILL.md frontmatter. DisableModelInvocation is a
// widely used client extension; unknown extensions are deliberately ignored.
type Metadata struct {
	Name                   string         `yaml:"name" json:"name"`
	Description            string         `yaml:"description" json:"description"`
	License                string         `yaml:"license,omitempty" json:"license,omitempty"`
	Compatibility          string         `yaml:"compatibility,omitempty" json:"compatibility,omitempty"`
	Metadata               map[string]any `yaml:"metadata,omitempty" json:"metadata,omitempty"`
	AllowedTools           string         `yaml:"allowed-tools,omitempty" json:"allowed_tools,omitempty"`
	DisableModelInvocation bool           `yaml:"disable-model-invocation,omitempty" json:"disable_model_invocation,omitempty"`
}

type Scope string

const (
	ScopeBuiltin Scope = "builtin"
	ScopeUser    Scope = "user"
)

// Info is the small, tier-one record disclosed at session start.
type Info struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Location    string `json:"location"`
	Scope       Scope  `json:"scope"`
}

// Document is produced only when the model or user activates a skill.
type Document struct {
	Info
	Compatibility string   `json:"compatibility,omitempty"`
	Instructions  string   `json:"instructions"`
	Resources     []string `json:"resources,omitempty"`
	Directory     string   `json:"directory"`
}

type Diagnostic struct {
	Location string `json:"location"`
	Message  string `json:"message"`
}

type Options struct {
	UserSkillRoot string
}

type entry struct {
	info      Info
	metadata  Metadata
	fsys      fs.FS
	directory string
	realRoot  string
	priority  int
}

type Catalog struct {
	entries     map[string]entry
	diagnostics []Diagnostic
}

// Open loads embedded standard skills plus the Eri-specific user directory.
// The user location deterministically overrides a built-in with the same name.
func Open(builtin fs.FS, options Options) (*Catalog, error) {
	catalog := &Catalog{entries: make(map[string]entry)}
	if builtin != nil {
		if err := catalog.discoverFS(builtin, ".", "builtin://skills", ScopeBuiltin, 0, true); err != nil {
			return nil, err
		}
	}
	if root := strings.TrimSpace(options.UserSkillRoot); root != "" {
		catalog.discoverDirectory(root, ScopeUser, 100)
	}
	return catalog, nil
}

func (c *Catalog) discoverDirectory(directory string, scope Scope, priority int) {
	directory = strings.TrimSpace(directory)
	if directory == "" {
		return
	}
	absolute, err := filepath.Abs(directory)
	if err != nil {
		c.diagnostic(directory, err.Error())
		return
	}
	real, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		if !os.IsNotExist(err) {
			c.diagnostic(absolute, err.Error())
		}
		return
	}
	if err := c.discoverFS(os.DirFS(real), ".", real, scope, priority, false, real); err != nil {
		c.diagnostic(real, err.Error())
	}
}

func (c *Catalog) discoverFS(fsys fs.FS, root, displayRoot string, scope Scope, priority int, strict bool, realRoots ...string) error {
	found := 0
	realRoot := ""
	if len(realRoots) > 0 {
		realRoot = realRoots[0]
	}
	err := fs.WalkDir(fsys, root, func(filePath string, item fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if strict {
				return walkErr
			}
			c.diagnostic(joinDisplay(displayRoot, filePath), walkErr.Error())
			return nil
		}
		if item.IsDir() || item.Name() != "SKILL.md" {
			return nil
		}
		if item.Type()&fs.ModeSymlink != 0 {
			c.diagnostic(joinDisplay(displayRoot, filePath), "symlinked SKILL.md is not discovered")
			return nil
		}
		if found >= maxSkills {
			return fs.SkipAll
		}
		body, err := fs.ReadFile(fsys, filePath)
		if err != nil {
			return c.handleDiscoveryError(strict, joinDisplay(displayRoot, filePath), err)
		}
		metadata, _, err := parse(body)
		if err != nil {
			return c.handleDiscoveryError(strict, joinDisplay(displayRoot, filePath), err)
		}
		directory := path.Dir(filePath)
		location := joinDisplay(displayRoot, filePath)
		if metadata.Name == "" {
			if strict {
				return c.handleDiscoveryError(true, location, fmt.Errorf("SKILL.md requires name"))
			}
			metadata.Name = path.Base(directory)
			c.diagnostic(location, fmt.Sprintf("name is missing; derived %q from the directory for Claude Code compatibility", metadata.Name))
		}
		if issue := validatePortable(metadata, path.Base(directory)); issue != "" {
			c.diagnostic(location, issue)
		}
		candidate := entry{
			info:     Info{Name: metadata.Name, Description: metadata.Description, Location: location, Scope: scope},
			metadata: metadata, fsys: fsys, directory: directory, realRoot: realRoot, priority: priority,
		}
		if current, exists := c.entries[metadata.Name]; exists {
			if current.priority > priority {
				c.diagnostic(location, fmt.Sprintf("skill %q is shadowed by %s", metadata.Name, current.info.Location))
				return nil
			}
			c.diagnostic(current.info.Location, fmt.Sprintf("skill %q is shadowed by %s", metadata.Name, location))
		}
		c.entries[metadata.Name] = candidate
		found++
		return nil
	})
	return err
}

func (c *Catalog) handleDiscoveryError(strict bool, location string, err error) error {
	if strict {
		return fmt.Errorf("load bundled skill %s: %w", location, err)
	}
	c.diagnostic(location, err.Error())
	return nil
}

func (c *Catalog) diagnostic(location, message string) {
	c.diagnostics = append(c.diagnostics, Diagnostic{Location: location, Message: message})
}

func parse(body []byte) (Metadata, string, error) {
	if len(body) == 0 || len(body) > maxSkillBytes {
		return Metadata{}, "", fmt.Errorf("SKILL.md must be between 1 byte and %d bytes", maxSkillBytes)
	}
	normalized := bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n"))
	if !bytes.HasPrefix(normalized, []byte("---\n")) {
		return Metadata{}, "", fmt.Errorf("SKILL.md must start with YAML frontmatter")
	}
	closing := bytes.Index(normalized[4:], []byte("\n---"))
	if closing < 0 {
		return Metadata{}, "", fmt.Errorf("SKILL.md frontmatter is not closed")
	}
	closing += 4
	end := closing + len("\n---")
	if len(normalized) > end && normalized[end] != '\n' {
		return Metadata{}, "", fmt.Errorf("SKILL.md closing delimiter must occupy its own line")
	}
	var metadata Metadata
	if err := yaml.Unmarshal(normalized[4:closing], &metadata); err != nil {
		return Metadata{}, "", fmt.Errorf("decode SKILL.md frontmatter: %w", err)
	}
	metadata.Name = strings.TrimSpace(metadata.Name)
	metadata.Description = strings.TrimSpace(metadata.Description)
	metadata.License = strings.TrimSpace(metadata.License)
	metadata.Compatibility = strings.TrimSpace(metadata.Compatibility)
	metadata.AllowedTools = strings.TrimSpace(metadata.AllowedTools)
	if metadata.Description == "" {
		return Metadata{}, "", fmt.Errorf("SKILL.md requires a non-empty description")
	}
	instructions := strings.TrimSpace(string(normalized[end:]))
	return metadata, instructions, nil
}

func validatePortable(metadata Metadata, directoryName string) string {
	issues := make([]string, 0, 4)
	if len(metadata.Name) > 64 || !standardName.MatchString(metadata.Name) {
		issues = append(issues, "name does not satisfy the Agent Skills naming rules")
	}
	if metadata.Name != directoryName {
		issues = append(issues, fmt.Sprintf("name %q does not match directory %q", metadata.Name, directoryName))
	}
	if len(metadata.Description) > 1024 {
		issues = append(issues, "description exceeds 1024 characters")
	}
	if len(metadata.Compatibility) > 500 {
		issues = append(issues, "compatibility exceeds 500 characters")
	}
	return strings.Join(issues, "; ")
}

func (c *Catalog) Available(ctx context.Context) ([]Info, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	items := make([]Info, 0, len(c.entries))
	for _, candidate := range c.entries {
		if !candidate.metadata.DisableModelInvocation {
			items = append(items, candidate.info)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	return items, nil
}

func (c *Catalog) Names(ctx context.Context) ([]string, error) {
	items, err := c.Available(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(items))
	for index := range items {
		names[index] = items[index].Name
	}
	return names, nil
}

func (c *Catalog) Activate(ctx context.Context, name string) (Document, error) {
	return c.activate(ctx, name, false)
}

func (c *Catalog) activate(ctx context.Context, name string, allowModelDisabled bool) (Document, error) {
	if err := ctx.Err(); err != nil {
		return Document{}, err
	}
	candidate, found := c.entries[strings.TrimSpace(name)]
	if !found || (candidate.metadata.DisableModelInvocation && !allowModelDisabled) {
		return Document{}, fmt.Errorf("skill %q is not available for model activation", name)
	}
	body, err := readEntryFile(candidate, path.Join(candidate.directory, "SKILL.md"), maxSkillBytes)
	if err != nil {
		return Document{}, err
	}
	metadata, instructions, err := parse(body)
	if err != nil {
		return Document{}, err
	}
	if metadata.Name == "" {
		metadata.Name = candidate.info.Name
	}
	if metadata.Name != candidate.info.Name {
		return Document{}, fmt.Errorf("skill name changed after discovery; reload the catalog")
	}
	resources, err := listResources(candidate)
	if err != nil {
		return Document{}, err
	}
	return Document{
		Info: candidate.info, Compatibility: metadata.Compatibility,
		Instructions: instructions, Resources: resources,
		Directory: strings.TrimSuffix(candidate.info.Location, "/SKILL.md"),
	}, nil
}

// Explicit resolves portable mention-style invocations. Eri accepts the Codex
// $name form and Pi's /skill:name form without making either client extension
// part of the SKILL.md format.
func (c *Catalog) Explicit(ctx context.Context, input string) ([]Document, error) {
	names := make([]string, 0)
	for _, match := range explicitMention.FindAllStringSubmatch(input, -1) {
		if len(match) > 1 {
			names = append(names, match[1])
		}
	}
	trimmed := strings.TrimSpace(input)
	if strings.HasPrefix(trimmed, "/skill:") {
		fields := strings.Fields(strings.TrimPrefix(trimmed, "/skill:"))
		if len(fields) > 0 {
			names = append(names, trimInvocationPunctuation(fields[0]))
		}
	} else if strings.HasPrefix(trimmed, "/") {
		fields := strings.Fields(strings.TrimPrefix(trimmed, "/"))
		if len(fields) > 0 {
			names = append(names, trimInvocationPunctuation(fields[0]))
		}
	}
	seen := map[string]struct{}{}
	documents := make([]Document, 0, len(names))
	for _, name := range names {
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		if _, found := c.entries[name]; !found {
			continue
		}
		document, err := c.activate(ctx, name, true)
		if err != nil {
			return nil, err
		}
		documents = append(documents, document)
	}
	return documents, nil
}

func trimInvocationPunctuation(value string) string {
	return strings.Trim(value, ".,;:!?，。；：！？")
}

// Render wraps activated content so compaction and downstream evaluators can
// distinguish instructions from user and tool data.
func Render(document Document) string {
	name, _ := xml.Marshal(struct {
		XMLName xml.Name `xml:"skill_name"`
		Value   string   `xml:",chardata"`
	}{Value: document.Name})
	directory, _ := xml.Marshal(struct {
		XMLName xml.Name `xml:"skill_directory"`
		Value   string   `xml:",chardata"`
	}{Value: document.Directory})
	return "<skill_content>\n" + string(name) + "\n" + document.Instructions + "\n" + string(directory) + "\nRelative paths in this skill are relative to the skill directory.\n</skill_content>"
}

func (c *Catalog) ReadResource(ctx context.Context, name, relative string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	candidate, found := c.entries[strings.TrimSpace(name)]
	if !found || candidate.metadata.DisableModelInvocation {
		return nil, fmt.Errorf("skill %q is not available for model activation", name)
	}
	clean := path.Clean(strings.TrimSpace(strings.ReplaceAll(relative, "\\", "/")))
	if clean == "." || clean == "SKILL.md" || path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
		return nil, fmt.Errorf("resource path must stay inside the skill and cannot be SKILL.md")
	}
	filePath := path.Join(candidate.directory, clean)
	body, err := readEntryFile(candidate, filePath, maxResourceBytes)
	if err != nil {
		return nil, err
	}
	if bytes.IndexByte(body, 0) >= 0 {
		return nil, fmt.Errorf("binary resources are not injected into model context")
	}
	return body, nil
}

func listResources(candidate entry) ([]string, error) {
	resources := make([]string, 0)
	err := fs.WalkDir(candidate.fsys, candidate.directory, func(filePath string, item fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if item.IsDir() || item.Type()&fs.ModeSymlink != 0 || filePath == path.Join(candidate.directory, "SKILL.md") {
			return nil
		}
		if len(resources) >= 100 {
			return fs.SkipAll
		}
		relative := strings.TrimPrefix(strings.TrimPrefix(filePath, candidate.directory), "/")
		resources = append(resources, relative)
		return nil
	})
	sort.Strings(resources)
	return resources, err
}

func readEntryFile(candidate entry, filePath string, maximum int64) ([]byte, error) {
	if candidate.realRoot == "" {
		stat, err := fs.Stat(candidate.fsys, filePath)
		if err != nil {
			return nil, err
		}
		if stat.IsDir() || stat.Size() > maximum {
			return nil, fmt.Errorf("resource must be a file no larger than %d bytes", maximum)
		}
		return fs.ReadFile(candidate.fsys, filePath)
	}
	target := filepath.Join(candidate.realRoot, filepath.FromSlash(filePath))
	real, err := filepath.EvalSymlinks(target)
	if err != nil {
		return nil, err
	}
	relative, err := filepath.Rel(candidate.realRoot, real)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return nil, fmt.Errorf("skill resource resolves outside its installed directory")
	}
	stat, err := os.Stat(real)
	if err != nil {
		return nil, err
	}
	if stat.IsDir() || stat.Size() > maximum {
		return nil, fmt.Errorf("resource must be a file no larger than %d bytes", maximum)
	}
	return os.ReadFile(real)
}

func (c *Catalog) Diagnostics() []Diagnostic {
	return append([]Diagnostic(nil), c.diagnostics...)
}

// Prompt discloses tier-one metadata only. Full instructions are available
// through builtin.skills after the model chooses a matching skill.
func (c *Catalog) Prompt(ctx context.Context) (string, error) {
	items, err := c.Available(ctx)
	if err != nil || len(items) == 0 {
		return "", err
	}
	var body strings.Builder
	body.WriteString("\n\nThe following Agent Skills provide specialized instructions. When a task matches a description, call builtin.skills with operation=load and the skill name before proceeding. Load referenced resources only when needed.\n<available_skills>\n")
	for _, item := range items {
		encodedName, _ := xml.Marshal(struct {
			XMLName xml.Name `xml:"name"`
			Value   string   `xml:",chardata"`
		}{Value: item.Name})
		encodedDescription, _ := xml.Marshal(struct {
			XMLName xml.Name `xml:"description"`
			Value   string   `xml:",chardata"`
		}{Value: item.Description})
		entryBody := "  <skill>" + string(encodedName) + string(encodedDescription) + "</skill>\n"
		body.WriteString(entryBody)
	}
	body.WriteString("</available_skills>")
	return body.String(), nil
}

func joinDisplay(root, filePath string) string {
	if strings.Contains(root, "://") {
		return strings.TrimRight(root, "/") + "/" + strings.TrimLeft(filePath, "./")
	}
	return filepath.Join(root, filepath.FromSlash(filePath))
}
