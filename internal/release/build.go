package release

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Artifact is one distributable file produced for a release: an archive, and later the
// checksum file and its signature.
type Artifact struct {
	// Name is the file's basename, which is also its GitHub release asset name and the
	// last path element a consumer downloads.
	Name string `json:"name"`

	// Path is where the file was written on disk.
	Path string `json:"path"`

	// Platform is the target the artifact was built for; zero for platform-independent
	// artifacts like checksums.txt.
	Platform Platform `json:"platform,omitzero"`

	// Kind distinguishes an archive from the metadata that describes it.
	Kind ArtifactKind `json:"kind"`
}

// ArtifactKind labels an artifact's role in the release.
type ArtifactKind string

// The kinds of file a release publishes. Only KindChecksums is signed; it commits to
// every other artifact by digest, so one signature covers them all.
const (
	KindArchive     ArtifactKind = "archive"
	KindChecksums   ArtifactKind = "checksums"
	KindSignature   ArtifactKind = "signature"
	KindCertificate ArtifactKind = "certificate"
	KindSBOM        ArtifactKind = "sbom"
)

// ArchiveName is the filename of a component's archive for one platform. It is the one
// string a consumer's resolver has to be able to construct from (component, version,
// os, arch), so it is defined once, here, and every other place derives it from this.
//
//	token_v0.1.0_linux_amd64.tar.gz
//	token_v0.1.0_windows_amd64.zip
func ArchiveName(c Component, v Version, p Platform) string {
	return fmt.Sprintf("%s_%s_%s_%s%s", c.Binary, v, p.GOOS, p.GOARCH, archiveExt(p))
}

func archiveExt(p Platform) string {
	if p.GOOS == "windows" {
		return ".zip"
	}
	return ".tar.gz"
}

// binaryName is the executable's name inside the archive.
func binaryName(c Component, p Platform) string {
	if p.GOOS == "windows" {
		return c.Binary + ".exe"
	}
	return c.Binary
}

// Build compiles a component for every platform in its matrix and packs each result into
// an archive under dist. Builds are reproducible: -trimpath erases local paths, CGO is
// off by default so there is no host toolchain in the output, the version stamps come
// from the tag, and the archive's timestamps come from the commit rather than the clock.
// Building the same commit twice, on two machines, yields byte-identical archives.
func Build(ctx context.Context, dir string, c Component, t Tag, commit string, commitTime time.Time, dist string) ([]Artifact, error) {
	// 0755: dist holds artifacts that are about to be published; they are not secrets.
	if err := os.MkdirAll(dist, 0o755); err != nil { //nolint:gosec // G301: public artifact directory
		return nil, err
	}
	// Binaries are staged outside dist so that only distributable files land there and
	// the checksum step cannot accidentally hash a build intermediate.
	stage, err := os.MkdirTemp("", "relkit-build-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(stage) }()

	extras, err := resolveExtraFiles(dir, c)
	if err != nil {
		return nil, err
	}

	var artifacts []Artifact
	for _, p := range c.Platforms() {
		exe := filepath.Join(stage, p.GOOS+"_"+p.GOARCH, binaryName(c, p))
		if err := compile(ctx, dir, c, t, commit, commitTime, p, exe); err != nil {
			return nil, err
		}
		name := ArchiveName(c, t.Version, p)
		out := filepath.Join(dist, name)
		files := append([]archiveEntry{{Name: binaryName(c, p), Path: exe, Mode: 0o755}}, extras...)
		if err := writeArchive(out, files, commitTime, p.GOOS == "windows"); err != nil {
			return nil, fmt.Errorf("archive %s: %w", name, err)
		}
		artifacts = append(artifacts, Artifact{Name: name, Path: out, Platform: p, Kind: KindArchive})
	}
	return artifacts, nil
}

// compile runs `go build` for one platform.
func compile(ctx context.Context, dir string, c Component, t Tag, commit string, commitTime time.Time, p Platform, out string) error {
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil { //nolint:gosec // G301: build staging directory
		return err
	}
	ldflags := append([]string{}, c.Ldflags...)
	for _, stamp := range [][2]string{
		{c.VersionVar, t.Version.String()},
		{c.CommitVar, commit},
		{c.DateVar, commitTime.Format(time.RFC3339)},
	} {
		if stamp[0] != "" {
			ldflags = append(ldflags, "-X", stamp[0]+"="+stamp[1])
		}
	}

	args := []string{"build", "-trimpath", "-o", out}
	if len(ldflags) > 0 {
		args = append(args, "-ldflags", strings.Join(ldflags, " "))
	}
	args = append(args, c.Main)

	// Invoking the Go toolchain is what this tool does; args are composed from the
	// manifest, which validation has already constrained.
	cmd := exec.CommandContext(ctx, "go", args...) //nolint:gosec // G204: building is the point
	cmd.Dir = dir
	cmd.Env = buildEnv(c, p)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("build %s for %s: %w\n%s", c.Main, p, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// buildEnv is the environment for a component's build and for the `go list` that derives
// its scope. Both must agree, or the scope would describe a different binary than the one
// shipped. CGO is off unless the component overrides it, so the artifact is static and
// does not pick up the runner's libc.
func buildEnv(c Component, p Platform) []string {
	env := append(os.Environ(),
		"GOOS="+p.GOOS,
		"GOARCH="+p.GOARCH,
		"CGO_ENABLED=0",
		// Clear any GOFLAGS the operator exported: a stray -tags or -mod would quietly
		// change what gets built, and a release must depend only on what the repo
		// declares. The toolchain version is one of those declared things, so it is left
		// to the go and toolchain lines in go.mod rather than pinned to the host's root
		// toolchain here, which would break every machine whose Go predates them.
		"GOFLAGS=",
	)
	// Component env comes last so it wins over the defaults above.
	env = append(env, c.Env...)
	return env
}

// resolveExtraFiles checks and loads the repo files that ship inside every archive.
func resolveExtraFiles(dir string, c Component) ([]archiveEntry, error) {
	var out []archiveEntry
	for _, f := range c.ExtraFiles {
		p := filepath.Join(dir, filepath.FromSlash(f))
		fi, err := os.Stat(p)
		if err != nil {
			return nil, fmt.Errorf("extra file %q: %w", f, err)
		}
		if fi.IsDir() {
			return nil, fmt.Errorf("extra file %q is a directory", f)
		}
		// Flattened into the archive root: an archive is a handful of files, and a
		// consumer should not have to reproduce the repo's layout to find the license.
		out = append(out, archiveEntry{Name: filepath.Base(f), Path: p, Mode: 0o644})
	}
	return out, nil
}
