package main

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const brokerLaunchdLabel = "io.github.z-chenhao.eri.google-auth-broker"

type brokerServiceConfig struct {
	ClientConfig     string
	IssuerSocket     string
	RedemptionSocket string
	Callback         string
	DataRoot         string
}

func installBrokerService(ctx context.Context, cfg brokerServiceConfig) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("launchd installation is supported on macOS only")
	}
	executable, err := os.Executable()
	if err == nil {
		executable, err = filepath.EvalSymlinks(executable)
	}
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	directory := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	path := filepath.Join(directory, brokerLaunchdLabel+".plist")
	if err := atomicWritePlist(path, brokerLaunchdPlist(executable, cfg)); err != nil {
		return err
	}
	domain := "gui/" + strconv.Itoa(os.Getuid())
	_ = exec.CommandContext(ctx, "launchctl", "bootout", domain+"/"+brokerLaunchdLabel).Run()
	if output, err := exec.CommandContext(ctx, "launchctl", "bootstrap", domain, path).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootstrap: %w: %s", err, strings.TrimSpace(string(output)))
	}
	if output, err := exec.CommandContext(ctx, "launchctl", "kickstart", "-k", domain+"/"+brokerLaunchdLabel).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl kickstart: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func uninstallBrokerService(ctx context.Context) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("launchd installation is supported on macOS only")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	domain := "gui/" + strconv.Itoa(os.Getuid())
	if output, err := exec.CommandContext(ctx, "launchctl", "bootout", domain+"/"+brokerLaunchdLabel).CombinedOutput(); err != nil && !strings.Contains(string(output), "Could not find service") {
		return fmt.Errorf("launchctl bootout: %w: %s", err, strings.TrimSpace(string(output)))
	}
	path := filepath.Join(home, "Library", "LaunchAgents", brokerLaunchdLabel+".plist")
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func brokerLaunchdPlist(executable string, cfg brokerServiceConfig) []byte {
	escape := func(value string) string {
		var builder strings.Builder
		_ = xml.EscapeText(&builder, []byte(value))
		return builder.String()
	}
	return []byte(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>` + brokerLaunchdLabel + `</string>
  <key>ProgramArguments</key><array>
    <string>` + escape(executable) + `</string>
    <string>--client-config</string><string>` + escape(cfg.ClientConfig) + `</string>
    <string>--issuer-socket</string><string>` + escape(cfg.IssuerSocket) + `</string>
    <string>--redemption-socket</string><string>` + escape(cfg.RedemptionSocket) + `</string>
    <string>--callback</string><string>` + escape(cfg.Callback) + `</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
  <key>ProcessType</key><string>Background</string>
	  <key>EnvironmentVariables</key><dict><key>ERI_DATA_ROOT</key><string>` + escape(cfg.DataRoot) + `</string></dict>
	  <key>Umask</key><integer>63</integer>
	  <key>StandardOutPath</key><string>/dev/null</string>
	  <key>StandardErrorPath</key><string>` + escape(filepath.Join(cfg.DataRoot, "logs", "google-auth-broker-bootstrap.log")) + `</string>
  <key>ThrottleInterval</key><integer>10</integer>
</dict></plist>
`)
}

func atomicWritePlist(path string, body []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), brokerLaunchdLabel+"-*.plist")
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
