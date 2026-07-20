package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/z-chenhao/eri/internal/skill"
)

func TestSkillsToolLoadsInstructionsThenReferencedResource(t *testing.T) {
	catalog, err := skill.Open(fstest.MapFS{
		"writing/SKILL.md":            &fstest.MapFile{Data: []byte("---\nname: writing\ndescription: Draft finished material.\n---\nUse references/style.md when polishing.")},
		"writing/references/style.md": &fstest.MapFile{Data: []byte("Prefer direct sentences.")},
	}, skill.Options{})
	if err != nil {
		t.Fatal(err)
	}
	loader, err := NewSkills(context.Background(), catalog)
	if err != nil {
		t.Fatal(err)
	}
	descriptor := loader.Descriptor()
	nameSchema := descriptor.InputSchema["properties"].(map[string]any)["name"].(map[string]any)
	if values := nameSchema["enum"].([]string); len(values) != 1 || values[0] != "writing" {
		t.Fatalf("name enum = %+v", values)
	}

	prepared, err := loader.Prepare(context.Background(), json.RawMessage(`{"operation":"load","name":"writing"}`))
	if err != nil {
		t.Fatal(err)
	}
	result, err := loader.Execute(context.Background(), prepared)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result.Output), "<skill_content>") || !strings.Contains(string(result.Output), "Use references/style.md") || strings.Contains(string(result.Output), "Prefer direct sentences") {
		t.Fatalf("load output = %s", result.Output)
	}

	prepared, err = loader.Prepare(context.Background(), json.RawMessage(`{"operation":"read_resource","name":"writing","path":"references/style.md"}`))
	if err != nil {
		t.Fatal(err)
	}
	result, err = loader.Execute(context.Background(), prepared)
	if err != nil || !strings.Contains(string(result.Output), "Prefer direct sentences") {
		t.Fatalf("resource output=%s err=%v", result.Output, err)
	}
}

func TestSkillsToolRejectsUnknownAndTraversal(t *testing.T) {
	catalog, err := skill.Open(fstest.MapFS{
		"safe/SKILL.md": &fstest.MapFile{Data: []byte("---\nname: safe\ndescription: Safe skill.\n---\ninstructions")},
	}, skill.Options{})
	if err != nil {
		t.Fatal(err)
	}
	loader, err := NewSkills(context.Background(), catalog)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{"operation":"load","name":"missing"}`,
		`{"operation":"read_resource","name":"safe","path":"../SKILL.md"}`,
	} {
		if _, err := loader.Prepare(context.Background(), json.RawMessage(body)); err == nil {
			t.Fatalf("unsafe request accepted: %s", body)
		}
	}
}
