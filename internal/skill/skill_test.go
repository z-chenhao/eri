package skill

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func TestCatalogUsesProgressiveDisclosureAndStandardFrontmatter(t *testing.T) {
	builtin := fstest.MapFS{
		"research/SKILL.md": &fstest.MapFile{Data: []byte(`---
name: research
description: Compare evidence and recommend an option. Use for research decisions.
license: Apache-2.0
compatibility: Requires a web search tool.
metadata:
  author: eri
  requires:
    bins: ["lark-cli"]
  cliHelp: "lark-cli im --help"
allowed-tools: Web Read
unknown-client-extension: accepted
---

# Private instructions

Read references/checklist.md only when evidence quality matters.
`)},
		"research/references/checklist.md": &fstest.MapFile{Data: []byte("Prefer primary evidence.")},
	}
	catalog, err := Open(builtin, Options{})
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := catalog.Prompt(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "<name>research</name>") || !strings.Contains(prompt, "Compare evidence") {
		t.Fatalf("catalog metadata missing: %s", prompt)
	}
	if strings.Contains(prompt, "Private instructions") || strings.Contains(prompt, "Prefer primary evidence") {
		t.Fatalf("catalog eagerly disclosed skill content: %s", prompt)
	}

	document, err := catalog.Activate(context.Background(), "research")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(document.Instructions, "Private instructions") || document.Compatibility != "Requires a web search tool." {
		t.Fatalf("activated document = %+v", document)
	}
	if len(document.Resources) != 1 || document.Resources[0] != "references/checklist.md" {
		t.Fatalf("resources = %+v", document.Resources)
	}
	resource, err := catalog.ReadResource(context.Background(), "research", "references/checklist.md")
	if err != nil || string(resource) != "Prefer primary evidence." {
		t.Fatalf("resource=%q err=%v", resource, err)
	}
	if _, err := catalog.ReadResource(context.Background(), "research", "../SKILL.md"); err == nil {
		t.Fatal("resource path traversal was accepted")
	}
}

func TestPromptDoesNotSilentlyHideAvailableSkillsBehindACatalogBudget(t *testing.T) {
	t.Parallel()
	files := fstest.MapFS{}
	for index := 0; index < 80; index++ {
		name := fmt.Sprintf("skill-%02d", index)
		files[name+"/SKILL.md"] = &fstest.MapFile{Data: []byte(fmt.Sprintf(`---
name: %s
description: %s
---
Load only when selected.
`, name, strings.Repeat("Useful capability description. ", 12)))}
	}
	catalog, err := Open(files, Options{})
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := catalog.Prompt(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"skill-00", "skill-40", "skill-79"} {
		if !strings.Contains(prompt, "<name>"+name+"</name>") {
			t.Fatalf("available skill %q was hidden from the model catalog", name)
		}
	}
}

func TestUserEriSkillOverridesBundledAndOtherSkillRootsAreIgnored(t *testing.T) {
	home := t.TempDir()
	userSkillRoot := filepath.Join(home, ".eri", "skills")
	writeSkill(t, filepath.Join(home, ".agents", "skills", "ignored"), "ignored", "must not load", "ignored body")
	writeSkill(t, filepath.Join(userSkillRoot, "shared"), "shared", "configured description", "configured body")
	builtin := fstest.MapFS{
		"shared/SKILL.md": &fstest.MapFile{Data: []byte("---\nname: shared\ndescription: builtin description\n---\nbuiltin body")},
	}
	catalog, err := Open(builtin, Options{UserSkillRoot: userSkillRoot})
	if err != nil {
		t.Fatal(err)
	}
	document, err := catalog.Activate(context.Background(), "shared")
	if err != nil {
		t.Fatal(err)
	}
	if document.Description != "configured description" || document.Instructions != "configured body" || document.Scope != ScopeUser {
		t.Fatalf("precedence result = %+v", document)
	}
	if _, err := catalog.Activate(context.Background(), "ignored"); err == nil {
		t.Fatal("project .agents skill was loaded")
	}
	if len(catalog.Diagnostics()) != 1 {
		t.Fatalf("shadowing diagnostics = %+v", catalog.Diagnostics())
	}
}

