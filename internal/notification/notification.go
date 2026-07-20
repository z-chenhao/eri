// Package notification provides the local operating-system notification
// boundary. Message bodies are passed over stdin and are never placed in
// process arguments or persisted by this adapter.
package notification

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

type Sender interface {
	Send(context.Context, string, string) (string, error)
}

type LocalSender struct{}

func (LocalSender) Send(ctx context.Context, title, body string) (string, error) {
	if runtime.GOOS != "darwin" {
		return "", fmt.Errorf("system notifications are unavailable on %s", runtime.GOOS)
	}
	title = strings.TrimSpace(title)
	body = strings.TrimSpace(body)
	if title == "" || body == "" {
		return "", fmt.Errorf("notification title and body are required")
	}
	if len([]byte(title)) > 256 || len([]byte(body)) > 4096 {
		return "", fmt.Errorf("notification exceeds local size limit")
	}
	script := `display notification "` + appleScriptString(body) + `" with title "` + appleScriptString(title) + `"`
	commandContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	command := exec.CommandContext(commandContext, "/usr/bin/osascript", "-")
	command.Stdin = bytes.NewBufferString(script)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("display local notification: %w", err)
	}
	return "accepted_by_macos_notification_center", nil
}

func appleScriptString(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, `"`, `\"`)
	value = strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ").Replace(value)
	return value
}
