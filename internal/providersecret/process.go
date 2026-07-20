package providersecret

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// Process lazily owns a Broker child when no compatible Broker is already
// listening. It preserves process isolation without requiring a second binary.
type Process struct {
	dataRoot string
	socket   string
	mu       sync.Mutex
	command  *exec.Cmd
	logFile  *os.File
}

func NewProcess(dataRoot, socket string) *Process {
	return &Process{dataRoot: dataRoot, socket: socket}
}

func (p *Process) Ensure(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	client := NewClient(p.socket)
	probe, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	err := client.Health(probe)
	cancel()
	if err == nil {
		return nil
	}
	if p.command != nil {
		return fmt.Errorf("provider secret broker is still starting")
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(p.dataRoot, "logs"), 0o700); err != nil {
		return err
	}
	logFile, err := os.OpenFile(filepath.Join(p.dataRoot, "logs", "provider-secret-broker-bootstrap.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	command := exec.CommandContext(ctx, executable, "provider-secret-broker")
	command.Env = []string{"ERI_DATA_ROOT=" + p.dataRoot, "PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
	command.Stdout = io.Discard
	command.Stderr = logFile
	if err := command.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("start provider secret broker: %w", err)
	}
	p.command = command
	p.logFile = logFile
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		probe, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
		err := client.Health(probe)
		cancel()
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	_ = command.Process.Kill()
	_ = command.Wait()
	p.command = nil
	logFile.Close()
	p.logFile = nil
	return fmt.Errorf("provider secret broker did not become ready")
}

func (p *Process) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.command != nil && p.command.Process != nil {
		_ = p.command.Process.Kill()
		_ = p.command.Wait()
	}
	p.command = nil
	if p.logFile != nil {
		_ = p.logFile.Close()
	}
	p.logFile = nil
}

func Serve(ctx context.Context, dataRoot, socketPath string, logger *slog.Logger) error {
	store, err := NewKeychainStore()
	if err != nil {
		return err
	}
	broker, err := NewBroker(store, logger)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		return err
	}
	if info, err := os.Lstat(socketPath); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("provider broker socket path is not a socket")
		}
		connection, dialErr := net.DialTimeout("unix", socketPath, 150*time.Millisecond)
		if dialErr == nil {
			connection.Close()
			return fmt.Errorf("provider secret broker is already running")
		}
		if err := os.Remove(socketPath); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return err
	}
	defer listener.Close()
	defer os.Remove(socketPath)
	if err := os.Chmod(socketPath, 0o600); err != nil {
		return err
	}
	server := &http.Server{Handler: broker.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 60 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 20}
	result := make(chan error, 1)
	go func() { result <- server.Serve(listener) }()
	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return server.Shutdown(shutdown)
	case err := <-result:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