func TestMalformedExternalSkillIsSkippedWithoutBreakingValidSkills(t *testing.T) {
	userSkillRoot := filepath.Join(t.TempDir(), ".eri", "skills")
	writeSkill(t, filepath.Join(userSkillRoot, "valid"), "valid", "valid skill", "instructions")
	invalidDir := filepath.Join(userSkillRoot, "invalid")
	if err := os.MkdirAll(invalidDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(invalidDir, "SKILL.md"), []byte("not frontmatter"), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog, err := Open(fstest.MapFS{}, Options{UserSkillRoot: userSkillRoot})
	if err != nil {
		t.Fatal(err)
	}
	names, err := catalog.Names(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "valid" || len(catalog.Diagnostics()) != 1 {
		t.Fatalf("names=%v diagnostics=%+v", names, catalog.Diagnostics())
	}
}

func TestDisabledSkillIsNotModelInvocable(t *testing.T) {
	builtin := fstest.MapFS{
		"manual/SKILL.md": &fstest.MapFile{Data: []byte("---\nname: manual\ndescription: user only\ndisable-model-invocation: true\n---\nsecret instructions")},
	}
	catalog, err := Open(builtin, Options{})
	if err != nil {
		t.Fatal(err)
	}
	names, err := catalog.Names(context.Background())
	if err != nil || len(names) != 0 {
		t.Fatalf("model-visible names=%v err=%v", names, err)
	}
	if _, err := catalog.Activate(context.Background(), "manual"); err == nil {
		t.Fatal("model activated a user-only skill")
	}
}

func TestExternalSkillResourceCannotEscapeThroughSymlink(t *testing.T) {
	userSkillRoot := filepath.Join(t.TempDir(), ".eri", "skills")
	skillDir := filepath.Join(userSkillRoot, "safe")
	writeSkill(t, skillDir, "safe", "safe skill", "Read references/local.md only.")
	if err := os.MkdirAll(filepath.Join(skillDir, "references"), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "private.txt")
	if err := os.WriteFile(outside, []byte("must not leak"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(skillDir, "references", "leak.md")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink is unavailable: %v", err)
	}
	catalog, err := Open(fstest.MapFS{}, Options{UserSkillRoot: userSkillRoot})
	if err != nil {
		t.Fatal(err)
	}
	document, err := catalog.Activate(context.Background(), "safe")
	if err != nil {
		t.Fatal(err)
	}
	if len(document.Resources) != 0 {
		t.Fatalf("symlink appeared in resources: %+v", document.Resources)
	}
	if body, err := catalog.ReadResource(context.Background(), "safe", "references/leak.md"); err == nil || strings.Contains(string(body), "must not leak") {
		t.Fatalf("external resource escaped: body=%q err=%v", body, err)
	}
}

func TestExternalClaudeSkillCanDeriveNameAndActivateNormally(t *testing.T) {
	userSkillRoot := filepath.Join(t.TempDir(), ".eri", "skills")
	directory := filepath.Join(userSkillRoot, "claude-style")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte("---\ndescription: Claude-compatible skill without an explicit name.\n---\nFollow these instructions."), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog, err := Open(fstest.MapFS{}, Options{UserSkillRoot: userSkillRoot})
	if err != nil {
		t.Fatal(err)
	}
	document, err := catalog.Activate(context.Background(), "claude-style")
	if err != nil || document.Name != "claude-style" {
		t.Fatalf("Claude-style activation document=%+v err=%v", document, err)
	}
	if len(catalog.Diagnostics()) != 1 || !strings.Contains(catalog.Diagnostics()[0].Message, "derived") {
		t.Fatalf("derived-name diagnostics=%+v", catalog.Diagnostics())
	}
}

func writeSkill(t *testing.T, directory, name, description, body string) {
	t.Helper()
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\ndescription: " + description + "\n---\n" + body
	if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Stat(os.DirFS(directory), "SKILL.md"); err != nil {
		t.Fatal(err)
	}
}
