package main

import "testing"

func TestNoticeNamesAreConservative(t *testing.T) {
	for _, name := range []string{"LICENSE", "LICENSE.txt", "COPYING.md", "NOTICE", "COPYRIGHT-3"} {
		if !isNoticeName(name) {
			t.Fatalf("notice %q rejected", name)
		}
	}
	for _, name := range []string{"README.md", "license-helper.go", "AUTHORS"} {
		if isNoticeName(name) {
			t.Fatalf("non-notice %q accepted", name)
		}
	}
}
