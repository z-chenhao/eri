// Command archive creates deterministic, identity-free release tarballs.
package main

import (
	"archive/tar"
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var archiveTime = time.Unix(0, 0).UTC()

func main() {
	source := flag.String("source", "", "source directory")
	output := flag.String("output", "", "output .tar.gz path")
	prefix := flag.String("prefix", "", "top-level archive directory")
	flag.Parse()
	if err := writeArchive(*source, *output, *prefix); err != nil {
		fmt.Fprintln(os.Stderr, "build deterministic archive:", err)
		os.Exit(1)
	}
}

func writeArchive(source, output, prefix string) (resultErr error) {
	if strings.TrimSpace(source) == "" || strings.TrimSpace(output) == "" || !safePrefix(prefix) {
		return fmt.Errorf("-source, -output and a safe single-component -prefix are required")
	}
	source, err := filepath.Abs(source)
	if err != nil {
		return err
	}
	info, err := os.Stat(source)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("source must be a directory")
	}
	entries := make([]string, 0)
	err = filepath.WalkDir(source, func(filePath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if filePath == source {
			return nil
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("release input contains symlink: %s", filePath)
		}
		entryInfo, err := entry.Info()
		if err != nil {
			return err
		}
		if !entryInfo.IsDir() && !entryInfo.Mode().IsRegular() {
			return fmt.Errorf("release input is not a regular file or directory: %s", filePath)
		}
		entries = append(entries, filePath)
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(entries)
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	complete := false
	defer func() {
		if closeErr := file.Close(); resultErr == nil && closeErr != nil {
			resultErr = closeErr
		}
		if !complete {
			_ = os.Remove(output)
		}
	}()
	gzipWriter, err := gzip.NewWriterLevel(file, gzip.BestCompression)
	if err != nil {
		return err
	}
	gzipWriter.Header.ModTime = archiveTime
	gzipWriter.Header.OS = 255
	tarWriter := tar.NewWriter(gzipWriter)
	if err := tarWriter.WriteHeader(directoryHeader(prefix + "/")); err != nil {
		return err
	}
	for _, filePath := range entries {
		relative, err := filepath.Rel(source, filePath)
		if err != nil {
			return err
		}
		entryInfo, err := os.Stat(filePath)
		if err != nil {
			return err
		}
		name := path.Join(prefix, filepath.ToSlash(relative))
		if entryInfo.IsDir() {
			if err := tarWriter.WriteHeader(directoryHeader(name + "/")); err != nil {
				return err
			}
			continue
		}
		mode := int64(0o644)
		if entryInfo.Mode().Perm()&0o111 != 0 {
			mode = 0o755
		}
		header := normalizedHeader(name, mode, entryInfo.Size(), tar.TypeReg)
		if err := tarWriter.WriteHeader(header); err != nil {
			return err
		}
		input, err := os.Open(filePath)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tarWriter, input)
		closeErr := input.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	if err := tarWriter.Close(); err != nil {
		return err
	}
	if err := gzipWriter.Close(); err != nil {
		return err
	}
	complete = true
	return nil
}

func safePrefix(value string) bool {
	return value != "" && value != "." && value != ".." && path.Base(value) == value && !strings.ContainsAny(value, "\\\x00")
}

func directoryHeader(name string) *tar.Header {
	return normalizedHeader(name, 0o755, 0, tar.TypeDir)
}

func normalizedHeader(name string, mode, size int64, kind byte) *tar.Header {
	return &tar.Header{
		Name: name, Mode: mode, Size: size, Typeflag: kind,
		Uid: 0, Gid: 0, Uname: "root", Gname: "root",
		ModTime: archiveTime, AccessTime: archiveTime, ChangeTime: archiveTime,
		Format: tar.FormatPAX,
	}
}
