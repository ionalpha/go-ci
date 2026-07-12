package release

import (
	"sort"
	"strings"
	"testing"
)

func TestParseTag(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in        string
		component string
		version   string
		wantErr   string
	}{
		{in: "token/v0.1.0", component: "token", version: "v0.1.0"},
		{in: "token/v1.2.3-rc.1", component: "token", version: "v1.2.3-rc.1"},
		{in: "token/v1.2.3+build.5", component: "token", version: "v1.2.3+build.5"},
		{in: "token/v1.2.3-rc.1+build.5", component: "token", version: "v1.2.3-rc.1+build.5"},
		{in: "web3-tools/v10.20.30", component: "web3-tools", version: "v10.20.30"},

		{in: "v0.1.0", wantErr: "want <component>/v<semver>"},
		{in: "token", wantErr: "want <component>/v<semver>"},
		{in: "token/0.1.0", wantErr: `must start with "v"`},
		{in: "token/v0.1", wantErr: "want vMAJOR.MINOR.PATCH"},
		{in: "token/v0.1.0.1", wantErr: "want vMAJOR.MINOR.PATCH"},
		{in: "token/v01.1.0", wantErr: "leading zero"},
		{in: "token/v0.1.0-", wantErr: "prerelease: is empty"},
		{in: "token/v0.1.0-rc..1", wantErr: "empty identifier"},
		{in: "token/v0.1.0-rc.01", wantErr: "leading zero"},
		{in: "token/v-1.0.0", wantErr: "want vMAJOR.MINOR.PATCH"},
		{in: "/v0.1.0", wantErr: "component name is empty"},
		{in: "Token/v0.1.0", wantErr: "must start with a lowercase letter or digit"},
		{in: "-token/v0.1.0", wantErr: "must start with a lowercase letter or digit"},
		{in: "to ken/v0.1.0", wantErr: `contains " "`},
		// A nested path would make the tag ambiguous with a component named "a".
		{in: "a/b/v0.1.0", wantErr: `must start with "v"`},
	}
	for _, tc := range tests {
		got, err := ParseTag(tc.in)
		switch {
		case tc.wantErr != "":
			if err == nil {
				t.Errorf("ParseTag(%q) = %v, want error %q", tc.in, got, tc.wantErr)
				continue
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("ParseTag(%q) error = %q, want it to contain %q", tc.in, err, tc.wantErr)
			}
		case err != nil:
			t.Errorf("ParseTag(%q) unexpected error: %v", tc.in, err)
		default:
			if got.Component != tc.component || got.Version.String() != tc.version {
				t.Errorf("ParseTag(%q) = %s/%s, want %s/%s", tc.in, got.Component, got.Version, tc.component, tc.version)
			}
			// The tag must round-trip: it is a git ref and a URL path element, so a
			// parse that silently normalized it would point at a tag nobody created.
			if got.String() != tc.in {
				t.Errorf("ParseTag(%q).String() = %q, want the input back", tc.in, got.String())
			}
		}
	}
}

// FuzzParseTag asserts the two invariants a release tag must satisfy no matter what is
// thrown at it: parsing never panics, and anything it accepts round-trips exactly. The
// round-trip is the security-relevant half. The tag string is spliced into a git ref, a
// release URL and an artifact name, so if two different strings could parse to the same
// tag (or one could parse to a different string than it came from), a consumer could
// verify one release and download another.
func FuzzParseTag(f *testing.F) {
	for _, s := range []string{
		"token/v0.1.0", "token/v1.2.3-rc.1+b.2", "", "/", "v1.0.0", "a/v0.0.0",
		"token/v0.1.0\n", "token//v1.0.0", "tok\x00en/v1.0.0", "token/v1.0.0-0a",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		tag, err := ParseTag(s)
		if err != nil {
			return
		}
		if got := tag.String(); got != s {
			t.Fatalf("ParseTag(%q) round-tripped to %q", s, got)
		}
		// An accepted tag must be usable unescaped as a ref and a path element.
		if strings.ContainsAny(tag.Component, "/\\ \t\n\r\x00") {
			t.Fatalf("accepted component %q contains a separator or control character", tag.Component)
		}
		if n := strings.Count(s, "/"); n != 1 {
			t.Fatalf("accepted tag %q has %d slashes, want exactly 1", s, n)
		}
		if _, err := ParseTag(tag.String()); err != nil {
			t.Fatalf("re-parsing %q failed: %v", tag, err)
		}
	})
}

func TestVersionCompare(t *testing.T) {
	t.Parallel()
	// semver.org §11's own precedence example, plus the build-metadata-is-ignored rule.
	ordered := []string{
		"v1.0.0-alpha", "v1.0.0-alpha.1", "v1.0.0-alpha.beta", "v1.0.0-beta",
		"v1.0.0-beta.2", "v1.0.0-beta.11", "v1.0.0-rc.1", "v1.0.0",
		"v1.0.1", "v1.1.0", "v2.0.0", "v10.0.0",
	}
	versions := make([]Version, len(ordered))
	for i, s := range ordered {
		v, err := ParseVersion(s)
		if err != nil {
			t.Fatalf("ParseVersion(%q): %v", s, err)
		}
		versions[i] = v
	}
	for i := range len(versions) - 1 {
		if versions[i].Compare(versions[i+1]) >= 0 {
			t.Errorf("%s should precede %s", versions[i], versions[i+1])
		}
		if versions[i+1].Compare(versions[i]) <= 0 {
			t.Errorf("Compare is not antisymmetric for %s and %s", versions[i], versions[i+1])
		}
		if versions[i].Compare(versions[i]) != 0 {
			t.Errorf("%s does not equal itself", versions[i])
		}
	}

	// Sorting a shuffled copy must reproduce the declared order: PreviousTag picks a
	// component's last release this way, and picking the wrong one would produce a
	// changelog that spans the wrong range.
	shuffled := []Version{versions[5], versions[0], versions[11], versions[7], versions[2]}
	sort.Slice(shuffled, func(i, j int) bool { return shuffled[i].Compare(shuffled[j]) < 0 })
	want := []string{"v1.0.0-alpha", "v1.0.0-alpha.beta", "v1.0.0-beta.11", "v1.0.0", "v10.0.0"}
	for i, v := range shuffled {
		if v.String() != want[i] {
			t.Errorf("sorted[%d] = %s, want %s", i, v, want[i])
		}
	}

	// Build metadata is not part of precedence.
	a, _ := ParseVersion("v1.0.0+a")
	b, _ := ParseVersion("v1.0.0+b")
	if a.Compare(b) != 0 {
		t.Errorf("build metadata must not affect precedence")
	}
}

func TestVersionIsPrerelease(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]bool{
		"v1.0.0":       false,
		"v1.0.0+b.1":   false,
		"v1.0.0-rc.1":  true,
		"v0.1.0-alpha": true,
	} {
		v, err := ParseVersion(in)
		if err != nil {
			t.Fatalf("ParseVersion(%q): %v", in, err)
		}
		if got := v.IsPrerelease(); got != want {
			t.Errorf("%q: IsPrerelease() = %v, want %v", in, got, want)
		}
	}
}
