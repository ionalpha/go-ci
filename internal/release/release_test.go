package release

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// testRepo is a real git repository with a real Go module in it. The release path is made
// almost entirely of git and go invocations, so testing it against fakes would test the
// fakes; these tests drive the actual tools.
type testRepo struct {
	t   *testing.T
	dir string
}

func newTestRepo(t *testing.T) *testRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	r := &testRepo{t: t, dir: t.TempDir()}
	r.git("init", "--initial-branch=main")
	r.git("config", "user.email", "test@example.com")
	r.git("config", "user.name", "Test")
	r.git("config", "commit.gpgsign", "false")
	return r
}

func (r *testRepo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = r.dir
	// A fixed committer time keeps the archives, and so the digests, identical between
	// runs, which is what lets the reproducibility assertion below mean anything.
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_DATE=2026-01-01T00:00:00Z",
		"GIT_COMMITTER_DATE=2026-01-01T00:00:00Z",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *testRepo) write(path, body string) {
	r.t.Helper()
	full := filepath.Join(r.dir, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *testRepo) commit(msg string) {
	r.t.Helper()
	r.git("add", "-A")
	r.git("commit", "-m", msg)
}

// scaffold builds a two-component monorepo that mirrors the real shape: both binaries
// import a shared package, and each also has a package only it imports.
func (r *testRepo) scaffold() {
	r.write("go.mod", "module example.com/exts\n\ngo 1.26\n")
	r.write("LICENSE", "Apache-2.0\n")
	r.write("shared/shared.go", "package shared\n\n// Greeting is used by both components.\nfunc Greeting() string { return \"hi\" }\n")
	r.write("token/token.go", "package token\n\n// Name identifies the token component.\nfunc Name() string { return \"token\" }\n")
	r.write("example/example.go", "package example\n\n// Name identifies the example component.\nfunc Name() string { return \"example\" }\n")
	r.write("cmd/token/main.go", `package main

import (
	"fmt"

	"example.com/exts/shared"
	"example.com/exts/token"
)

var version = "dev"

func main() { fmt.Println(version, token.Name(), shared.Greeting()) }
`)
	r.write("cmd/example/main.go", `package main

import (
	"fmt"

	"example.com/exts/example"
	"example.com/exts/shared"
)

var version = "dev"

func main() { fmt.Println(version, example.Name(), shared.Greeting()) }
`)
	r.write(ManifestFile, `version: 1
defaults:
  goos: [linux, windows]
  goarch: [amd64]
  env: ["CGO_ENABLED=0"]
  ldflags: ["-s", "-w"]
  version_var: main.version
  extra_files: [LICENSE]
components:
  - name: token
  - name: example
`)
	r.commit("feat: initial monorepo")
}

func (r *testRepo) manifest() *Manifest {
	r.t.Helper()
	m, err := LoadManifest(filepath.Join(r.dir, ManifestFile), r.dir)
	if err != nil {
		r.t.Fatalf("LoadManifest: %v", err)
	}
	return m
}

func (r *testRepo) manifestPath() string { return filepath.Join(r.dir, ManifestFile) }

// TestScopeFollowsTheImportGraph is the claim the whole design rests on: a component's
// source footprint comes from what it actually imports, so a shared package counts and a
// sibling's package does not. Get this wrong and every changelog and every "needs a
// release" answer in the monorepo is wrong with it.
func TestScopeFollowsTheImportGraph(t *testing.T) {
	t.Parallel()
	r := newTestRepo(t)
	r.scaffold()
	m := r.manifest()

	c, err := m.Component("token")
	if err != nil {
		t.Fatal(err)
	}
	scope, err := Scope(t.Context(), r.dir, c, r.manifestPath())
	if err != nil {
		t.Fatalf("Scope: %v", err)
	}

	for _, in := range []string{"cmd/token/main.go", "token/token.go", "shared/shared.go", "go.mod", "go.sum", ManifestFile, "LICENSE"} {
		if !scope.Covers(in) {
			t.Errorf("token's scope should cover %s", in)
		}
	}
	// The sibling's sources are not in the token's graph, so a change to them must not
	// show up in the token's changelog or force it a new release.
	for _, out := range []string{"cmd/example/main.go", "example/example.go", "README.md", "docs/x.md"} {
		if scope.Covers(out) {
			t.Errorf("token's scope should not cover %s", out)
		}
	}
}

// TestNeedsRelease drives the question a monorepo has to answer before it can tag: which
// components actually changed. A path-glob tool answers this wrong in both directions,
// so both directions are asserted.
func TestNeedsRelease(t *testing.T) {
	t.Parallel()
	r := newTestRepo(t)
	r.scaffold()
	m := r.manifest()
	ctx := t.Context()

	// Never released: everything it contains is unreleased.
	changed, last, err := NeedsRelease(ctx, r.dir, r.manifestPath(), m, "token", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if !changed || last != "" {
		t.Fatalf("an unreleased component should be changed with no baseline; got changed=%v last=%q", changed, last)
	}

	r.git("tag", "token/v0.1.0")
	r.git("tag", "example/v0.1.0")

	// Nothing has moved since the tags.
	for _, name := range []string{"token", "example"} {
		changed, last, err := NeedsRelease(ctx, r.dir, r.manifestPath(), m, name, "HEAD")
		if err != nil {
			t.Fatal(err)
		}
		if changed {
			t.Errorf("%s reports changes at its own release tag", name)
		}
		if last != name+"/v0.1.0" {
			t.Errorf("%s baseline = %q, want its own last tag", name, last)
		}
	}

	// A change to the sibling must not drag the token into a release.
	r.write("example/example.go", "package example\n\n// Name identifies the example component.\nfunc Name() string { return \"example v2\" }\n")
	r.commit("feat(example): rename")

	changed, _, err = NeedsRelease(ctx, r.dir, r.manifestPath(), m, "token", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("a change to the example component marked the token as needing a release")
	}
	changed, _, err = NeedsRelease(ctx, r.dir, r.manifestPath(), m, "example", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("the example component did not notice its own change")
	}

	// A change to the shared package must drag in *both*, because both import it. This
	// is the case a per-directory release tool gets wrong and ships a stale binary.
	r.write("shared/shared.go", "package shared\n\n// Greeting is used by both components.\nfunc Greeting() string { return \"hello\" }\n")
	r.commit("fix(shared): friendlier greeting")

	for _, name := range []string{"token", "example"} {
		changed, _, err := NeedsRelease(ctx, r.dir, r.manifestPath(), m, name, "HEAD")
		if err != nil {
			t.Fatal(err)
		}
		if !changed {
			t.Errorf("%s imports the shared package but did not notice it changed", name)
		}
	}

	// A doc-only commit changes no component.
	r.git("tag", "token/v0.2.0")
	r.write("README.md", "# docs\n")
	r.commit("docs: readme")
	changed, _, err = NeedsRelease(ctx, r.dir, r.manifestPath(), m, "token", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("a docs-only commit marked the token as needing a release")
	}
}

// TestChangelogIsScopedToTheComponent checks that release notes describe the component
// being released and nothing else, and that the range starts at that component's own
// previous release rather than at whatever tag happens to be most recent.
func TestChangelogIsScopedToTheComponent(t *testing.T) {
	t.Parallel()
	r := newTestRepo(t)
	r.scaffold()
	m := r.manifest()
	ctx := t.Context()

	r.git("tag", "token/v0.1.0")

	r.write("token/token.go", "package token\n\n// Name identifies the token component.\nfunc Name() string { return \"tok\" }\n")
	r.commit("feat(token): shorter name")
	r.write("example/example.go", "package example\n\n// Name identifies the example component.\nfunc Name() string { return \"ex\" }\n")
	r.commit("feat(example): shorter name")
	r.write("shared/shared.go", "package shared\n\n// Greeting is used by both components.\nfunc Greeting() string { return \"yo\" }\n")
	r.commit("fix(shared): shorter greeting")
	r.write("docs.md", "docs\n")
	r.commit("docs: add docs")

	// An unrelated component releasing in the middle must not truncate the token's range.
	r.git("tag", "example/v0.2.0")
	r.write("token/extra.go", "package token\n\n// Extra is new.\nfunc Extra() string { return \"x\" }\n")
	r.commit("feat(token)!: add Extra")

	c, err := m.Component("token")
	if err != nil {
		t.Fatal(err)
	}
	scope, err := Scope(ctx, r.dir, c, r.manifestPath())
	if err != nil {
		t.Fatal(err)
	}
	tag, err := ParseTag("token/v0.2.0")
	if err != nil {
		t.Fatal(err)
	}
	cl, err := BuildChangelog(ctx, Git{Dir: r.dir}, tag, "HEAD", scope)
	if err != nil {
		t.Fatalf("BuildChangelog: %v", err)
	}

	if cl.Previous != "token/v0.1.0" {
		t.Errorf("previous = %q, want the token's own last release, not example/v0.2.0", cl.Previous)
	}

	body := cl.Render("ionalpha/flynn-extensions", tag, "")
	mustContain := map[string]string{
		"shorter name":         "the token's own feature",
		"shorter greeting":     "a fix to a shared package the token imports",
		"Breaking changes":     "the ! marker must open a breaking-changes section",
		"add Extra":            "the breaking change itself",
		"token/v0.1.0...token": "a compare link between the component's own releases",
	}
	for needle, why := range mustContain {
		if !strings.Contains(body, needle) {
			t.Errorf("notes are missing %s (%q)\n%s", why, needle, body)
		}
	}
	// The example component's feature landed in the same range but is not the token's
	// change; and a docs commit is not consumer-facing.
	if strings.Contains(body, "feat(example)") || strings.Contains(body, "add docs") {
		t.Errorf("notes leak commits that are not this component's:\n%s", body)
	}
	if cl.Skipped != 2 { // the example commit and the docs commit
		t.Errorf("skipped = %d, want 2 (the sibling's commit and the docs commit)", cl.Skipped)
	}
}

func TestChangelogClassification(t *testing.T) {
	t.Parallel()
	tests := []struct {
		subject string
		group   string
		entry   string
		dropped bool
	}{
		{subject: "feat(token): mint", group: "Features", entry: "token: mint (abcdefg)"},
		{subject: "feat: mint", group: "Features", entry: "mint (abcdefg)"},
		{subject: "fix(safety): guard", group: "Bug fixes", entry: "safety: guard (abcdefg)"},
		{subject: "perf: faster", group: "Performance", entry: "faster (abcdefg)"},
		{subject: "feat(api)!: drop v1", group: "Breaking changes", entry: "api: drop v1 (abcdefg)"},
		{subject: "fix!: drop v1", group: "Breaking changes", entry: "drop v1 (abcdefg)"},
		{subject: "just a subject", group: "Other changes", entry: "just a subject (abcdefg)"},
		{subject: "docs: readme", dropped: true},
		{subject: "chore(deps): bump", dropped: true},
		{subject: "test: add", dropped: true},
		{subject: "ci: pin", dropped: true},
	}
	for _, tc := range tests {
		group, entry, ok := classify(LogEntry{SHA: "abcdefg1234", Subject: tc.subject})
		if tc.dropped {
			if ok {
				t.Errorf("%q should not appear in consumer-facing notes, got group %q", tc.subject, group)
			}
			continue
		}
		if !ok {
			t.Errorf("%q was dropped, want group %q", tc.subject, tc.group)
			continue
		}
		if group != tc.group || entry != tc.entry {
			t.Errorf("%q -> (%q, %q), want (%q, %q)", tc.subject, group, entry, tc.group, tc.entry)
		}
	}
}

// TestBuildProducesVerifiableArtifacts drives a real cross-compile and checks the release
// a consumer would actually download: the archive names their resolver constructs, an
// executable and a license inside, a checksums.txt that commits to every archive, and
// byte-identical output when the same commit is built twice.
func TestBuildProducesVerifiableArtifacts(t *testing.T) {
	t.Parallel()
	r := newTestRepo(t)
	r.scaffold()
	m := r.manifest()
	ctx := t.Context()
	r.git("tag", "token/v0.1.0")

	p, err := NewPlan(ctx, r.dir, r.manifestPath(), m, "token/v0.1.0", "HEAD")
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	want := []string{
		"token_v0.1.0_linux_amd64.tar.gz",
		"token_v0.1.0_windows_amd64.zip",
	}
	if strings.Join(p.Archives, ",") != strings.Join(want, ",") {
		t.Fatalf("planned archives = %v, want %v", p.Archives, want)
	}

	c, err := m.Component("token")
	if err != nil {
		t.Fatal(err)
	}
	tag, err := ParseTag("token/v0.1.0")
	if err != nil {
		t.Fatal(err)
	}

	dist := filepath.Join(t.TempDir(), "dist")
	arts, err := Build(ctx, r.dir, c, tag, p.Commit, p.CommitTime, dist)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(arts) != 2 {
		t.Fatalf("built %d archives, want 2", len(arts))
	}

	// The linux archive must contain an executable binary and the license, at the root.
	names, modes := tarContents(t, filepath.Join(dist, "token_v0.1.0_linux_amd64.tar.gz"))
	if len(names) != 2 || names[0] != "LICENSE" || names[1] != "token" {
		t.Errorf("archive contains %v, want [LICENSE token]", names)
	}
	if modes["token"]&0o111 == 0 {
		t.Errorf("the binary is not executable inside the archive (mode %o)", modes["token"])
	}

	sums, err := WriteChecksums(dist, arts)
	if err != nil {
		t.Fatalf("WriteChecksums: %v", err)
	}
	body, err := os.ReadFile(sums.Path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) != 2 {
		t.Fatalf("checksums.txt has %d lines, want one per archive:\n%s", len(lines), body)
	}
	// checksums.txt is the only thing signed, so if it did not commit to every archive,
	// an unsigned archive could be swapped into the release undetected.
	for _, a := range arts {
		if !strings.Contains(string(body), a.Name) {
			t.Errorf("checksums.txt does not cover %s, so its signature would not either", a.Name)
		}
	}
	for _, line := range lines {
		digest, name, ok := strings.Cut(line, "  ")
		if !ok || len(digest) != 64 {
			t.Errorf("malformed checksum line %q; `sha256sum -c` must be able to read it", line)
		}
		if strings.ContainsAny(name, `/\`) {
			t.Errorf("checksum line %q names a path, but the archives sit beside checksums.txt", line)
		}
	}

	// Building the same commit again must reproduce the same digests, or nobody can
	// independently rebuild the release and confirm the signature covers this source.
	dist2 := filepath.Join(t.TempDir(), "dist2")
	arts2, err := Build(ctx, r.dir, c, tag, p.Commit, p.CommitTime, dist2)
	if err != nil {
		t.Fatal(err)
	}
	sums2, err := WriteChecksums(dist2, arts2)
	if err != nil {
		t.Fatal(err)
	}
	body2, err := os.ReadFile(sums2.Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != string(body2) {
		t.Errorf("rebuilding the same commit produced different digests:\n%s\nvs\n%s", body, body2)
	}
}

// TestBuildStampsTheVersion checks the binary actually reports the version from the tag.
// It runs the host-platform build, so it is the one artifact the test can execute.
func TestBuildStampsTheVersion(t *testing.T) {
	t.Parallel()
	r := newTestRepo(t)
	r.scaffold()
	// Restrict the matrix to this host so the built binary can be run.
	r.write(ManifestFile, `version: 1
defaults:
  goos: ["`+hostOS()+`"]
  goarch: ["`+hostArch()+`"]
  version_var: main.version
  extra_files: [LICENSE]
components:
  - name: token
  - name: example
`)
	r.commit("chore: host-only matrix")
	m := r.manifest()
	ctx := t.Context()
	r.git("tag", "token/v1.4.2")

	c, err := m.Component("token")
	if err != nil {
		t.Fatal(err)
	}
	tag, err := ParseTag("token/v1.4.2")
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewPlan(ctx, r.dir, r.manifestPath(), m, tag.String(), "HEAD")
	if err != nil {
		t.Fatal(err)
	}

	stage := t.TempDir()
	exe := filepath.Join(stage, binaryName(c, Platform{hostOS(), hostArch()}))
	if err := compile(ctx, r.dir, c, tag, p.Commit, p.CommitTime, Platform{hostOS(), hostArch()}, exe); err != nil {
		t.Fatalf("compile: %v", err)
	}
	out, err := exec.Command(exe).Output()
	if err != nil {
		t.Fatalf("running the built binary: %v", err)
	}
	if !strings.HasPrefix(string(out), "v1.4.2 ") {
		t.Errorf("the binary reports %q, want the version from the tag", strings.TrimSpace(string(out)))
	}
}

// TestPreflightRefusesUnsafeReleases covers the checks that stop a release before it can
// publish something wrong: a tag that does not exist, a tag that points somewhere else,
// and a dirty tree whose contents are not the tagged source.
func TestPreflightRefusesUnsafeReleases(t *testing.T) {
	t.Parallel()
	r := newTestRepo(t)
	r.scaffold()
	ctx := t.Context()

	tag, err := ParseTag("token/v0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if err := PreflightRelease(ctx, r.dir, tag, "HEAD"); err == nil ||
		!strings.Contains(err.Error(), "does not exist") {
		t.Errorf("releasing an untagged version should fail; got %v", err)
	}

	r.git("tag", "token/v0.1.0")
	if err := PreflightRelease(ctx, r.dir, tag, "HEAD"); err != nil {
		t.Errorf("a clean tree at the tag should pass preflight: %v", err)
	}

	// The tag now points at an older commit than HEAD: building HEAD would publish
	// source the tag does not name, under a signature that claims it does.
	r.write("token/token.go", "package token\n\n// Name identifies the token component.\nfunc Name() string { return \"drift\" }\n")
	r.commit("feat(token): drift")
	if err := PreflightRelease(ctx, r.dir, tag, "HEAD"); err == nil ||
		!strings.Contains(err.Error(), "points at") {
		t.Errorf("a tag that does not point at the build commit should fail; got %v", err)
	}

	// A dirty tree is not the tagged source either.
	r.git("tag", "token/v0.2.0")
	tag2, err := ParseTag("token/v0.2.0")
	if err != nil {
		t.Fatal(err)
	}
	r.write("token/token.go", "package token\n\n// Name identifies the token component.\nfunc Name() string { return \"uncommitted\" }\n")
	if err := PreflightRelease(ctx, r.dir, tag2, "HEAD"); err == nil ||
		!strings.Contains(err.Error(), "uncommitted changes") {
		t.Errorf("a dirty working tree should fail preflight; got %v", err)
	}
}

// TestPreviousTagIgnoresOtherComponents pins the range a changelog is built over.
func TestPreviousTagIgnoresOtherComponents(t *testing.T) {
	t.Parallel()
	r := newTestRepo(t)
	r.scaffold()
	ctx := t.Context()
	g := Git{Dir: r.dir}

	for _, tag := range []string{"token/v0.1.0", "token/v0.2.0", "example/v9.9.9", "token/v0.10.0", "v1.0.0"} {
		r.git("tag", tag)
	}
	cur, err := ParseTag("token/v0.11.0")
	if err != nil {
		t.Fatal(err)
	}
	prev, ok, err := g.PreviousTag(ctx, cur)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("no previous tag found")
	}
	// v0.10.0 beats v0.2.0 numerically (a lexical sort would pick v0.2.0), and the
	// unrelated example/ and bare v1.0.0 tags are not this component's history.
	if prev.String() != "token/v0.10.0" {
		t.Errorf("previous = %s, want token/v0.10.0", prev)
	}

	// A component with no prior release has no baseline, and the changelog spans
	// everything rather than nothing.
	fresh, err := ParseTag("brand-new/v0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := g.PreviousTag(ctx, fresh); err != nil || ok {
		t.Errorf("an unreleased component must have no previous tag; got ok=%v err=%v", ok, err)
	}
}

func tarContents(t *testing.T, path string) ([]string, map[string]int64) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var names []string
	modes := map[string]int64{}
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		names = append(names, h.Name)
		modes[h.Name] = h.Mode
	}
	return names, modes
}

func hostOS() string   { return goenv("GOOS") }
func hostArch() string { return goenv("GOARCH") }

func goenv(key string) string {
	out, err := exec.Command("go", "env", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
