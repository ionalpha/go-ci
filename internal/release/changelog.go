package release

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// Changelog is the set of commits a release ships, already filtered to the component.
type Changelog struct {
	// Previous is the component's last release, if it had one.
	Previous string `json:"previous,omitempty"`

	// Groups are the rendered sections, in display order.
	Groups []ChangeGroup `json:"groups,omitempty"`

	// Skipped counts commits in the range that touched no source this component is
	// built from. They are reported rather than hidden: "12 commits, none of them
	// yours" is the useful answer when someone wonders why a release is empty.
	Skipped int `json:"skipped"`
}

// ChangeGroup is one titled section of the changelog.
type ChangeGroup struct {
	Title   string   `json:"title"`
	Entries []string `json:"entries"`
}

// Empty reports whether the release contains no changes to this component.
func (c Changelog) Empty() bool { return len(c.Groups) == 0 }

// conventionalCommit splits a Conventional Commits subject into its type, optional scope,
// breaking marker, and description.
var conventionalCommit = regexp.MustCompile(`^([a-zA-Z]+)(?:\(([^)]*)\))?(!)?:\s*(.+)$`)

// changeGroups are the sections a changelog renders, in order. A commit type not listed
// here is dropped: docs, test and chore commits are noise in a consumer-facing release
// note, and a commit that is not conventional at all is surfaced under "Other" rather
// than being silently lost.
var changeGroups = []struct {
	title string
	types []string
}{
	{"Breaking changes", nil}, // populated by the "!" marker, not by type
	{"Features", []string{"feat"}},
	{"Bug fixes", []string{"fix"}},
	{"Performance", []string{"perf"}},
	{"Security", []string{"sec", "security"}},
	{"Other changes", []string{"refactor", "revert", ""}},
}

// BuildChangelog collects the commits between the component's previous release and head,
// keeping only those that touched source the component is actually built from.
//
// The filter is the import graph, not a path prefix. A commit to a shared package the
// token imports is the token's change and appears here; the same commit is invisible to a
// component that does not import it. That is what makes a per-component release note
// truthful in a monorepo where components share code.
func BuildChangelog(ctx context.Context, g Git, t Tag, head string, scope SourceScope) (Changelog, error) {
	prev, hasPrev, err := g.PreviousTag(ctx, t)
	if err != nil {
		return Changelog{}, err
	}
	base := ""
	cl := Changelog{}
	if hasPrev {
		base = prev.String()
		cl.Previous = base
	}

	commits, err := g.Log(ctx, base, head)
	if err != nil {
		return Changelog{}, err
	}

	byGroup := map[string][]string{}
	for _, c := range commits {
		files, err := g.CommitFiles(ctx, c.SHA)
		if err != nil {
			return Changelog{}, err
		}
		if !scope.CoversAny(files) {
			cl.Skipped++
			continue
		}
		title, entry, ok := classify(c)
		if !ok {
			cl.Skipped++
			continue
		}
		byGroup[title] = append(byGroup[title], entry)
	}

	for _, g := range changeGroups {
		if entries := byGroup[g.title]; len(entries) > 0 {
			cl.Groups = append(cl.Groups, ChangeGroup{Title: g.title, Entries: entries})
		}
	}
	return cl, nil
}

// classify maps a commit to its changelog group and rendered line, reporting false for a
// commit that should not appear (docs, tests, chores, CI).
func classify(c LogEntry) (group, entry string, ok bool) {
	short := c.SHA
	if len(short) > 7 {
		short = short[:7]
	}

	m := conventionalCommit.FindStringSubmatch(c.Subject)
	if m == nil {
		// Not a conventional commit. It changed code in scope, so it is real work and
		// belongs in the notes; it just cannot be categorized.
		return "Other changes", fmt.Sprintf("%s (%s)", c.Subject, short), true
	}
	typ, scope, breaking, desc := strings.ToLower(m[1]), m[2], m[3] == "!", m[4]

	line := desc
	if scope != "" {
		line = scope + ": " + desc
	}
	line = fmt.Sprintf("%s (%s)", line, short)

	if breaking {
		return "Breaking changes", line, true
	}
	for _, g := range changeGroups {
		for _, t := range g.types {
			if t != "" && t == typ {
				return g.title, line, true
			}
		}
	}
	return "", "", false // docs, test, chore, ci, style, build: not consumer-facing
}

// Render turns the changelog into the Markdown body of a GitHub release.
func (c Changelog) Render(repo string, t Tag, identity string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "## %s %s\n\n", t.Component, t.Version)

	if c.Empty() {
		b.WriteString("No changes to this component's sources since its last release.\n\n")
	}
	for _, g := range c.Groups {
		fmt.Fprintf(&b, "### %s\n\n", g.Title)
		for _, e := range g.Entries {
			fmt.Fprintf(&b, "- %s\n", e)
		}
		b.WriteString("\n")
	}
	if c.Previous != "" && repo != "" {
		fmt.Fprintf(&b, "**Full diff:** https://github.com/%s/compare/%s...%s\n\n",
			repo, c.Previous, t)
	}

	b.WriteString("### Verifying this release\n\n")
	b.WriteString("Every archive is listed by digest in `checksums.txt`, and that file carries a keyless ")
	b.WriteString("cosign signature made by the release workflow. Download `checksums.txt`, its `.sig` and ")
	b.WriteString("`.pem`, then:\n\n")
	b.WriteString("```sh\n")
	if identity != "" {
		b.WriteString(VerifyCommand(identity))
	} else {
		b.WriteString("cosign verify-blob checksums.txt --signature checksums.txt.sig --certificate checksums.txt.pem")
	}
	b.WriteString("\n```\n\n")
	b.WriteString("Then check the archive you downloaded against it:\n\n")
	b.WriteString("```sh\nsha256sum --check --ignore-missing checksums.txt\n```\n")

	// The build provenance is a separate statement with a separate verifier, and its
	// signer is the workflow's repo, not the released one. Spelling that out here is the
	// difference between a command that works and one that rejects a valid attestation.
	if signer := SignerRepo(identity); signer != "" && repo != "" {
		b.WriteString("\nEach archive also carries SLSA build provenance: a signed statement of which ")
		b.WriteString("workflow built it, from which commit.\n\n")
		fmt.Fprintf(&b, "```sh\ngh attestation verify <archive> \\\n  --repo %s \\\n  --signer-repo %s\n```\n",
			repo, signer)
		if signer != repo {
			fmt.Fprintf(&b, "\n`--signer-repo` is required because the release is built by a reusable workflow "+
				"in `%s`, so that is the identity Sigstore binds the signature to, not `%s`.\n", signer, repo)
		}
	}
	return b.String()
}
