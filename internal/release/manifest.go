package release

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ManifestFile is the conventional manifest name at a module's root.
const ManifestFile = ".release.yaml"

// Manifest declares the independently releasable components of one Go module. It is the
// single source of truth for what a repo ships: relkit builds, names, signs and publishes
// only what is declared here, and `relkit validate` fails if a command exists in the tree
// without a matching entry, so a new binary cannot ship unreleased or unsigned by accident.
type Manifest struct {
	// Version is the manifest schema version. Only 1 exists.
	Version int `yaml:"version"`

	// Defaults supply every field a component omits.
	Defaults Component `yaml:"defaults"`

	// Components are the releasable units, each tagged as "<name>/vX.Y.Z".
	Components []Component `yaml:"components"`

	// CmdDir is the directory scanned by the "every command is releasable" check.
	// Defaults to "cmd"; set it to "-" to disable the check.
	CmdDir string `yaml:"cmd_dir"`

	// IgnoredCmds are directories under CmdDir that are deliberately not released
	// (a test helper, say). Listing one is an explicit, reviewable decision.
	IgnoredCmds []string `yaml:"ignored_cmds"`
}

// Component is one releasable binary. Zero-valued fields inherit from Manifest.Defaults,
// so a repo whose components differ only by name and main package writes almost nothing.
type Component struct {
	// Name is the tag prefix and the identity a consumer installs by ("token").
	Name string `yaml:"name"`

	// Main is the package to build, relative to the module root ("./cmd/token").
	Main string `yaml:"main"`

	// Binary is the executable name inside the archive. Defaults to Name.
	Binary string `yaml:"binary"`

	// GOOS and GOARCH are the target matrix; every combination is built unless it is
	// listed in Exclude.
	GOOS   []string `yaml:"goos"`
	GOARCH []string `yaml:"goarch"`

	// Exclude drops individual os/arch pairs from the matrix.
	Exclude []Platform `yaml:"exclude"`

	// Env is extra environment for the build ("CGO_ENABLED=0").
	Env []string `yaml:"env"`

	// Ldflags are passed to -ldflags, before the injected version stamps.
	Ldflags []string `yaml:"ldflags"`

	// VersionVar, CommitVar and DateVar are fully qualified variables stamped with the
	// release version, the commit SHA, and the commit's RFC3339 timestamp. Each is
	// stamped only if named; an empty name injects nothing.
	VersionVar string `yaml:"version_var"`
	CommitVar  string `yaml:"commit_var"`
	DateVar    string `yaml:"date_var"`

	// ExtraFiles are repo-root-relative files copied into every archive (LICENSE and
	// NOTICE, typically). A listed file that does not exist is an error, never a
	// silent omission: a release that quietly drops its license is a legal problem.
	ExtraFiles []string `yaml:"extra_files"`
}

// Platform is one os/arch build target.
type Platform struct {
	GOOS   string `yaml:"goos"`
	GOARCH string `yaml:"goarch"`
}

func (p Platform) String() string { return p.GOOS + "/" + p.GOARCH }

// LoadManifest reads and validates the manifest at path, resolving defaults into every
// component. Every path in the returned manifest has been checked against dir.
func LoadManifest(path, dir string) (*Manifest, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: reading the manifest the operator pointed us at
	if err != nil {
		return nil, err
	}
	return ParseManifest(raw, dir)
}

// ParseManifest parses manifest bytes and validates them against the module rooted at dir.
func ParseManifest(raw []byte, dir string) (*Manifest, error) {
	var m Manifest
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true) // a typo'd key is a mistake, not a field to ignore
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if err := m.resolve(); err != nil {
		return nil, err
	}
	if err := m.validate(dir); err != nil {
		return nil, err
	}
	return &m, nil
}

