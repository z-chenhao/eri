package googleauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"os/user"
	"runtime"
	"strings"

	"github.com/z-chenhao/eri/internal/keychain"
)

type KeychainStore struct {
	account string
	service string
}

func NewKeychainStore(clientID string) (*KeychainStore, error) {
	if runtime.GOOS != "darwin" {
		return nil, fmt.Errorf("Google Auth Broker currently requires macOS Keychain")
	}
	current, err := user.Current()
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(clientID))
	return &KeychainStore{account: current.Username, service: "dev.eri.google-oauth." + hex.EncodeToString(digest[:8])}, nil
}

func (s *KeychainStore) Load(ctx context.Context) (Grant, bool, error) {
	command := exec.CommandContext(ctx, "/usr/bin/security", "find-generic-password", "-a", s.account, "-s", s.service, "-w")
	body, err := command.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 44 {
			return Grant{}, false, nil
		}
		return Grant{}, false, fmt.Errorf("read macOS Keychain: %w", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(body)))
	if err != nil {
		return Grant{}, false, fmt.Errorf("decode Keychain grant: %w", err)
	}
	var grant Grant
	if err := json.Unmarshal(decoded, &grant); err != nil || grant.RefreshToken == "" {
		return Grant{}, false, fmt.Errorf("Keychain grant is invalid")
	}
	return grant, true, nil
}

func (s *KeychainStore) Save(ctx context.Context, grant Grant) error {
	if strings.TrimSpace(grant.RefreshToken) == "" || len(grant.Scopes) == 0 {
		return fmt.Errorf("cannot store an incomplete Google grant")
	}
	body, err := json.Marshal(grant)
	if err != nil {
		return err
	}
	encoded := base64.StdEncoding.EncodeToString(body)
	if err := keychain.AddGenericPassword(ctx, s.account, s.service, encoded); err != nil {
		return fmt.Errorf("store Google grant in macOS Keychain: %w", err)
	}
	return nil
}

func (s *KeychainStore) Delete(ctx context.Context) error {
	command := exec.CommandContext(ctx, "/usr/bin/security", "delete-generic-password", "-a", s.account, "-s", s.service)
	if err := command.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 44 {
			return nil
		}
		return fmt.Errorf("delete Google grant from macOS Keychain: %w", err)
	}
	return nil
}
