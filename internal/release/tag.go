package release

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Tag is a component-scoped release tag: "<component>/v<semver>", e.g. "token/v0.1.0".
//
// A monorepo releases each component on its own timeline, so the tag has to say which
// component it releases. GitHub allows "/" in a tag name and serves release assets under
// it unchanged, so the prefixed form needs no encoding anywhere in the chain: git tag,
// release page, and asset download URL all use the same string.
type Tag struct {
	Component string  // component name, the part before the first "/"
	Version   Version // the "vX.Y.Z[-pre][+build]" part
}

func (t Tag) String() string { return t.Component + "/" + t.Version.String() }

// ParseTag parses a component-scoped release tag. It is strict on purpose: a tag is the
// input to a signed, immutable release, so anything ambiguous is rejected rather than
// normalized into something the publisher did not type.
func ParseTag(s string) (Tag, error) {
	name, ver, ok := strings.Cut(s, "/")
	if !ok {
		return Tag{}, fmt.Errorf("tag %q: want <component>/v<semver>, e.g. token/v0.1.0", s)
	}
	if err := ValidateComponentName(name); err != nil {
		return Tag{}, fmt.Errorf("tag %q: %w", s, err)
	}
	v, err := ParseVersion(ver)
	if err != nil {
		return Tag{}, fmt.Errorf("tag %q: %w", s, err)
	}
	return Tag{Component: name, Version: v}, nil
}

// ValidateComponentName enforces the character set a component name may use. The name is
// spliced into a git ref, a release asset filename, and a download URL, so it is limited
// to lowercase alphanumerics plus "-", "_" and ".", must start with an alphanumeric, and
// may not contain a path separator or a leading/trailing dot.
func ValidateComponentName(name string) error {
	switch {
	case name == "":
		return errors.New("component name is empty")
	case len(name) > 64:
		return fmt.Errorf("component name %q is longer than 64 characters", name)
	case name == "." || name == "..":
		return fmt.Errorf("component name %q is a path element", name)
	}
	for i := range len(name) {
		c := name[i]
		alnum := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
		if i == 0 && !alnum {
			return fmt.Errorf("component name %q must start with a lowercase letter or digit", name)
		}
		if !alnum && c != '-' && c != '_' && c != '.' {
			return fmt.Errorf("component name %q contains %q, but only a-z 0-9 - _ . are allowed", name, string(c))
		}
	}
	if strings.HasSuffix(name, ".") {
		return fmt.Errorf("component name %q ends with a dot", name)
	}
	return nil
}

// Version is a "v"-prefixed semantic version. Only the exact "vMAJOR.MINOR.PATCH" form,
// optionally with a prerelease and build suffix, is accepted; the leading "v" is required
// so the tag reads the way every Go release tag does.
type Version struct {
	Major, Minor, Patch int
	Prerelease          string // without the leading "-"; empty if absent
	Build               string // without the leading "+"; empty if absent
}

func (v Version) String() string {
	s := "v" + strconv.Itoa(v.Major) + "." + strconv.Itoa(v.Minor) + "." + strconv.Itoa(v.Patch)
	if v.Prerelease != "" {
		s += "-" + v.Prerelease
	}
	if v.Build != "" {
		s += "+" + v.Build
	}
	return s
}

// IsPrerelease reports whether the version carries a prerelease suffix, which is what
// marks the GitHub release as a prerelease.
func (v Version) IsPrerelease() bool { return v.Prerelease != "" }

// ParseVersion parses a "v"-prefixed semantic version per semver.org, rejecting the
// looseness (missing "v", missing patch, leading zeroes) that would let two spellings of
// the same release exist.
func ParseVersion(s string) (Version, error) {
	rest, ok := strings.CutPrefix(s, "v")
	if !ok {
		return Version{}, fmt.Errorf("version %q must start with %q", s, "v")
	}
	var v Version

	// Split off build metadata first: it may itself contain "-", so it must not be
	// mistaken for the start of a prerelease.
	if core, build, found := strings.Cut(rest, "+"); found {
		if err := validateDotIdents(build, false); err != nil {
			return Version{}, fmt.Errorf("version %q: build metadata: %w", s, err)
		}
		v.Build, rest = build, core
	}
	if core, pre, found := strings.Cut(rest, "-"); found {
		if err := validateDotIdents(pre, true); err != nil {
			return Version{}, fmt.Errorf("version %q: prerelease: %w", s, err)
		}
		v.Prerelease, rest = pre, core
	}

	parts := strings.Split(rest, ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("version %q: want vMAJOR.MINOR.PATCH", s)
	}
	nums := make([]int, 3)
	for i, p := range parts {
		n, err := parseNumericIdent(p)
		if err != nil {
			return Version{}, fmt.Errorf("version %q: %w", s, err)
		}
		nums[i] = n
	}
	v.Major, v.Minor, v.Patch = nums[0], nums[1], nums[2]
	return v, nil
}

// parseNumericIdent parses a semver numeric identifier: digits only, no leading zero
// unless the identifier is exactly "0".
func parseNumericIdent(p string) (int, error) {
	if p == "" {
		return 0, errors.New("empty numeric identifier")
	}
	if len(p) > 1 && p[0] == '0' {
		return 0, fmt.Errorf("numeric identifier %q has a leading zero", p)
	}
	n, err := strconv.Atoi(p)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("numeric identifier %q is not a non-negative integer", p)
	}
	return n, nil
}

// validateDotIdents validates a dot-separated identifier list (a prerelease or build
// metadata). Prerelease identifiers additionally forbid leading zeroes on numeric parts.
func validateDotIdents(s string, prerelease bool) error {
	if s == "" {
		return errors.New("is empty")
	}
	for _, ident := range strings.Split(s, ".") {
		if ident == "" {
			return errors.New("has an empty identifier")
		}
		numeric := true
		for i := range len(ident) {
			c := ident[i]
			switch {
			case c >= '0' && c <= '9':
			case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '-':
				numeric = false
			default:
				return fmt.Errorf("identifier %q contains %q", ident, string(c))
			}
		}
		if prerelease && numeric && len(ident) > 1 && ident[0] == '0' {
			return fmt.Errorf("numeric identifier %q has a leading zero", ident)
		}
	}
	return nil
}

// Compare orders two versions by precedence (semver §11): numeric fields first, then
// prerelease, with build metadata ignored. It reports -1, 0 or +1.
func (v Version) Compare(o Version) int {
	for _, p := range [][2]int{{v.Major, o.Major}, {v.Minor, o.Minor}, {v.Patch, o.Patch}} {
		if c := cmpInt(p[0], p[1]); c != 0 {
			return c
		}
	}
	switch {
	case v.Prerelease == "" && o.Prerelease == "":
		return 0
	case v.Prerelease == "": // a release outranks any prerelease of the same core version
		return 1
	case o.Prerelease == "":
		return -1
	}
	return comparePrerelease(v.Prerelease, o.Prerelease)
}

func comparePrerelease(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		an, aNum := asNum(as[i])
		bn, bNum := asNum(bs[i])
		switch {
		case aNum && bNum:
			if c := cmpInt(an, bn); c != 0 {
				return c
			}
		case aNum: // numeric identifiers have lower precedence than alphanumeric ones
			return -1
		case bNum:
			return 1
		default:
			if c := strings.Compare(as[i], bs[i]); c != 0 {
				return c
			}
		}
	}
	return cmpInt(len(as), len(bs)) // a larger set of identifiers wins if all else is equal
}

func asNum(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	return n, err == nil && n >= 0
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
