package identifier

import (
	"encoding/hex"
	"testing"
)

func TestNewReturnsOpaqueUnique128BitIdentifiers(t *testing.T) {
	seen := make(map[string]struct{}, 128)
	for index := 0; index < 128; index++ {
		id, err := New()
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := hex.DecodeString(id)
		if err != nil || len(decoded) != 16 {
			t.Fatalf("identifier %q is not 128-bit lowercase hex: bytes=%d err=%v", id, len(decoded), err)
		}
		if _, duplicate := seen[id]; duplicate {
			t.Fatalf("duplicate identifier %q", id)
		}
		seen[id] = struct{}{}
	}
}

func TestMustNewUsesTheSameOpaqueFormat(t *testing.T) {
	id := MustNew()
	decoded, err := hex.DecodeString(id)
	if err != nil || len(decoded) != 16 {
		t.Fatalf("identifier %q is not 128-bit lowercase hex: bytes=%d err=%v", id, len(decoded), err)
	}
}
