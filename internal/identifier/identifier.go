// Package identifier creates opaque identifiers that carry no user data.
package identifier

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// New returns a random 128-bit identifier encoded as lowercase hexadecimal.
func New() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate identifier: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

// MustNew returns a new identifier and panics only when the operating system
// random source is unavailable. It is intended for process-local bootstrap
// values; persisted command paths should propagate New errors.
func MustNew() string {
	id, err := New()
	if err != nil {
		panic(err)
	}
	return id
}
