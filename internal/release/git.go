package release

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Git runs git commands against one working tree.
type Git struct{ Dir string }

func (g Git) run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // G204: args are literals and validated refs, never a shell string
	cmd.Dir = g.Dir
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Commit is the SHA a ref resolves to.
func (g Git) Commit(ctx context.Context, ref string) (string, error) {
	return g.run(ctx, "rev-parse", ref)
}

// CommitTime is the committer timestamp of ref, in UTC. It is the release's single
// source of time: build stamps and archive entry timestamps both come from it, so a
// rebuild of the same commit produces byte-identical archives no matter when it runs.
func (g Git) CommitTime(ctx context.Context, ref string) (time.Time, error) {
	out, err := g.run(ctx, "show", "-s", "--format=%ct", ref)
	if err != nil {
		return time.Time{}, err
	}
	secs, err := strconv.ParseInt(out, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse commit time %q: %w", out, err)
	}
	return time.Unix(secs, 0).UTC(), nil
}

// Tags lists every tag in the repository.
func (g Git) Tags(ctx context.Context) ([]string, error) {
	out, err := g.run(ctx, "tag", "--list")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

// PreviousTag returns the highest release tag of the same component that precedes cur by
// semver precedence, and whether one exists. It is scoped to the component on purpose: a
// component's changelog spans its own releases, so an unrelated extension's tag landing
// in between must not truncate it.
func (g Git) PreviousTag(ctx context.Context, cur Tag) (Tag, bool, error) {
	all, err := g.Tags(ctx)
	if err != nil {
		return Tag{}, false, err
	}
	var prior []Tag
	for _, s := range all {
		t, err := ParseTag(s)
		if err != nil || t.Component != cur.Component {
			continue // not a release tag of this component
		}
		if t.Version.Compare(cur.Version) < 0 {
			prior = append(prior, t)
		}
	}
	if len(prior) == 0 {
		return Tag{}, false, nil
	}
	sort.Slice(prior, func(i, j int) bool { return prior[i].Version.Compare(prior[j].Version) < 0 })
	return prior[len(prior)-1], true, nil
}

// LogEntry is one commit in a changelog range.
type LogEntry struct {
	SHA     string
	Subject string
}

// Log lists the commits reachable from head but not from base, oldest first. A base of ""
// means the component has never been released, so the whole history is in range.
func (g Git) Log(ctx context.Context, base, head string) ([]LogEntry, error) {
	rng := head
	if base != "" {
		rng = base + ".." + head
	}
	// A unit separator between fields and a record separator between commits: a commit
	// subject can contain anything, including a newline-looking sequence, so the parse
	// must not depend on the subject's content.
	out, err := g.run(ctx, "log", "--no-merges", "--reverse", "--format=%H%x1f%s%x1e", rng)
	if err != nil {
		return nil, err
	}
	var entries []LogEntry
	for _, rec := range strings.Split(out, "\x1e") {
		rec = strings.Trim(rec, "\n")
		if rec == "" {
			continue
		}
		sha, subj, ok := strings.Cut(rec, "\x1f")
		if !ok {
			continue
		}
		entries = append(entries, LogEntry{SHA: sha, Subject: subj})
	}
	return entries, nil
}

// ChangedFiles lists the repo-relative paths that differ between base and head. A base of
// "" means every tracked file, which is what a first release should treat as changed.
func (g Git) ChangedFiles(ctx context.Context, base, head string) ([]string, error) {
	var out string
	var err error
	if base == "" {
		out, err = g.run(ctx, "ls-tree", "-r", "--name-only", head)
	} else {
		out, err = g.run(ctx, "diff", "--name-only", base+".."+head)
	}
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

// CommitFiles lists the paths one commit touched.
func (g Git) CommitFiles(ctx context.Context, sha string) ([]string, error) {
	out, err := g.run(ctx, "show", "--no-commit-id", "--name-only", "--format=", "-m", "--first-parent", sha)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, f := range strings.Split(out, "\n") {
		if f = strings.TrimSpace(f); f != "" {
			files = append(files, f)
		}
	}
	return files, nil
}

// IsClean reports whether the working tree has no uncommitted changes to tracked files.
// A release is built from a commit, so building one from a dirty tree would publish and
// sign artifacts whose source is not the tagged commit.
func (g Git) IsClean(ctx context.Context) (bool, error) {
	out, err := g.run(ctx, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return false, err
	}
	return out == "", nil
}

// TagExists reports whether the named tag exists locally.
func (g Git) TagExists(ctx context.Context, tag string) (bool, error) {
	out, err := g.run(ctx, "tag", "--list", tag)
	if err != nil {
		return false, err
	}
	return out != "", nil
}
