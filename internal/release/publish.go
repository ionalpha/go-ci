package release

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Publisher creates the GitHub release and uploads its assets.
//
// It shells out to `gh`, which is preinstalled on GitHub runners and already authenticates
// from GITHUB_TOKEN, so the release step needs no bespoke API client and no second way to
// hold a credential.
type Publisher struct {
	Repo string // "owner/name"
	Dir  string // working tree, so gh resolves the right repo
}

// Publish creates the release for tag t with the given notes and uploads every artifact.
//
// The release is created first and the assets uploaded into it, so a partially uploaded
// release is visible as such. It refuses to touch a release that already exists: a
// published tag is immutable in every consumer's mind, and quietly replacing its assets
// would invalidate a signature someone has already pinned.
func (p Publisher) Publish(ctx context.Context, t Tag, notes string, artifacts []Artifact) error {
	exists, err := p.releaseExists(ctx, t)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("release %s already exists; releases are immutable, so cut a new version instead of overwriting it", t)
	}

	notesFile, err := os.CreateTemp("", "relkit-notes-*.md")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(notesFile.Name()) }()
	if _, err := notesFile.WriteString(notes); err != nil {
		return err
	}
	if err := notesFile.Close(); err != nil {
		return err
	}

	args := []string{
		"release", "create", t.String(),
		"--repo", p.Repo,
		"--title", t.Component + " " + t.Version.String(),
		"--notes-file", notesFile.Name(),
		"--verify-tag", // the tag must already exist and point at the built commit
	}
	if t.Version.IsPrerelease() {
		args = append(args, "--prerelease")
	}
	for _, a := range artifacts {
		args = append(args, a.Path)
	}
	return p.gh(ctx, args...)
}

func (p Publisher) releaseExists(ctx context.Context, t Tag) (bool, error) {
	cmd := exec.CommandContext(ctx, "gh", "release", "view", t.String(), "--repo", p.Repo, "--json", "tagName") //nolint:gosec // G204: a parsed tag and an owner/name, passed as argv
	cmd.Dir = p.Dir
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return false, nil // gh exits non-zero when the release is not found
		}
		return false, fmt.Errorf("gh release view: %w", err)
	}
	return true, nil
}

func (p Publisher) gh(ctx context.Context, args ...string) error {
	if _, err := exec.LookPath("gh"); err != nil {
		return fmt.Errorf("the GitHub CLI (gh) is not installed: %w", err)
	}
	cmd := exec.CommandContext(ctx, "gh", args...) //nolint:gosec // G204: argv built from validated inputs, never a shell string
	cmd.Dir = p.Dir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gh %s: %w", strings.Join(args[:2], " "), err)
	}
	return nil
}
