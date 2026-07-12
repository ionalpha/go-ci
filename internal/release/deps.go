package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// SourceScope is the set of repo-relative paths whose contents can change what a
// component's binary does.
//
// It is computed from the component's real import graph (`go list -deps`), not from a
// hand-written path glob. That distinction is the whole point of scoping a monorepo per
// component: when the token extension imports a shared package, a fix to that package
// belongs in the token's changelog and means the token needs a new release, and no
// human has to remember to update a list of directories to make that true. Equally, a
// change to a *sibling* component's package is correctly excluded, because it is not in
// this component's graph.
type SourceScope struct {
	// Dirs are repo-relative directories holding packages the binary is built from,
	// including the main package itself.
	Dirs []string

	// Files are individual repo-relative files that also affect the build: the module
	// files (a dependency bump changes the binary) and the release manifest (it changes
	// how the binary is built and named).
	Files []string
}

// Covers reports whether a repo-relative path is inside the scope.
func (s SourceScope) Covers(p string) bool {
	p = path.Clean(filepath.ToSlash(p))
	for _, f := range s.Files {
		if p == f {
			return true
		}
	}
	for _, d := range s.Dirs {
		// A directory covers files directly inside it. Go packages are per-directory,
		// so a nested directory is a *different* package: it is in scope only if the
		// graph put it there, in which case it is listed in Dirs on its own.
		if path.Dir(p) == d {
			return true
		}
	}
	return false
}

// CoversAny reports whether any of the paths is in scope.
func (s SourceScope) CoversAny(paths []string) bool {
	for _, p := range paths {
		if s.Covers(p) {
			return true
		}
	}
	return false
}

// goListPkg is the subset of `go list -json` output the scope needs.
type goListPkg struct {
	Dir        string // absolute directory of the package
	ImportPath string //
	Module     *struct{ Path, Dir string }
	Standard   bool     // a stdlib package
	GoFiles    []string //
}

// Scope computes the source scope of a component by walking its build graph. Only
// packages inside this module count: a change to a third-party dependency arrives
// through go.mod/go.sum, which is in Files.
//
// The graph is resolved for every target platform in the component's matrix, because
// build constraints mean the set of packages a binary depends on is platform-specific
// (a Windows-only import is invisible to a linux/amd64 `go list`). Missing one would let
// a change to a platform-gated package slip out of the component's changelog.
func Scope(ctx context.Context, dir string, c Component, manifestPath string) (SourceScope, error) {
	modRoot, _, err := moduleRoot(ctx, dir)
	if err != nil {
		return SourceScope{}, err
	}

	dirs := map[string]bool{}
	for _, p := range c.Platforms() {
		pkgs, err := listDeps(ctx, dir, c, p)
		if err != nil {
			return SourceScope{}, err
		}
		for _, pkg := range pkgs {
			if pkg.Standard || pkg.Module == nil || pkg.Dir == "" {
				continue
			}
			// Restrict to packages inside this module's tree. A dependency in the
			// module cache lives outside modRoot and is tracked via go.sum instead.
			rel, err := filepath.Rel(modRoot, pkg.Dir)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				continue
			}
			dirs[filepath.ToSlash(rel)] = true
		}
	}
	if len(dirs) == 0 {
		return SourceScope{}, fmt.Errorf("component %q: import graph resolved to no in-module packages", c.Name)
	}

	scope := SourceScope{Files: []string{"go.mod", "go.sum"}}
	for d := range dirs {
		scope.Dirs = append(scope.Dirs, d)
	}
	if manifestPath != "" {
		if rel, err := filepath.Rel(modRoot, manifestPath); err == nil {
			scope.Files = append(scope.Files, filepath.ToSlash(rel))
		}
	}
	// Every listed extra file (LICENSE, NOTICE) ships inside the archive, so a change
	// to one changes the artifact.
	scope.Files = append(scope.Files, c.ExtraFiles...)

	sort.Strings(scope.Dirs)
	sort.Strings(scope.Files)
	scope.Files = dedupe(scope.Files)
	return scope, nil
}

// listDeps runs `go list -deps -json` for one target platform.
func listDeps(ctx context.Context, dir string, c Component, p Platform) ([]goListPkg, error) {
	cmd := exec.CommandContext(ctx, "go", "list", "-deps", "-json", c.Main) //nolint:gosec // G204: c.Main is a validated package path from the manifest
	cmd.Dir = dir
	cmd.Env = buildEnv(c, p)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("go list %s for %s: %s", c.Main, p, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("go list %s for %s: %w", c.Main, p, err)
	}
	// `go list -json` emits a stream of concatenated JSON objects, not an array.
	dec := json.NewDecoder(strings.NewReader(string(out)))
	var pkgs []goListPkg
	for dec.More() {
		var pkg goListPkg
		if err := dec.Decode(&pkg); err != nil {
			return nil, fmt.Errorf("decode go list output: %w", err)
		}
		pkgs = append(pkgs, pkg)
	}
	return pkgs, nil
}

// moduleRoot returns the module's root directory and path.
func moduleRoot(ctx context.Context, dir string) (root, modPath string, err error) {
	cmd := exec.CommandContext(ctx, "go", "list", "-m", "-f", "{{.Dir}}\t{{.Path}}")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return "", "", fmt.Errorf("resolve module root: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return "", "", fmt.Errorf("resolve module root: %w", err)
	}
	root, modPath, ok := strings.Cut(strings.TrimSpace(string(out)), "\t")
	if !ok {
		return "", "", fmt.Errorf("resolve module root: unexpected output %q", out)
	}
	return root, modPath, nil
}

func dedupe(in []string) []string {
	out := in[:0]
	var prev string
	for i, s := range in {
		if i == 0 || s != prev {
			out = append(out, s)
		}
		prev = s
	}
	return out
}
