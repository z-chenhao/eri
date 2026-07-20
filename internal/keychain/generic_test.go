package keychain

import (
	"context"
	"slices"
	"testing"
)

func TestGenericPasswordCommandRequiresTerminalWithoutSecretInArguments(t *testing.T) {
	const secret = "credential-material-must-not-enter-argv"
	command := addGenericPasswordCommand(context.Background(), "owner", "eri")
	if slices.Contains(command.Args, secret) {
		t.Fatalf("secret appears in argv: %v", command.Args)
	}
	if command.Args[len(command.Args)-1] != "-w" || command.Stdin != nil {
		t.Fatalf("security command is not reserved for a private terminal: args=%v stdin=%T", command.Args, command.Stdin)
	}
	if !slices.Contains(command.Env, "LC_ALL=C") {
		t.Fatalf("security prompt locale is not deterministic: %v", command.Env)
	}
}
