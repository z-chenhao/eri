package repository_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var markdownLink = regexp.MustCompile(`\[[^\]]*\]\(([^)]+)\)`)

func TestLocalMarkdownLinksResolveInsideRepository(t *testing.T) {
	root := repositoryRoot(t)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != root && (entry.Name() == ".git" || entry.Name() == ".eri" || entry.Name() == "bin" || entry.Name() == "dist") {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".md" {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, match := range markdownLink.FindAllStringSubmatch(string(body), -1) {
			target := strings.TrimSpace(match[1])
			if strings.HasPrefix(target, "<") && strings.HasSuffix(target, ">") {
				target = strings.TrimSuffix(strings.TrimPrefix(target, "<"), ">")
			}
			if target == "" || strings.HasPrefix(target, "#") || strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
				continue
			}
			if fragment := strings.IndexByte(target, '#'); fragment >= 0 {
				target = target[:fragment]
			}
			if query := strings.IndexByte(target, '?'); query >= 0 {
				target = target[:query]
			}
			if target == "" {
				continue
			}
			resolved := filepath.Clean(filepath.Join(filepath.Dir(path), filepath.FromSlash(target)))
			relative, err := filepath.Rel(root, resolved)
			if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				t.Errorf("%s links outside repository: %s", displayPath(root, path), match[1])
				continue
			}
			if _, err := os.Stat(resolved); err != nil {
				t.Errorf("%s contains broken local link %s: %v", displayPath(root, path), match[1], err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestProjectLocalDataRootIsIgnoredByGitAndSourceScans(t *testing.T) {
	root := repositoryRoot(t)
	checks := map[string][]string{
		".gitignore":            {"/.eri/"},
		"Makefile":              {"-not -path './.eri/*'"},
		"scripts/check-repo.sh": {"--exclude-dir=.eri"},
	}
	for name, required := range checks {
		body, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		for _, marker := range required {
			if !strings.Contains(string(body), marker) {
				t.Errorf("%s does not exclude project-local Eri data with %q", name, marker)
			}
		}
	}
}

func TestRepositoryDocumentationContainsNoMachineSpecificAbsolutePaths(t *testing.T) {
	root := repositoryRoot(t)
	for _, name := range []string{"README.md", "CONTRIBUTING.md", "SECURITY.md", "AGENTS.md"} {
		body, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"/Users/", "file://", "~/.codex/attachments/"} {
			if strings.Contains(string(body), forbidden) {
				t.Errorf("%s contains machine-specific path marker %q", name, forbidden)
			}
		}
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func displayPath(root, path string) string {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(relative)
}
