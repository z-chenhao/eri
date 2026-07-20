package content

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"strings"

	"github.com/z-chenhao/eri/internal/keychain"
)

const masterKeyEnvironment = "ERI_MASTER_KEY"

// LoadOrCreateMasterKey obtains a per-instance key. macOS stores it in the
// user's Keychain; other platforms require an ephemeral environment value
// until an equivalent OS credential adapter is implemented.
func LoadOrCreateMasterKey(ctx context.Context, dataRoot string) ([]byte, error) {
	if encoded := strings.TrimSpace(os.Getenv(masterKeyEnvironment)); encoded != "" {
		key, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", masterKeyEnvironment, err)
		}
		if len(key) != 32 {
			return nil, fmt.Errorf("%s must encode exactly 32 bytes", masterKeyEnvironment)
		}
		return key, nil
	}
	if runtime.GOOS != "darwin" {
		return nil, fmt.Errorf("no OS key provider for %s; set %s to a base64-encoded 32-byte key", runtime.GOOS, masterKeyEnvironment)
	}
	return loadOrCreateMacOSKey(ctx, dataRoot)
}

func loadOrCreateMacOSKey(ctx context.Context, dataRoot string) ([]byte, error) {
	current, err := user.Current()
	if err != nil {
		return nil, fmt.Errorf("resolve keychain account: %w", err)
	}
	hash := sha256.Sum256([]byte(dataRoot))
	service := "dev.eri.master-key." + hex.EncodeToString(hash[:6])

	find := exec.CommandContext(ctx, "/usr/bin/security", "find-generic-password", "-a", current.Username, "-s", service, "-w")
	output, err := find.Output()
	if err == nil {
		return decodeStoredKey(output)
	}
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) {
		return nil, fmt.Errorf("read macOS Keychain: %w", err)
	}

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate master key: %w", err)
	}
	encoded := base64.StdEncoding.EncodeToString(key)
	if err := keychain.AddGenericPassword(ctx, current.Username, service, encoded); err != nil {
		return nil, fmt.Errorf("store master key in macOS Keychain: %w", err)
	}
	return key, nil
}

func decodeStoredKey(output []byte) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(output)))
	if err != nil {
		return nil, fmt.Errorf("decode Keychain master key: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("Keychain master key has invalid length %d", len(key))
	}
	return key, nil
}
