// Package content provides encrypted, integrity-checked storage for private
// bodies referenced by operational records.
package content

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/z-chenhao/eri/internal/identifier"
)

const fileMagic = "ERIC1"

var objectIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

var (
	// ErrDeleted indicates that a logical object has been removed.
	ErrDeleted = errors.New("content object deleted")
	// ErrIntegrity indicates ciphertext or metadata no longer matches its ref.
	ErrIntegrity = errors.New("content integrity check failed")
)

// Ref is safe to store in events and relational records. It never includes a
// physical path, plaintext, or encryption key.
type Ref struct {
	ObjectID         string `json:"object_id"`
	Version          int    `json:"version"`
	ContentHash      string `json:"content_hash"`
	MediaType        string `json:"media_type"`
	SizeBytes        int64  `json:"size_bytes"`
	EncryptionDomain string `json:"encryption_domain"`
	PrivacyClass     string `json:"privacy_class"`
	RetentionPolicy  string `json:"retention_policy"`
	ProvenanceRef    string `json:"provenance_ref,omitempty"`
}

// Metadata controls the logical policy attached to a new object.
type Metadata struct {
	MediaType        string
	EncryptionDomain string
	PrivacyClass     string
	RetentionPolicy  string
	ProvenanceRef    string
}

// Store encrypts each content object independently with AES-256-GCM.
type Store struct {
	root string
	aead cipher.AEAD
}

// New constructs a content store with a 256-bit key.
func New(root string, key []byte) (*Store, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("content key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create content cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create content AEAD: %w", err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create content root: %w", err)
	}
	return &Store{root: root, aead: aead}, nil
}

// Put encrypts and atomically stores a new immutable object.
func (s *Store) Put(ctx context.Context, body []byte, metadata Metadata) (Ref, error) {
	if err := ctx.Err(); err != nil {
		return Ref{}, err
	}
	id, err := identifier.New()
	if err != nil {
		return Ref{}, err
	}
	hash := sha256.Sum256(body)
	ref := Ref{
		ObjectID:         id,
		Version:          1,
		ContentHash:      hex.EncodeToString(hash[:]),
		MediaType:        defaultString(metadata.MediaType, "application/octet-stream"),
		SizeBytes:        int64(len(body)),
		EncryptionDomain: defaultString(metadata.EncryptionDomain, "default"),
		PrivacyClass:     defaultString(metadata.PrivacyClass, "private"),
		RetentionPolicy:  defaultString(metadata.RetentionPolicy, "user_owned"),
		ProvenanceRef:    metadata.ProvenanceRef,
	}

	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return Ref{}, fmt.Errorf("generate content nonce: %w", err)
	}
	aad := []byte(ref.ObjectID + ":" + ref.ContentHash)
	ciphertext := s.aead.Seal(nil, nonce, body, aad)
	payload := make([]byte, 0, len(fileMagic)+len(nonce)+len(ciphertext))
	payload = append(payload, fileMagic...)
	payload = append(payload, nonce...)
	payload = append(payload, ciphertext...)

	path := s.objectPath(id)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Ref{}, fmt.Errorf("create content shard: %w", err)
	}
	temporary, err := os.CreateTemp(dir, ".pending-*")
	if err != nil {
		return Ref{}, fmt.Errorf("create temporary content: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return Ref{}, fmt.Errorf("protect temporary content: %w", err)
	}
	if _, err := temporary.Write(payload); err != nil {
		temporary.Close()
		return Ref{}, fmt.Errorf("write encrypted content: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return Ref{}, fmt.Errorf("sync encrypted content: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return Ref{}, fmt.Errorf("close encrypted content: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return Ref{}, fmt.Errorf("commit encrypted content: %w", err)
	}
	return ref, nil
}

// Get decrypts an object and verifies its declared hash and size.
func (s *Store) Get(ctx context.Context, ref Ref) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !objectIDPattern.MatchString(ref.ObjectID) {
		return nil, ErrIntegrity
	}
	payload, err := os.ReadFile(s.objectPath(ref.ObjectID))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrDeleted
	}
	if err != nil {
		return nil, fmt.Errorf("read encrypted content: %w", err)
	}
	headerSize := len(fileMagic) + s.aead.NonceSize()
	if len(payload) < headerSize || string(payload[:len(fileMagic)]) != fileMagic {
		return nil, ErrIntegrity
	}
	nonce := payload[len(fileMagic):headerSize]
	aad := []byte(ref.ObjectID + ":" + ref.ContentHash)
	body, err := s.aead.Open(nil, nonce, payload[headerSize:], aad)
	if err != nil {
		return nil, ErrIntegrity
	}
	hash := sha256.Sum256(body)
	if hex.EncodeToString(hash[:]) != ref.ContentHash || int64(len(body)) != ref.SizeBytes {
		return nil, ErrIntegrity
	}
	return body, nil
}

// Delete removes the ciphertext. Operational callers remain responsible for
// recording a deletion receipt and propagating derived invalidations.
func (s *Store) Delete(ctx context.Context, ref Ref) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !objectIDPattern.MatchString(ref.ObjectID) {
		return ErrIntegrity
	}
	err := os.Remove(s.objectPath(ref.ObjectID))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete encrypted content: %w", err)
	}
	return nil
}

func (s *Store) objectPath(id string) string {
	shard := "00"
	if len(id) >= 2 {
		shard = id[:2]
	}
	return filepath.Join(s.root, shard, id+".eri")
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