// resolve folds Defaults into each component and applies built-in fallbacks.
func (m *Manifest) resolve() error {
	if m.Version != 1 {
		return fmt.Errorf("manifest version %d is unsupported (want 1)", m.Version)
	}
	if m.CmdDir == "" {
		m.CmdDir = "cmd"
	}
	for i := range m.Components {
		c := &m.Components[i]
		d := m.Defaults
		if c.Main == "" {
			c.Main = d.Main
		}
		if c.Binary == "" {
			c.Binary = d.Binary
		}
		if len(c.GOOS) == 0 {
			c.GOOS = d.GOOS
		}
		if len(c.GOARCH) == 0 {
			c.GOARCH = d.GOARCH
		}
		if len(c.Exclude) == 0 {
			c.Exclude = d.Exclude
		}
		if len(c.Env) == 0 {
			c.Env = d.Env
		}
		if len(c.Ldflags) == 0 {
			c.Ldflags = d.Ldflags
		}
		if len(c.ExtraFiles) == 0 {
			c.ExtraFiles = d.ExtraFiles
		}
		if c.VersionVar == "" {
			c.VersionVar = d.VersionVar
		}
		if c.CommitVar == "" {
			c.CommitVar = d.CommitVar
		}
		if c.DateVar == "" {
			c.DateVar = d.DateVar
		}
		// A component that names nothing else still needs a main package and a binary
		// name; derive both from its name so the common case configures one line.
		if c.Main == "" && c.Name != "" && m.CmdDir != "" {
			c.Main = "./" + path.Join(m.CmdDir, c.Name)
		}
		if c.Binary == "" {
			c.Binary = c.Name
		}
	}
	return nil
}

// validate checks every invariant that would otherwise surface as a broken release: an
// unbuildable main package, a name that cannot appear in a tag, a colliding artifact
// name, an unknown platform, or a command in the tree that no component releases.
func (m *Manifest) validate(dir string) error {
	if len(m.Components) == 0 {
		return errors.New("manifest declares no components")
	}
	if m.Defaults.Name != "" {
		return errors.New("defaults must not set a name")
	}

	names := map[string]bool{}
	binaries := map[string]string{}
	for _, c := range m.Components {
		if err := ValidateComponentName(c.Name); err != nil {
			return err
		}
		if names[c.Name] {
			return fmt.Errorf("component %q is declared twice", c.Name)
		}
		names[c.Name] = true

		// Two components sharing a binary name would produce two different archives
		// with the same filename, so a consumer could not tell them apart.
		if other, dup := binaries[c.Binary]; dup {
			return fmt.Errorf("components %q and %q both build a binary named %q", other, c.Name, c.Binary)
		}
		binaries[c.Binary] = c.Name

		if !strings.HasPrefix(c.Main, "./") {
			return fmt.Errorf("component %q: main %q must be a module-relative package path like ./cmd/%s", c.Name, c.Main, c.Name)
		}
		mainDir := filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(c.Main, "./")))
		if fi, err := os.Stat(mainDir); err != nil || !fi.IsDir() {
			return fmt.Errorf("component %q: main package %s does not exist", c.Name, c.Main)
		}
		if len(c.GOOS) == 0 || len(c.GOARCH) == 0 {
			return fmt.Errorf("component %q: goos and goarch must each list at least one target", c.Name)
		}
		for _, p := range c.Platforms() {
			if !knownPlatform(p) {
				return fmt.Errorf("component %q: %s is not a supported Go platform", c.Name, p)
			}
		}
		if len(c.Platforms()) == 0 {
			return fmt.Errorf("component %q: every os/arch pair is excluded", c.Name)
		}
		for _, f := range c.ExtraFiles {
			if filepath.IsAbs(f) || strings.Contains(f, "..") {
				return fmt.Errorf("component %q: extra_files entry %q must be a path inside the repo", c.Name, f)
			}
			if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(f))); err != nil {
				return fmt.Errorf("component %q: extra_files entry %q does not exist", c.Name, f)
			}
		}
		for _, e := range c.Env {
			if !strings.Contains(e, "=") {
				return fmt.Errorf("component %q: env entry %q is not KEY=VALUE", c.Name, e)
			}
		}
		for _, v := range []string{c.VersionVar, c.CommitVar, c.DateVar} {
			if v != "" && !strings.Contains(v, ".") {
				return fmt.Errorf("component %q: stamp variable %q must be fully qualified, e.g. main.version", c.Name, v)
			}
		}
		if err := validateStampVars(dir, c); err != nil {
			return err
		}
	}
	return m.validateCmdCoverage(dir, names)
}

