package release

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// validateStampVars checks that every variable a component asks to stamp actually exists
// in its main package.
//
// The linker's -X flag is silent about a symbol it cannot find: a typo in version_var, or
// a rename of the variable it points at, does not fail the build. It just produces a
// binary that reports "dev" forever, and nobody notices until a user asks an extension
// what version it is and it lies. So the existence of the symbol is checked here, where a
// mistake is a failed CI gate rather than a shipped release.
//
// Only variables in package main are checked, because that is where the source lives that
// this manifest can see. A stamp aimed at another package is left alone.
func validateStampVars(dir string, c Component) error {
	wanted := map[string]string{} // variable name -> the manifest field that asked for it
	for _, s := range []struct{ field, qualified string }{
		{"version_var", c.VersionVar},
		{"commit_var", c.CommitVar},
		{"date_var", c.DateVar},
	} {
		pkg, name, ok := strings.Cut(s.qualified, ".")
		if !ok || pkg != "main" {
			continue
		}
		wanted[name] = s.field
	}
	if len(wanted) == 0 {
		return nil
	}

	mainDir := filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(c.Main, "./")))
	entries, err := os.ReadDir(mainDir)
	if err != nil {
		return fmt.Errorf("component %q: read %s: %w", c.Name, c.Main, err)
	}

	// Every .go file in the directory is parsed, including build-tagged ones: the
	// variable may well be declared in a file that only one platform compiles, and this
	// check should not depend on which platform is asking.
	fset := token.NewFileSet()
	declared := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(mainDir, e.Name()), nil, 0)
		if err != nil {
			return fmt.Errorf("component %q: parse %s: %w", c.Name, path.Join(c.Main, e.Name()), err)
		}
		if f.Name.Name != "main" {
			continue
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, id := range vs.Names {
					declared[id.Name] = true
				}
			}
		}
	}

	for name, field := range wanted {
		if !declared[name] {
			return fmt.Errorf("component %q: %s names main.%s, but %s declares no package-level var %q; "+
				"the linker ignores -X for a symbol it cannot find, so the binary would silently report no version",
				c.Name, field, name, c.Main, name)
		}
	}
	return nil
}
