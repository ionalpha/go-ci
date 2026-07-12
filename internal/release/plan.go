package release

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Plan is everything a release will do, resolved before anything is built or published.
//
// It exists so the destructive half of a release is never the first time a mistake is
// noticed. A plan can be printed (`relkit plan`) and diffed against expectations, and the
// build and publish steps consume the same struct, so what is shown is what runs.
type Plan struct {
	Tag        string     `json:"tag"`
	Component  string     `json:"component"`
	Version    string     `json:"version"`
	Prerelease bool       `json:"prerelease"`
	Commit     string     `json:"commit"`
	CommitTime time.Time  `json:"commit_time"`
	Main       string     `json:"main"`
	Binary     string     `json:"binary"`
	Platforms  []Platform `json:"platforms"`

	// Archives are the filenames the release will publish, so a consumer's resolver can
	// be checked against them without downloading anything.
	Archives []string `json:"archives"`

	// Scope is the source footprint the changelog and the "needs a release" check use.
	Scope SourceScope `json:"scope"`

	// Changelog is what the release notes will say.
	Changelog Changelog `json:"changelog"`
}

// NewPlan resolves a tag against the manifest and the repository into a full plan.
func NewPlan(ctx context.Context, dir, manifestPath string, m *Manifest, tagStr, head string) (*Plan, error) {
	t, err := ParseTag(tagStr)
	if err != nil {
		return nil, err
	}
	c, err := m.Component(t.Component)
	if err != nil {
		return nil, err
	}

	g := Git{Dir: dir}
	commit, err := g.Commit(ctx, head)
	if err != nil {
		return nil, err
	}
	commitTime, err := g.CommitTime(ctx, head)
	if err != nil {
		return nil, err
	}
	scope, err := Scope(ctx, dir, c, manifestPath)
	if err != nil {
		return nil, err
	}
	cl, err := BuildChangelog(ctx, g, t, head, scope)
	if err != nil {
		return nil, err
	}

	p := &Plan{
		Tag:        t.String(),
		Component:  c.Name,
		Version:    t.Version.String(),
		Prerelease: t.Version.IsPrerelease(),
		Commit:     commit,
		CommitTime: commitTime,
		Main:       c.Main,
		Binary:     c.Binary,
		Platforms:  c.Platforms(),
		Scope:      scope,
		Changelog:  cl,
	}
	for _, plat := range c.Platforms() {
		p.Archives = append(p.Archives, ArchiveName(c, t.Version, plat))
	}
	return p, nil
}

// NeedsRelease reports whether a component has changes since its last release, i.e.
// whether cutting a new tag for it would ship anything.
//
// This is the question a monorepo has to answer before it can tag anything, and it is the
// one a path-glob release tool answers wrong. The comparison is against the component's
// own previous tag, and the filter is its import graph, so an extension is "changed" when
// a shared package it depends on changed, and not when a sibling extension changed.
func NeedsRelease(ctx context.Context, dir, manifestPath string, m *Manifest, name, head string) (bool, string, error) {
	c, err := m.Component(name)
	if err != nil {
		return false, "", err
	}
	g := Git{Dir: dir}
	scope, err := Scope(ctx, dir, c, manifestPath)
	if err != nil {
		return false, "", err
	}

	// The highest existing tag of this component, whatever its version, is the baseline.
	tags, err := g.Tags(ctx)
	if err != nil {
		return false, "", err
	}
	var latest *Tag
	for _, s := range tags {
		t, err := ParseTag(s)
		if err != nil || t.Component != name {
			continue
		}
		if latest == nil || t.Version.Compare(latest.Version) > 0 {
			tc := t
			latest = &tc
		}
	}
	if latest == nil {
		return true, "", nil // never released, so everything it contains is unreleased
	}
	files, err := g.ChangedFiles(ctx, latest.String(), head)
	if err != nil {
		return false, "", err
	}
	return scope.CoversAny(files), latest.String(), nil
}

// PreflightRelease checks everything that must hold before a release is built, so a
// failure costs nothing instead of leaving a half-published tag behind.
func PreflightRelease(ctx context.Context, dir string, t Tag, head string) error {
	g := Git{Dir: dir}

	clean, err := g.IsClean(ctx)
	if err != nil {
		return err
	}
	if !clean {
		return errors.New("working tree has uncommitted changes; a release must be built from the tagged commit, not from local edits")
	}

	exists, err := g.TagExists(ctx, t.String())
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("tag %s does not exist in this repository", t)
	}

	// The tag has to point at what is being built. In CI the tag push is the trigger, so
	// this normally holds; it fails loudly when someone runs a release locally from the
	// wrong branch, which is exactly when it matters.
	tagCommit, err := g.Commit(ctx, t.String()+"^{commit}")
	if err != nil {
		return err
	}
	headCommit, err := g.Commit(ctx, head)
	if err != nil {
		return err
	}
	if tagCommit != headCommit {
		return fmt.Errorf("tag %s points at %s but the build would use %s; check out the tag", t, short(tagCommit), short(headCommit))
	}
	return nil
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
