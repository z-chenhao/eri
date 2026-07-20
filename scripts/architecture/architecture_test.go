package architecture_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/z-chenhao/eri"

func TestDomainBoundariesRemainAcyclicByConstruction(t *testing.T) {
	root := repositoryRoot(t)
	imports := productionImports(t, root)

	assertNoImports(t, imports, "internal/agent",
		"internal/daemon", "internal/localapi", "internal/store/sqlite", "internal/model/")
	assertNoImports(t, imports, "internal/runtime",
		"internal/agent", "internal/daemon", "internal/localapi", "internal/store/sqlite", "internal/model/")
	assertNoImports(t, imports, "internal/tool",
		"internal/daemon", "internal/localapi", "internal/store/sqlite", "internal/tool/builtin")

	for source, dependencies := range imports {
		if !strings.HasPrefix(source, "internal/model/") {
			continue
		}
		for _, dependency := range dependencies {
			if strings.HasPrefix(dependency, modulePath+"/internal/") && dependency != modulePath+"/internal/agent" {
				t.Errorf("%s imports concrete internal dependency %s; provider adapters may depend only on the agent model contract", source, dependency)
			}
		}
	}

	for source, dependencies := range imports {
		if source == "internal/cli" {
			continue
		}
		for _, dependency := range dependencies {
			if dependency == modulePath+"/internal/daemon" {
				t.Errorf("%s imports composition root internal/daemon; only internal/cli may start the daemon", source)
			}
		}
	}
}

func TestPrimaryCommandRemainsThin(t *testing.T) {
	imports := productionImports(t, repositoryRoot(t))
	dependencies := imports["cmd/eri"]
	if len(dependencies) == 0 {
		t.Fatal("cmd/eri has no parsed imports")
	}
	for _, dependency := range dependencies {
		if strings.HasPrefix(dependency, modulePath+"/") && dependency != modulePath+"/internal/cli" {
			t.Errorf("cmd/eri imports %s; the primary command must delegate only to internal/cli", dependency)
		}
	}
}

func TestEveryProductPackageShipsInADistributedCommand(t *testing.T) {
	imports := productionImports(t, repositoryRoot(t))
	reachable := make(map[string]bool)
	var visit func(string)
	visit = func(packagePath string) {
		if reachable[packagePath] {
			return
		}
		reachable[packagePath] = true
		for _, dependency := range imports[packagePath] {
			if strings.HasPrefix(dependency, modulePath+"/") {
				visit(strings.TrimPrefix(dependency, modulePath+"/"))
			}
		}
	}
	for _, command := range []string{"cmd/eri", "cmd/eri-google-workspace", "cmd/eri-google-auth-broker"} {
		visit(command)
	}
	for packagePath := range imports {
		if strings.HasPrefix(packagePath, "scripts/") {
			continue
		}
		if !reachable[packagePath] {
			t.Errorf("production package %s is not linked into any distributed command; remove speculative code or wire a real vertical slice", packagePath)
		}
	}
}

func TestRepositoryDoesNotGrowGenericBoundarylessPackages(t *testing.T) {
	root := repositoryRoot(t)
	for _, name := range []string{"core", "common", "utils", "types", "ports", "adapters"} {
		path := filepath.Join(root, "internal", name)
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			t.Errorf("generic package %s is forbidden; place code in the domain that owns the behavior", filepath.ToSlash(path))
		}
	}
}

func TestProductionDoesNotEmbedAssistantReplies(t *testing.T) {
	root := repositoryRoot(t)
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != root && (entry.Name() == ".git" || entry.Name() == ".eri" || entry.Name() == "vendor" || entry.Name() == "bin" || entry.Name() == "dist") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		parsed, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) < 5 {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "commitEvaluatedReply" {
				return true
			}
			if staticStringExpression(call.Args[4]) {
				position := fset.Position(call.Args[4].Pos())
				t.Errorf("%s:%d embeds a fixed assistant reply; evaluated conversation text must come from the model candidate", displayPath(root, position.Filename), position.Line)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func staticStringExpression(expression ast.Expr) bool {
	switch value := expression.(type) {
	case *ast.BasicLit:
		return value.Kind == token.STRING
	case *ast.ParenExpr:
		return staticStringExpression(value.X)
	case *ast.BinaryExpr:
		return value.Op == token.ADD && staticStringExpression(value.X) && staticStringExpression(value.Y)
	default:
		return false
	}
}

func assertNoImports(t *testing.T, imports map[string][]string, source string, forbidden ...string) {
	t.Helper()
	for _, dependency := range imports[source] {
		for _, boundary := range forbidden {
			if dependency == modulePath+"/"+boundary || strings.HasPrefix(dependency, modulePath+"/"+boundary) {
				t.Errorf("%s imports forbidden dependency %s", source, dependency)
			}
		}
	}
}

func productionImports(t *testing.T, root string) map[string][]string {
	t.Helper()
	result := make(map[string][]string)
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != root && (entry.Name() == ".git" || entry.Name() == ".eri" || entry.Name() == "vendor" || entry.Name() == "bin" || entry.Name() == "dist") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		parsed, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		packagePath := filepath.ToSlash(relative)
		for _, declaration := range parsed.Decls {
			general, ok := declaration.(*ast.GenDecl)
			if !ok || general.Tok != token.IMPORT {
				continue
			}
			for _, spec := range general.Specs {
				importSpec := spec.(*ast.ImportSpec)
				value, err := strconv.Unquote(importSpec.Path.Value)
				if err != nil {
					return err
				}
				result[packagePath] = append(result[packagePath], value)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
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
