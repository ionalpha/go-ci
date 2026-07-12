// Command relkit releases the independently versioned components of a Go monorepo.
//
// A monorepo that ships several binaries cannot version them together: an extension that
// has not changed should not get a new version because a sibling did, and a consumer that
// pinned one should not be asked to re-verify all of them. So each component is tagged on
// its own timeline, as "<component>/vX.Y.Z", and a release builds, signs and publishes
// only that component.
//
// GoReleaser cannot express this in its open-source edition: it derives the version from
// `git describe`, so a tag like "token/v0.1.0" is not a version it can parse, and the
// prefixed-tag support is a paid feature. relkit therefore owns the whole artifact path,
// which also buys the thing a config-driven tool cannot do: it scopes a component by its
// real import graph (`go list -deps`), so a change to a shared package lands in the
// changelog of every component that imports it and no others, with nobody maintaining a
// list of paths.
//
// Usage:
//
//	relkit validate                     check the manifest against the tree (a CI gate)
//	relkit components                   list component names as JSON
//	relkit changed [--base <ref>]       which components have unreleased changes
//	relkit plan --tag token/v0.1.0      resolve a release without doing anything
//	relkit notes --tag token/v0.1.0     print the release notes
//	relkit build --tag token/v0.1.0     build, archive and checksum into ./dist
//	relkit release --tag token/v0.1.0   build, sign, and publish the GitHub release
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/ionalpha/go-ci/internal/release"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := run(ctx, os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, "relkit: "+err.Error())
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		usage()
		return errors.New("no command given")
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "validate":
		return cmdValidate(ctx, rest)
	case "components":
		return cmdComponents(ctx, rest)
	case "changed":
		return cmdChanged(ctx, rest)
	case "plan":
		return cmdPlan(ctx, rest)
	case "notes":
		return cmdNotes(ctx, rest)
	case "build":
		return cmdBuild(ctx, rest)
	case "release":
		return cmdRelease(ctx, rest)
	case "-h", "--help", "help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `relkit releases the independently versioned components of a Go monorepo.

Commands:
  validate     Check `+release.ManifestFile+` against the tree. Run it in CI.
  components   List component names as JSON.
  changed      Report which components have changes since their own last release.
  plan         Resolve a tag into the full release plan, as JSON. Does nothing else.
  notes        Print the release notes for a tag.
  build        Build, archive and checksum a component into --dist.
  release      Build, sign and publish the GitHub release for a tag.

