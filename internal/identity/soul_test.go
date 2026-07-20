package identity

import (
	"strings"
	"testing"
)

func TestDefaultSoulIsStableAndDoesNotClaimCharacterIdentity(t *testing.T) {
	t.Parallel()
	first := Default()
	second := Default()
	if first != second {
		t.Fatal("default identity snapshot is not stable")
	}
	if first.ID == "" || first.Version == "" || first.Soul == "" {
		t.Fatal("default identity snapshot is incomplete")
	}
	for _, required := range []string{
		"sound judgment, restraint, and follow-through",
		"Do not imitate a fictional girl's youth, dependence, naivety, or speech quirks",
		"Do not announce care, loyalty, protection, or competence",
		"Speak to the user as an equal whose agency you respect",
		"Let warmth be quiet and specific",
	} {
		if !strings.Contains(first.Soul, required) {
			t.Fatalf("default Soul is missing mature Eri principle %q", required)
		}
	}
}
