package builtin

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/z-chenhao/eri/internal/policy"
)

func TestTerminalRunsSafeCommandAndStripsSecrets(t *testing.T) {
	root := t.TempDir()
	terminal, err := NewTerminal(root)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := terminal.Prepare(context.Background(), json.RawMessage(`{"executable":"pwd"}`))
	if err != nil || prepared.Action.Effect != policy.ReadOnly {
		t.Fatalf("safe command = %+v, err = %v", prepared.Action, err)
	}
	result, err := terminal.Execute(context.Background(), prepared)
	if err != nil || !strings.Contains(string(result.Output), root) {
		t.Fatalf("pwd result = %s, err = %v", result.Output, err)
	}
	t.Setenv("DEEPSEEK_API_KEY", "terminal-must-not-see-this")
	prepared, err = terminal.Prepare(context.Background(), json.RawMessage(`{"executable":"env"}`))
	if err != nil || prepared.Action.Effect != policy.Privileged {
		t.Fatalf("arbitrary command = %+v, err = %v", prepared.Action, err)
	}
	result, err = terminal.Execute(context.Background(), prepared)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(result.Output), os.Getenv("DEEPSEEK_API_KEY")) {
		t.Fatal("parent process secret leaked into terminal subprocess")
	}
}

func TestTerminalRejectsEscapingWorkingDirectory(t *testing.T) {
	terminal, err := NewTerminal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := terminal.Prepare(context.Background(), json.RawMessage(`{"executable":"pwd","working_dir":".."}`)); err == nil {
		t.Fatal("escaping working directory accepted")
	}
}

func TestTerminalDoesNotMisclassifyConfigurableProgramsOrPathsAsReadOnly(t *testing.T) {
	terminal, err := NewTerminal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{
		`{"executable":"git","arguments":["status"]}`,
		`{"executable":"git","arguments":["diff","--no-ext-diff"]}`,
		`{"executable":"rg","arguments":["needle","."]}`,
		`{"executable":"./pwd"}`,
		`{"executable":"./ls","arguments":["-la"]}`,
		`{"executable":"ls","arguments":["linked-directory/"]}`,
		`{"executable":"ls","arguments":["--recursive"]}`,
	} {
		prepared, err := terminal.Prepare(context.Background(), json.RawMessage(input))
		if err != nil {
			t.Fatalf("prepare %s: %v", input, err)
		}
		if prepared.Action.Effect != policy.Privileged {
			t.Fatalf("potentially executable command was classified %s: %s", prepared.Action.Effect, input)
		}
	}
	for _, input := range []string{
		`{"executable":"pwd"}`,
		`{"executable":"ls","arguments":["-la"]}`,
	} {
		prepared, err := terminal.Prepare(context.Background(), json.RawMessage(input))
		if err != nil || prepared.Action.Effect != policy.ReadOnly {
			t.Fatalf("governed local inspection = %+v, err=%v, input=%s", prepared.Action, err, input)
		}
	}
}
