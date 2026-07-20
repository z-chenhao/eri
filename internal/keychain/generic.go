// Package keychain contains the narrow macOS Keychain command boundary shared
// by Eri's isolated credential owners.
package keychain

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"

	"github.com/creack/pty"
)

// AddGenericPassword stores a value without placing it in argv. security(1)
// reads a missing final -w value from its controlling terminal rather than
// ordinary stdin, so the command must run behind a private pseudo-terminal.
// All terminal output is discarded because it may echo data queued before the
// child disables local echo.
func AddGenericPassword(ctx context.Context, account, service, value string) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("macOS Keychain is unavailable on %s", runtime.GOOS)
	}
	if strings.TrimSpace(account) == "" || strings.TrimSpace(service) == "" || value == "" {
		return fmt.Errorf("Keychain account, service, and value are required")
	}
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("Keychain value must be a single line")
	}
	command := addGenericPasswordCommand(ctx, account, service)
	terminal, err := pty.Start(command)
	if err != nil {
		return fmt.Errorf("start private Keychain terminal: %w", err)
	}
	// security(1) flushes input before it disables terminal echo and requires
	// the value twice. Wait for each C-locale prompt before writing; otherwise
	// eagerly queued input is discarded and leaves the process blocked.
	reader := bufio.NewReader(terminal)
	if err := expectPrompt(reader, "password data for new item:"); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		_ = terminal.Close()
		return err
	}
	if _, err := io.WriteString(terminal, value+"\r"); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		_ = terminal.Close()
		return fmt.Errorf("write private Keychain terminal: %w", err)
	}
	if err := expectPrompt(reader, "retype password for new item:"); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		_ = terminal.Close()
		return err
	}
	drained := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, reader)
		close(drained)
	}()
	if _, err := io.WriteString(terminal, value+"\r"); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		_ = terminal.Close()
		<-drained
		return fmt.Errorf("write private Keychain terminal: %w", err)
	}
	waitErr := command.Wait()
	_ = terminal.Close()
	<-drained
	if waitErr != nil {
		if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return ctx.Err()
		}
		return fmt.Errorf("security add-generic-password failed: %w", waitErr)
	}
	return nil
}

func expectPrompt(reader *bufio.Reader, expected string) error {
	var prompt strings.Builder
	for prompt.Len() < 512 {
		value, err := reader.ReadByte()
		if err != nil {
			return fmt.Errorf("read private Keychain terminal prompt: %w", err)
		}
		prompt.WriteByte(value)
		if value == ':' {
			if strings.Contains(prompt.String(), expected) {
				return nil
			}
			return fmt.Errorf("security returned an unexpected Keychain prompt")
		}
	}
	return fmt.Errorf("security Keychain prompt exceeded the safe limit")
}

func addGenericPasswordCommand(ctx context.Context, account, service string) *exec.Cmd {
	command := exec.CommandContext(ctx, "/usr/bin/security", "add-generic-password", "-a", account, "-s", service, "-U", "-w")
	command.Env = []string{"LANG=C", "LC_ALL=C", "PATH=/usr/bin:/bin"}
	return command
}
