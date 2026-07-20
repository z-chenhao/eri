// Command notices generates the complete third-party license bundle for the
// packages linked into Eri's distributed binaries.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

type module struct {
	Path    string
	Version string
	Dir     string
	Main    bool
	Replace *module
}

type listedPackage struct {
	Module *module
}

func main() {
	output := flag.String("output", "", "output file")
	flag.Parse()
	if *output == "" {
		fatal(fmt.Errorf("-output is required"))
	}
	modules, err := linkedModules()
	if err != nil {
		fatal(err)
	}
	if err := writeBundle(*output, modules); err != nil {
		fatal(err)
	}
}

func linkedModules() ([]module, error) {
	command := exec.Command("go", "list", "-deps", "-json", "./cmd/eri", "./cmd/eri-google-workspace", "./cmd/eri-google-auth-broker")
	command.Stderr = os.Stderr
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := command.Start(); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(stdout)
	unique := map[string]module{}
	for {
		var pkg listedPackage
		if err := decoder.Decode(&pkg); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("decode go list output: %w", err)
		}
		if pkg.Module == nil || pkg.Module.Main {
			continue
		}
		selected := pkg.Module
		if selected.Replace != nil {
			selected = selected.Replace
		}
		if selected.Dir == "" || selected.Path == "" {
			return nil, fmt.Errorf("dependency %s has no local source directory", selected.Path)
		}
		unique[selected.Path+"@"+selected.Version] = *selected
	}
	if err := command.Wait(); err != nil {
		return nil, fmt.Errorf("go list linked dependencies: %w", err)
	}
	result := make([]module, 0, len(unique))
	for _, dependency := range unique {
		result = append(result, dependency)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Path == result[j].Path {
			return result[i].Version < result[j].Version
		}
		return result[i].Path < result[j].Path
	})
	return result, nil
}

func writeBundle(path string, modules []module) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	fmt.Fprintln(file, "Eri third-party licenses")
	fmt.Fprintln(file, "Generated from the dependency graph linked into the distributed binaries.")
	for _, dependency := range modules {
		entries, err := os.ReadDir(dependency.Dir)
		if err != nil {
			_ = file.Close()
			return fmt.Errorf("read dependency %s: %w", dependency.Path, err)
		}
		files := make([]string, 0)
		for _, entry := range entries {
			if !entry.IsDir() && isNoticeName(entry.Name()) {
				files = append(files, entry.Name())
			}
		}
		sort.Strings(files)
		if len(files) == 0 {
			_ = file.Close()
			return fmt.Errorf("dependency %s@%s has no top-level license or notice file", dependency.Path, dependency.Version)
		}
		fmt.Fprintf(file, "\n================================================================================\n%s@%s\n================================================================================\n", dependency.Path, dependency.Version)
		for _, name := range files {
			body, err := os.ReadFile(filepath.Join(dependency.Dir, name))
			if err != nil {
				_ = file.Close()
				return err
			}
			fmt.Fprintf(file, "\n--- %s ---\n%s", name, body)
			if len(body) == 0 || body[len(body)-1] != '\n' {
				fmt.Fprintln(file)
			}
		}
	}
	return file.Close()
}

func isNoticeName(name string) bool {
	upper := strings.ToUpper(name)
	for _, prefix := range []string{"LICENSE", "LICENCE", "COPYING", "NOTICE", "COPYRIGHT"} {
		if upper == prefix || strings.HasPrefix(upper, prefix+".") {
			return true
		}
		if strings.HasPrefix(upper, prefix+"-") && !strings.Contains(strings.TrimPrefix(upper, prefix+"-"), ".") {
			return true
		}
	}
	return false
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "generate third-party notices:", err)
	os.Exit(1)
}
