package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/z-chenhao/eri/brokers/googleauth"
	ericonfig "github.com/z-chenhao/eri/internal/config"
	"github.com/z-chenhao/eri/internal/observability"
)

var googleScopes = []string{
	"https://www.googleapis.com/auth/calendar.events.readonly",
	"https://www.googleapis.com/auth/calendar.events",
	"https://www.googleapis.com/auth/gmail.metadata",
	"https://www.googleapis.com/auth/gmail.send",
}

func main() {
	truncateBrokerBootstrapLog()
	mode := "run"
	arguments := os.Args[1:]
	if len(arguments) > 0 && (arguments[0] == "install" || arguments[0] == "uninstall") {
		mode, arguments = arguments[0], arguments[1:]
	}
	defaultIssuer, defaultRedemption, defaultsErr := defaultSocketPaths()
	if defaultsErr != nil {
		fmt.Fprintln(os.Stderr, "eri-google-auth-broker:", defaultsErr)
		os.Exit(1)
	}
	flags := flag.NewFlagSet("eri-google-auth-broker", flag.ExitOnError)
	issuerSocket := flags.String("issuer-socket", defaultIssuer, "absolute Unix socket used only by Eri Core to issue one-use capabilities")
	redemptionSocket := flags.String("redemption-socket", defaultRedemption, "distinct absolute Unix socket exposed to the Google plugin")
	callback := flags.String("callback", "127.0.0.1:7792", "numeric loopback OAuth callback address")
	clientConfig := flags.String("client-config", "", "path to a Google Desktop OAuth client JSON file outside EriDataRoot")
	flags.Parse(arguments)
	if mode == "uninstall" {
		if err := uninstallBrokerService(context.Background()); err != nil {
			fmt.Fprintln(os.Stderr, "eri-google-auth-broker uninstall:", err)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stdout, "Eri Google Auth Broker launchd service was removed. The Google grant remains in Keychain until authorization_disconnect is used.")
		return
	}
	if *clientConfig == "" || !safeSocketPath(*issuerSocket) || !safeSocketPath(*redemptionSocket) || filepath.Clean(*issuerSocket) == filepath.Clean(*redemptionSocket) {
		fmt.Fprintln(os.Stderr, "eri-google-auth-broker: --client-config and distinct absolute --issuer-socket/--redemption-socket values are required")
		os.Exit(2)
	}
	if err := requireClientConfigOutsideEriDataRoot(*clientConfig); err != nil {
		fmt.Fprintln(os.Stderr, "eri-google-auth-broker:", err)
		os.Exit(2)
	}
	host, port, err := net.SplitHostPort(*callback)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() || host == "::1" {
		fmt.Fprintln(os.Stderr, "eri-google-auth-broker: --callback must use a numeric IPv4 loopback address")
		os.Exit(2)
	}
	redirectURI := "http://" + net.JoinHostPort(host, port) + "/oauth/google/callback"
	oauth, err := googleauth.LoadDesktopClient(*clientConfig, redirectURI)
	if err != nil {
		fmt.Fprintln(os.Stderr, "eri-google-auth-broker:", err)
		os.Exit(1)
	}
	if mode == "install" {
		dataRoot, err := eriDataRoot()
		if err != nil {
			fmt.Fprintln(os.Stderr, "eri-google-auth-broker install:", err)
			os.Exit(1)
		}
		if err := installBrokerService(context.Background(), brokerServiceConfig{
			ClientConfig: *clientConfig, IssuerSocket: *issuerSocket, RedemptionSocket: *redemptionSocket, Callback: *callback, DataRoot: dataRoot,
		}); err != nil {
			fmt.Fprintln(os.Stderr, "eri-google-auth-broker install:", err)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stdout, "Eri Google Auth Broker is installed and running as a separate per-user launchd service.")
		return
	}
	store, err := googleauth.NewKeychainStore(oauth.ClientID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "eri-google-auth-broker:", err)
		os.Exit(1)
	}
	dataRoot, err := eriDataRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, "eri-google-auth-broker:", err)
		os.Exit(1)
	}
	logger, logFile, err := observability.NewProcessLogger(filepath.Join(dataRoot, "logs", "google-auth-broker.log"), nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "eri-google-auth-broker:", err)
		os.Exit(1)
	}
	defer logFile.Close()
	broker, err := googleauth.New(googleauth.Options{OAuth: oauth, Store: store, AllowedScopes: googleScopes, Logger: logger})
	if err != nil {
		fmt.Fprintln(os.Stderr, "eri-google-auth-broker:", err)
		os.Exit(1)
	}
	issuerListener, err := openPrivateUnixSocket(*issuerSocket)
	if err != nil {
		fmt.Fprintln(os.Stderr, "eri-google-auth-broker:", err)
		os.Exit(1)
	}
	defer issuerListener.Close()
	defer os.Remove(*issuerSocket)
	redemptionListener, err := openPrivateUnixSocket(*redemptionSocket)
	if err != nil {
		fmt.Fprintln(os.Stderr, "eri-google-auth-broker:", err)
		os.Exit(1)
	}
	defer redemptionListener.Close()
	defer os.Remove(*redemptionSocket)
	callbackListener, err := net.Listen("tcp", *callback)
	if err != nil {
		fmt.Fprintln(os.Stderr, "eri-google-auth-broker:", err)
		os.Exit(1)
	}
	issuerServer := brokerHTTPServer(broker.IssuerHandler())
	redemptionServer := brokerHTTPServer(broker.PluginHandler())
	callbackServer := brokerHTTPServer(broker.CallbackHandler())
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 3)
	go func() { errCh <- issuerServer.Serve(issuerListener) }()
	go func() { errCh <- redemptionServer.Serve(redemptionListener) }()
	go func() { errCh <- callbackServer.Serve(callbackListener) }()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, server := range []*http.Server{issuerServer, redemptionServer, callbackServer} {
			if err := server.Shutdown(shutdown); err != nil {
				logger.Error("Google Auth Broker HTTP shutdown failed", "component", "google_auth_broker", "error_code", observability.ErrorCode(err), "error", observability.SafeError(err))
			}
		}
	}()
	logger.Info("Google Auth Broker started", "component", "google_auth_broker", "issuer_socket", observability.SafeText(*issuerSocket, 240), "redemption_socket", observability.SafeText(*redemptionSocket, 240), "callback", *callback)
	err = <-errCh
	if err != nil && err != http.ErrServerClosed {
		logger.Error("Google Auth Broker stopped unexpectedly", "component", "google_auth_broker", "error_code", observability.ErrorCode(err), "error", observability.SafeError(err))
		os.Exit(1)
	}
	logger.Info("Google Auth Broker stopped", "component", "google_auth_broker")
}

func brokerHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 20}
}

func truncateBrokerBootstrapLog() {
	root := strings.TrimSpace(os.Getenv("ERI_DATA_ROOT"))
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) == string(filepath.Separator) {
		return
	}
	directory := filepath.Join(root, "logs")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return
	}
	_ = os.Chmod(directory, 0o700)
	file, err := os.OpenFile(filepath.Join(directory, "google-auth-broker-bootstrap.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return
	}
	_ = file.Chmod(0o600)
	_ = file.Close()
}

func safeSocketPath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) != string(filepath.Separator)
}

func openPrivateUnixSocket(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := removeStaleSocket(path); err != nil {
		return nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		listener.Close()
		os.Remove(path)
		return nil, err
	}
	return listener, nil
}

func requireClientConfigOutsideEriDataRoot(path string) error {
	configPath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve Google OAuth client file: %w", err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(configPath); resolveErr == nil {
		configPath = resolved
	}
	dataRoot, err := eriDataRoot()
	if err != nil {
		return err
	}
	if resolved, resolveErr := filepath.EvalSymlinks(dataRoot); resolveErr == nil {
		dataRoot = resolved
	}
	relative, err := filepath.Rel(dataRoot, configPath)
	if err != nil {
		return fmt.Errorf("compare Google OAuth client file with Eri data root: %w", err)
	}
	if relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
		return fmt.Errorf("Google OAuth client file must remain outside EriDataRoot")
	}
	return nil
}

func defaultSocketPaths() (string, string, error) {
	root, err := eriDataRoot()
	if err != nil {
		return "", "", err
	}
	runtimeRoot := filepath.Join(root, "runtime")
	return filepath.Join(runtimeRoot, "google-auth-issuer.sock"), filepath.Join(runtimeRoot, "google-auth-redemption.sock"), nil
}

func eriDataRoot() (string, error) {
	return ericonfig.ResolveDataRoot()
}

func removeStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("refusing to replace non-socket path %s", path)
	}
	connection, dialErr := net.DialTimeout("unix", path, 150*time.Millisecond)
	if dialErr == nil {
		connection.Close()
		return fmt.Errorf("Google Auth Broker is already listening on %s", path)
	}
	return os.Remove(path)
}
