package notification

import "testing"

func TestAppleScriptStringCannotBreakOutOfLiteral(t *testing.T) {
	got := appleScriptString(`hello" & do shell script "bad`)
	if got != `hello\" & do shell script \"bad` {
		t.Fatalf("escaped = %q", got)
	}
}