Run "relkit <command> -h" for the flags of one command.
`)
}

// common holds the flags every command shares.
type common struct {
	dir      string
	manifest string
}

func (c *common) bind(fs *flag.FlagSet) {
	fs.StringVar(&c.dir, "C", ".", "module root to operate on")
	fs.StringVar(&c.manifest, "manifest", "", "path to the release manifest (default <dir>/"+release.ManifestFile+")")
}

// load resolves the working directory and loads the manifest, which every command needs.
func (c *common) load() (string, string, *release.Manifest, error) {
	dir, err := filepath.Abs(c.dir)
	if err != nil {
		return "", "", nil, err
	}
	path := c.manifest
	if path == "" {
		path = filepath.Join(dir, release.ManifestFile)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", "", nil, err
	}
	m, err := release.LoadManifest(path, dir)
	if err != nil {
		return "", "", nil, err
	}
	return dir, path, m, nil
}

func parse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	return nil
}

// requireTag reads and parses a --tag that is not optional.
func requireTag(s string) (release.Tag, error) {
	if s == "" {
		return release.Tag{}, errors.New("--tag is required, e.g. --tag token/v0.1.0")
	}
	return release.ParseTag(s)
}

func emitJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func cmdValidate(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	var c common
	c.bind(fs)
	if err := parse(fs, args); err != nil {
		return err
	}
	// Loading is validating: LoadManifest resolves defaults and checks every invariant
	// against the tree, so there is no second, weaker code path that a release could
	// take past a check this gate enforces.
	_, path, m, err := c.load()
	if err != nil {
		return err
	}
	fmt.Printf("%s is valid: %d component(s): %s\n",
		filepath.Base(path), len(m.Components), strings.Join(m.Names(), ", "))
	return nil
}

func cmdComponents(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("components", flag.ContinueOnError)
	var c common
	c.bind(fs)
	if err := parse(fs, args); err != nil {
		return err
	}
	_, _, m, err := c.load()
	if err != nil {
		return err
	}
	return emitJSON(m.Names())
}

// changedComponent is one row of `relkit changed`.
type changedComponent struct {
	Name     string `json:"name"`
	Changed  bool   `json:"changed"`
	Released string `json:"released,omitempty"` // its last release tag, if any
}

func cmdChanged(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("changed", flag.ContinueOnError)
	var c common
	c.bind(fs)
	head := fs.String("head", "HEAD", "commit to compare against each component's last release")
	only := fs.Bool("only-changed", false, "list only the components that changed")
	if err := parse(fs, args); err != nil {
		return err
	}
	dir, path, m, err := c.load()
	if err != nil {
		return err
	}

	var out []changedComponent
	for _, name := range m.Names() {
		changed, last, err := release.NeedsRelease(ctx, dir, path, m, name, *head)
		if err != nil {
			return err
		}
		if *only && !changed {
			continue
		}
		out = append(out, changedComponent{Name: name, Changed: changed, Released: last})
	}
	if out == nil {
		out = []changedComponent{}
	}
	return emitJSON(out)
}

func cmdPlan(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	var c common
	c.bind(fs)
	tag := fs.String("tag", "", "release tag, e.g. token/v0.1.0")
	head := fs.String("head", "HEAD", "commit to build")
	if err := parse(fs, args); err != nil {
		return err
	}
	if _, err := requireTag(*tag); err != nil {
		return err
	}
	dir, path, m, err := c.load()
	if err != nil {
		return err
	}
	p, err := release.NewPlan(ctx, dir, path, m, *tag, *head)
	if err != nil {
		return err
	}
	return emitJSON(p)
}

func cmdNotes(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("notes", flag.ContinueOnError)
	var c common
	c.bind(fs)
	tag := fs.String("tag", "", "release tag, e.g. token/v0.1.0")
	head := fs.String("head", "HEAD", "commit to build")
	repo := fs.String("repo", repoFromEnv(), "owner/name, for compare links")
	if err := parse(fs, args); err != nil {
		return err
	}
	t, err := requireTag(*tag)
	if err != nil {
		return err
	}
	dir, path, m, err := c.load()
	if err != nil {
		return err
	}
	p, err := release.NewPlan(ctx, dir, path, m, *tag, *head)
	if err != nil {
		return err
	}
	fmt.Print(p.Changelog.Render(*repo, t, ""))
	return nil
}

func cmdBuild(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	var c common
	c.bind(fs)
	tag := fs.String("tag", "", "release tag, e.g. token/v0.1.0")
	head := fs.String("head", "HEAD", "commit to build")
	dist := fs.String("dist", "dist", "output directory for the artifacts")
	sbom := fs.Bool("sbom", false, "also write a CycloneDX SBOM per archive (needs syft)")
	if err := parse(fs, args); err != nil {
		return err
	}
	if _, err := requireTag(*tag); err != nil {
		return err
	}
	dir, path, m, err := c.load()
	if err != nil {
		return err
	}
	arts, err := buildArtifacts(ctx, dir, path, m, *tag, *head, *dist, *sbom)
	if err != nil {
		return err
	}
	for _, a := range arts {
		fmt.Println(a.Name)
	}
	return nil
}

// buildArtifacts is the one build path: `relkit build` and `relkit release` both call it,
// so what a maintainer inspects locally is byte-for-byte what the release publishes.
func buildArtifacts(ctx context.Context, dir, manifestPath string, m *release.Manifest, tagStr, head, dist string, sbom bool) ([]release.Artifact, error) {
	t, err := release.ParseTag(tagStr)
	if err != nil {
		return nil, err
	}
	comp, err := m.Component(t.Component)
	if err != nil {
		return nil, err
	}
	p, err := release.NewPlan(ctx, dir, manifestPath, m, tagStr, head)
	if err != nil {
		return nil, err
	}
	// dist is emptied first so a stale archive from an earlier run cannot be hashed into
	// checksums.txt and published as if it belonged to this release.
	if err := os.RemoveAll(dist); err != nil {
		return nil, err
	}

	arts, err := release.Build(ctx, dir, comp, t, p.Commit, p.CommitTime, dist)
	if err != nil {
		return nil, err
	}
	if sbom {
		sboms, err := release.SBOM(ctx, arts)
		if err != nil {
			return nil, err
		}
		arts = append(arts, sboms...)
	}
	// The checksum file is written last and over everything else, so the signature it
	// carries covers every artifact the release publishes, SBOMs included.
	sums, err := release.WriteChecksums(dist, arts)
	if err != nil {
		return nil, err
	}
	return append(arts, sums), nil
}

func cmdRelease(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("release", flag.ContinueOnError)
	var c common
	c.bind(fs)
	tag := fs.String("tag", "", "release tag, e.g. token/v0.1.0")
	head := fs.String("head", "HEAD", "commit to build")
	dist := fs.String("dist", "dist", "output directory for the artifacts")
	repo := fs.String("repo", repoFromEnv(), "owner/name to publish to")
	sbom := fs.Bool("sbom", true, "write a CycloneDX SBOM per archive (needs syft)")
	sign := fs.Bool("sign", true, "sign checksums.txt with keyless cosign (needs an OIDC token)")
	dry := fs.Bool("dry-run", false, "build and sign, but do not create the GitHub release")
	if err := parse(fs, args); err != nil {
		return err
	}
	t, err := requireTag(*tag)
	if err != nil {
		return err
	}
	if *repo == "" {
		return errors.New("--repo is required (owner/name), or set GITHUB_REPOSITORY")
	}
	dir, path, m, err := c.load()
	if err != nil {
		return err
	}

	// Everything that can be checked without side effects is checked before anything is
	// built, so a bad release fails while it is still free to fail.
	if err := release.PreflightRelease(ctx, dir, t, *head); err != nil {
		return err
	}

	arts, err := buildArtifacts(ctx, dir, path, m, *tag, *head, *dist, *sbom)
	if err != nil {
		return err
	}

	identity := ""
	if *sign {
		var sums release.Artifact
		for _, a := range arts {
			if a.Kind == release.KindChecksums {
				sums = a
			}
		}
		sigs, err := release.Sign(ctx, sums)
		if err != nil {
			return err
		}
		arts = append(arts, sigs...)
		for _, a := range sigs {
			if a.Kind == release.KindCertificate {
				// The notes tell a consumer exactly which identity to pin, read back
				// out of the certificate Sigstore just issued rather than guessed.
				if identity, err = release.CertificateIdentity(a.Path); err != nil {
					return err
				}
			}
		}
	}

	p, err := release.NewPlan(ctx, dir, path, m, *tag, *head)
	if err != nil {
		return err
	}
	notes := p.Changelog.Render(*repo, t, identity)

	if *dry {
		fmt.Fprintf(os.Stderr, "dry run: would publish %s to %s with %d asset(s)\n", t, *repo, len(arts))
		fmt.Print(notes)
		return nil
	}
	pub := release.Publisher{Repo: *repo, Dir: dir}
	if err := pub.Publish(ctx, t, notes, arts); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "published %s with %d asset(s)\n", t, len(arts))
	return nil
}

// repoFromEnv reads the repository GitHub Actions is running for, so a workflow does not
// have to pass what the runner already knows.
func repoFromEnv() string { return os.Getenv("GITHUB_REPOSITORY") }