// validateCmdCoverage fails if a command exists under CmdDir that no component releases.
// Without it, adding cmd/foo ships a binary nobody signs, versions, or can install; the
// only way past the gate is to declare the component or to list it in ignored_cmds, and
// both are visible in review.
func (m *Manifest) validateCmdCoverage(dir string, names map[string]bool) error {
	if m.CmdDir == "" || m.CmdDir == "-" {
		return nil
	}
	entries, err := os.ReadDir(filepath.Join(dir, filepath.FromSlash(m.CmdDir)))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	ignored := map[string]bool{}
	for _, ig := range m.IgnoredCmds {
		ignored[ig] = true
	}

	// A component may build a main package outside CmdDir, so match on the resolved
	// main path rather than assuming main == "./" + CmdDir + "/" + name.
	claimed := map[string]bool{}
	for _, c := range m.Components {
		claimed[path.Clean(strings.TrimPrefix(c.Main, "./"))] = true
	}
	var orphans []string
	for _, e := range entries {
		if !e.IsDir() || ignored[e.Name()] {
			continue
		}
		if !claimed[path.Join(m.CmdDir, e.Name())] {
			orphans = append(orphans, path.Join(m.CmdDir, e.Name()))
		}
	}
	if len(orphans) > 0 {
		sort.Strings(orphans)
		return fmt.Errorf("%s is not declared in %s: add a component for it, or list it in ignored_cmds",
			strings.Join(orphans, ", "), ManifestFile)
	}
	for ig := range ignored {
		if names[ig] {
			return fmt.Errorf("%q is both a component and in ignored_cmds", ig)
		}
	}
	return nil
}

// Component returns the component with the given name.
func (m *Manifest) Component(name string) (Component, error) {
	for _, c := range m.Components {
		if c.Name == name {
			return c, nil
		}
	}
	var have []string
	for _, c := range m.Components {
		have = append(have, c.Name)
	}
	return Component{}, fmt.Errorf("no component named %q in %s (have: %s)", name, ManifestFile, strings.Join(have, ", "))
}

// Names returns every component name, sorted.
func (m *Manifest) Names() []string {
	out := make([]string, 0, len(m.Components))
	for _, c := range m.Components {
		out = append(out, c.Name)
	}
	sort.Strings(out)
	return out
}

// Platforms expands the component's matrix into the os/arch pairs it actually builds,
// in a stable order, with Exclude applied.
func (c Component) Platforms() []Platform {
	excluded := map[Platform]bool{}
	for _, e := range c.Exclude {
		excluded[e] = true
	}
	var out []Platform
	for _, goos := range c.GOOS {
		for _, arch := range c.GOARCH {
			p := Platform{GOOS: goos, GOARCH: arch}
			if !excluded[p] {
				out = append(out, p)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].GOOS != out[j].GOOS {
			return out[i].GOOS < out[j].GOOS
		}
		return out[i].GOARCH < out[j].GOARCH
	})
	return out
}

// knownPlatform reports whether Go can build for the pair. The list is the subset of
// `go tool dist list` that a distributed CLI plausibly targets; an unlisted pair is
// rejected loudly rather than failing deep inside a build.
func knownPlatform(p Platform) bool {
	supported := map[string][]string{
		"linux":   {"amd64", "arm64", "arm", "386", "riscv64", "ppc64le", "s390x"},
		"darwin":  {"amd64", "arm64"},
		"windows": {"amd64", "arm64", "386"},
		"freebsd": {"amd64", "arm64"},
		"openbsd": {"amd64", "arm64"},
		"netbsd":  {"amd64", "arm64"},
	}
	for _, a := range supported[p.GOOS] {
		if a == p.GOARCH {
			return true
		}
	}
	return false
}
