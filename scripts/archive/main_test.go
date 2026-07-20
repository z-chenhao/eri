package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestArchiveIsDeterministicAndContainsNoBuilderIdentity(t *testing.T) {
	source := t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "plugins"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("Eri\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "eri"), []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(source, "README.md"), time.Now().Add(-time.Hour), time.Now()); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(t.TempDir(), "first.tar.gz")
	second := filepath.Join(t.TempDir(), "second.tar.gz")
	if err := writeArchive(source, first, "eri_0.1.0_darwin_arm64"); err != nil {
		t.Fatal(err)
	}
	if err := writeArchive(source, second, "eri_0.1.0_darwin_arm64"); err != nil {
		t.Fatal(err)
	}
	firstBody, _ := os.ReadFile(first)
	secondBody, _ := os.ReadFile(second)
	if !bytes.Equal(firstBody, secondBody) {
		t.Fatal("identical release inputs produced different archives")
	}
	reader, err := gzip.NewReader(bytes.NewReader(firstBody))
	if err != nil {
		t.Fatal(err)
	}
	tarReader := tar.NewReader(reader)
	var names []string
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, header.Name)
		if header.Uid != 0 || header.Gid != 0 || header.Uname != "root" || header.Gname != "root" || !header.ModTime.Equal(archiveTime) {
			t.Fatalf("non-normalized header: %+v", header)
		}
		if header.Name == "eri_0.1.0_darwin_arm64/eri" && header.Mode != 0o755 {
			t.Fatalf("binary mode = %o", header.Mode)
		}
	}
	want := []string{"eri_0.1.0_darwin_arm64/", "eri_0.1.0_darwin_arm64/README.md", "eri_0.1.0_darwin_arm64/eri", "eri_0.1.0_darwin_arm64/plugins/"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("archive entries = %v", names)
	}
}

func TestArchiveRejectsSymlink(t *testing.T) {
	source := t.TempDir()
	if err := os.Symlink("outside", filepath.Join(source, "link")); err != nil {
		t.Fatal(err)
	}
	err := writeArchive(source, filepath.Join(t.TempDir(), "release.tar.gz"), "eri")
	if err == nil {
		t.Fatal("release archive accepted a symlink")
	}
}
