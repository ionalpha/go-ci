package release

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRepo lays out a minimal module tree so manifest validation has real paths, and real
// sources, to check. Each command gets a main package declaring a `version` var, because
// validation now checks that a stamped variable exists.
func fakeRepo(t *testing.T, dirs ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, d := range dirs {
		full := filepath.Join(root, filepath.FromSlash(d))
		if err := os.MkdirAll(full, 0o755); err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(d, "cmd/") {
			main := "package main\n\nvar version = \"dev\"\n\nfunc main() { _ = version }\n"
			if err := os.WriteFile(filepath.Join(full, "main.go"), []byte(main), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.WriteFile(filepath.Join(root, "LICENSE"), []byte("Apache-2.0"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestParseManifestResolvesDefaults(t *testing.T) {
	t.Parallel()
	root := fakeRepo(t, "cmd/token", "cmd/example")
	m, err := ParseManifest([]byte(`
version: 1
defaults:
  goos: [linux, darwin, windows]
  goarch: [amd64, arm64]
  exclude:
    - {goos: windows, goarch: arm64}
  env: ["CGO_ENABLED=0"]
  ldflags: ["-s", "-w"]
  version_var: main.version
  extra_files: [LICENSE]
components:
  - name: token
  - name: example
    binary: flynn-example
    goarch: [amd64]
`), root)
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}

	token, err := m.Component("token")
	if err != nil {
		t.Fatal(err)
	}
	// A component that names only itself inherits everything, including a main package
	// derived from cmd/<name>: the common case should configure one line.
	if token.Main != "./cmd/token" || token.Binary != "token" || token.VersionVar != "main.version" {
		t.Errorf("token resolved to main=%q binary=%q version_var=%q", token.Main, token.Binary, token.VersionVar)
	}
	if got := len(token.Platforms()); got != 5 {
		t.Errorf("token builds %d platforms, want 5 (3 os x 2 arch, minus the excluded windows/arm64)", got)
	}
	for _, p := range token.Platforms() {
		if p.GOOS == "windows" && p.GOARCH == "arm64" {
			t.Error("the excluded windows/arm64 pair was built anyway")
		}
	}

	example, err := m.Component("example")
	if err != nil {
		t.Fatal(err)
	}
	// An override replaces the default rather than merging with it.
	if example.Binary != "flynn-example" {
		t.Errorf("example binary = %q, want the override", example.Binary)
	}
	if got := len(example.Platforms()); got != 3 {
		t.Errorf("example builds %d platforms, want 3 (amd64 only, on 3 OSes)", got)
	}
	// Inherited defaults still apply to the fields it did not override.
	if example.VersionVar != "main.version" || len(example.ExtraFiles) != 1 {
		t.Errorf("example did not inherit the defaults it left unset: %+v", example)
	}
}

func TestManifestValidation(t *testing.T) {
	t.Parallel()
	base := `
version: 1
defaults:
  goos: [linux]
  goarch: [amd64]
components:
`
	tests := []struct {
		name    string
		yaml    string
		dirs    []string
		wantErr string
	}{
		{
			name:    "unknown field is a typo, not something to ignore",
			yaml:    base + "  - name: token\n    binry: token\n",
			dirs:    []string{"cmd/token"},
			wantErr: "field binry not found",
		},
		{
			name:    "main package must exist",
			yaml:    base + "  - name: token\n",
			dirs:    []string{"cmd/other"},
			wantErr: "main package ./cmd/token does not exist",
		},
		{
			name:    "a command with no component would ship unreleased",
			yaml:    base + "  - name: token\n",
			dirs:    []string{"cmd/token", "cmd/orphan"},
			wantErr: "cmd/orphan is not declared",
		},
		{
			name: "an unreleased command can be ignored, explicitly",
			yaml: base + "  - name: token\n" + "ignored_cmds: [orphan]\n",
			dirs: []string{"cmd/token", "cmd/orphan"},
		},
		{
			name:    "two components cannot build the same binary name",
			yaml:    base + "  - name: token\n  - name: example\n    binary: token\n",
			dirs:    []string{"cmd/token", "cmd/example"},
			wantErr: `both build a binary named "token"`,
		},
		{
			name:    "a duplicate component is ambiguous",
			yaml:    base + "  - name: token\n  - name: token\n",
			dirs:    []string{"cmd/token"},
			wantErr: `component "token" is declared twice`,
		},
		{
			name:    "a name that cannot appear in a tag is rejected at config time",
			yaml:    base + "  - name: Token\n",
			dirs:    []string{"cmd/Token"},
			wantErr: "must start with a lowercase letter or digit",
		},
		{
			name:    "an unsupported platform fails before the build does",
			yaml:    "version: 1\ncomponents:\n  - name: token\n    goos: [plan9]\n    goarch: [amd64]\n",
			dirs:    []string{"cmd/token"},
			wantErr: "plan9/amd64 is not a supported Go platform",
		},
		{
			name:    "a missing license is an error, never a silent omission",
			yaml:    base + "  - name: token\n    extra_files: [NOTICE]\n",
			dirs:    []string{"cmd/token"},
			wantErr: `extra_files entry "NOTICE" does not exist`,
		},
		{
			name:    "extra_files cannot escape the repo",
			yaml:    base + "  - name: token\n    extra_files: [\"../../etc/passwd\"]\n",
			dirs:    []string{"cmd/token"},
			wantErr: "must be a path inside the repo",
		},
		{
			name:    "an unqualified stamp variable would silently stamp nothing",
			yaml:    base + "  - name: token\n    version_var: version\n",
			dirs:    []string{"cmd/token"},
			wantErr: "must be fully qualified",
		},
		{
			// The linker ignores -X for a symbol it cannot find, so a typo here would
			// otherwise ship a binary that reports "dev" as its version, forever, with
			// no error anywhere.
			name:    "a stamp variable that does not exist would silently stamp nothing",
			yaml:    base + "  - name: token\n    version_var: main.buildVersion\n",
			dirs:    []string{"cmd/token"},
			wantErr: `declares no package-level var "buildVersion"`,
		},
		{
			name:    "excluding the whole matrix builds nothing",
			yaml:    base + "  - name: token\n    exclude: [{goos: linux, goarch: amd64}]\n",
			dirs:    []string{"cmd/token"},
			wantErr: "every os/arch pair is excluded",
		},
		{
			name:    "an unsupported schema version is not guessed at",
			yaml:    "version: 2\ncomponents:\n  - name: token\n",
			dirs:    []string{"cmd/token"},
			wantErr: "manifest version 2 is unsupported",
		},
		{
			name:    "a manifest with no components releases nothing",
			yaml:    "version: 1\ncomponents: []\n",
			dirs:    []string{"cmd/token"},
			wantErr: "declares no components",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := fakeRepo(t, tc.dirs...)
			_, err := ParseManifest([]byte(tc.yaml), root)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("manifest was accepted, want error %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestArchiveNaming(t *testing.T) {
	t.Parallel()
	// The archive name is the contract between this tool and every consumer's resolver,
	// which builds the same string from (component, version, os, arch) to find its
	// download. Pin it.
	c := Component{Name: "token", Binary: "token"}
	v, err := ParseVersion("v0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		p    Platform
		want string
	}{
		{Platform{"linux", "amd64"}, "token_v0.1.0_linux_amd64.tar.gz"},
		{Platform{"darwin", "arm64"}, "token_v0.1.0_darwin_arm64.tar.gz"},
		{Platform{"windows", "amd64"}, "token_v0.1.0_windows_amd64.zip"},
	} {
		if got := ArchiveName(c, v, tc.p); got != tc.want {
			t.Errorf("ArchiveName(%s) = %q, want %q", tc.p, got, tc.want)
		}
	}
	// Windows binaries need the extension, or the archive unpacks to something the OS
	// will not execute.
	if got := binaryName(c, Platform{"windows", "amd64"}); got != "token.exe" {
		t.Errorf("windows binary name = %q, want token.exe", got)
	}
	if got := binaryName(c, Platform{"linux", "amd64"}); got != "token" {
		t.Errorf("linux binary name = %q, want token", got)
	}
}
