package cli

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/z-chenhao/eri/internal/config"
	"github.com/z-chenhao/eri/internal/daemon"
	"github.com/z-chenhao/eri/internal/observability"
	"github.com/z-chenhao/eri/internal/providersecret"
)

const launchdLabel = "io.github.z-chenhao.eri"

func runServiceInstall(ctx context.Context, cfg config.Config, stdin io.Reader, stdout, stderr io.Writer) int {
	if runtime.GOOS != "darwin" {
		fmt.Fprintln(stderr, "eri install: launchd installation is supported on macOS only")
		return 1
	}
	if err := daemon.PrepareDataRoot(cfg.DataRoot); err != nil {
		fmt.Fprintln(stderr, "eri install:", observability.SafeError(err))
		return 1
	}
	// Shell model overrides are deliberately not copied into launchd. Persist a
	// user-confirmed profile now so the installed service has the same binding.
	if cfg.ModelEnvironmentOverride {
		cfg.ModelConfigured = false
		cfg.ModelEnvironmentOverride = false
		cfg.DeepSeekKeySet = false
	}
	broker := providersecret.NewProcess(cfg.DataRoot, cfg.ProviderBrokerSocket)
	defer broker.Close()
	var err error
	cfg, err = ensureModelConfigured(ctx, cfg, broker, stdin, stdout, false)
	if err != nil {
		fmt.Fprintln(stderr, "eri install setup:", observability.SafeError(err))
		return 1
	}
	executable, err := os.Executable()
	if err == nil {
		executable, err = filepath.EvalSymlinks(executable)
	}
	if err != nil {
		fmt.Fprintln(stderr, "eri install:", err)
		return 1
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(stderr, "eri install:", err)
		return 1
	}
	directory := filepath.Join(home, "Library", "LaunchAgents")
	path := filepath.Join(directory, launchdLabel+".plist")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		fmt.Fprintln(stderr, "eri install:", err)
		return 1
	}
	if err := os.MkdirAll(filepath.Join(cfg.DataRoot, "logs"), 0o700); err != nil {
		fmt.Fprintln(stderr, "eri install:", err)
		return 1
	}
	body := launchdPlist(executable, cfg)
	temporary, err := os.CreateTemp(directory, launchdLabel+"-*.plist")
	if err == nil {
		if chmodErr := temporary.Chmod(0o600); chmodErr != nil {
			err = chmodErr
		} else if _, writeErr := temporary.Write(body); writeErr != nil {
			err = writeErr
		} else if closeErr := temporary.Close(); closeErr != nil {
			err = closeErr
		} else {
			err = os.Rename(temporary.Name(), path)
		}
	}
	if temporary != nil {
		_ = temporary.Close()
		if err != nil {
			_ = os.Remove(temporary.Name())
		}
	}
	if err != nil {
		fmt.Fprintln(stderr, "eri install:", err)
		return 1
	}
	domain := "gui/" + strconv.Itoa(os.Getuid())
	_ = exec.CommandContext(ctx, "launchctl", "bootout", domain+"/"+launchdLabel).Run()
	if output, err := exec.CommandContext(ctx, "launchctl", "bootstrap", domain, path).CombinedOutput(); err != nil {
		fmt.Fprintf(stderr, "eri install: launchctl bootstrap: %v: %s\n", err, strings.TrimSpace(string(output)))
		return 1
	}
	if output, err := exec.CommandContext(ctx, "launchctl", "kickstart", "-k", domain+"/"+launchdLabel).CombinedOutput(); err != nil {
		fmt.Fprintf(stderr, "eri install: launchctl kickstart: %v: %s\n", err, strings.TrimSpace(string(output)))
		return 1
	}
	fmt.Fprintln(stdout, "Eri is installed and running as a per-user launchd service.")
	fmt.Fprintf(stdout, "Open http://%s to start chatting.\n", cfg.ConversationAddr)
	return 0
}

func runServiceUninstall(ctx context.Context, _ config.Config, stdout, stderr interface{ Write([]byte) (int, error) }) int {
	if runtime.GOOS != "darwin" {
		fmt.Fprintln(stderr, "eri uninstall: launchd installation is supported on macOS only")
		return 1
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(stderr, "eri uninstall:", err)
		return 1
	}
	path := filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
	domain := "gui/" + strconv.Itoa(os.Getuid())
	if output, err := exec.CommandContext(ctx, "launchctl", "bootout", domain+"/"+launchdLabel).CombinedOutput(); err != nil && !strings.Contains(string(output), "Could not find service") {
		fmt.Fprintf(stderr, "eri uninstall: launchctl bootout: %v: %s\n", err, strings.TrimSpace(string(output)))
		return 1
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(stderr, "eri uninstall:", err)
		return 1
	}
	fmt.Fprintln(stdout, "Eri's launchd service was removed. User data was not deleted.")
	return 0
}

func launchdPlist(executable string, cfg config.Config) []byte {
	escape := func(value string) string {
		var builder strings.Builder
		_ = xml.EscapeText(&builder, []byte(value))
		return builder.String()
	}
	return []byte(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>` + launchdLabel + `</string>
  <key>ProgramArguments</key><array><string>` + escape(executable) + `</string><string>daemon</string></array>
	  <key>WorkingDirectory</key><string>` + escape(cfg.WorkspaceRoot) + `</string>
  <key>EnvironmentVariables</key><dict>
    <key>ERI_DATA_ROOT</key><string>` + escape(cfg.DataRoot) + `</string>
    <key>ERI_WORKSPACE_ROOT</key><string>` + escape(cfg.WorkspaceRoot) + `</string>
    <key>ERI_GOOGLE_AUTH_BROKER</key><string>` + escape("unix://"+filepath.Join(cfg.DataRoot, "runtime", "google-auth-issuer.sock")) + `</string>
    <key>ERI_GOOGLE_AUTH_REDEMPTION_BROKER</key><string>` + escape("unix://"+filepath.Join(cfg.DataRoot, "runtime", "google-auth-redemption.sock")) + `</string>
  </dict>
	  <key>RunAtLoad</key><true/>
	  <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
	  <key>ProcessType</key><string>Background</string>
	  <key>Umask</key><integer>63</integer>
	  <key>StandardOutPath</key><string>/dev/null</string>
	  <key>StandardErrorPath</key><string>` + escape(filepath.Join(cfg.DataRoot, "logs", "bootstrap.log")) + `</string>
	  <key>ThrottleInterval</key><integer>10</integer>
</dict></plist>
`)
}
