package content

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestStoreRoundTripIsEncryptedAndIntegrityChecked(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	key := bytes.Repeat([]byte{0x42}, 32)
	store, err := New(root, key)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("a private conversation fragment")
	ref, err := store.Put(context.Background(), body, Metadata{MediaType: "text/plain; charset=utf-8"})
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(root, ref.ObjectID[:2], ref.ObjectID+".eri")
	ciphertext, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ciphertext, body) {
		t.Fatal("ciphertext contains plaintext")
	}
	got, err := store.Get(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("got %q, want %q", got, body)
	}

	ref.ContentHash = "tampered"
	if _, err := store.Get(context.Background(), ref); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("tampered ref error = %v, want ErrIntegrity", err)
	}
}

func TestStoreRejectsContentRefPathTraversal(t *testing.T) {
	t.Parallel()
	store, err := New(t.TempDir(), bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	malicious := Ref{ObjectID: "../../outside", ContentHash: "irrelevant"}
	if _, err := store.Get(context.Background(), malicious); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("Get traversal error = %v", err)
	}
	if err := store.Delete(context.Background(), malicious); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("Delete traversal error = %v", err)
	}
}

func TestStoreDeleteIsIdempotent(t *testing.T) {
	t.Parallel()
	store, err := New(t.TempDir(), bytes.Repeat([]byte{0x18}, 32))
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.Put(context.Background(), []byte("delete me"), Metadata{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(context.Background(), ref); !errors.Is(err, ErrDeleted) {
		t.Fatalf("get deleted error = %v, want ErrDeleted", err)
	}
}
