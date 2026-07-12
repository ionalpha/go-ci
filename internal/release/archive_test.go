package release

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// stageFiles writes source files for an archive and returns the entries pointing at them.
func stageFiles(t *testing.T, files map[string]string) []archiveEntry {
	t.Helper()
	dir := t.TempDir()
	var entries []archiveEntry
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, archiveEntry{Name: name, Path: p, Mode: 0o755})
	}
	return entries
}

// TestArchiveIsReproducible is the test that protects the release's root of trust. The
// release signs checksums.txt, which commits to each archive by digest, so if two builds
// of the same commit produced different archive bytes, the published signature could
// never be reproduced or independently checked. Every source of nondeterminism the
// packer could have (entry order, mtimes, ownership, the gzip header's own timestamp)
// must therefore be pinned.
func TestArchiveIsReproducible(t *testing.T) {
	t.Parallel()
	modTime := time.Unix(1_700_000_000, 0).UTC()

	for _, useZip := range []bool{false, true} {
		name := "tar.gz"
		if useZip {
			name = "zip"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// The same content, staged twice in different temp dirs, listed in a
			// different order, at a different wall-clock moment.
			a := stageFiles(t, map[string]string{"token": "binary", "LICENSE": "text"})
			b := stageFiles(t, map[string]string{"LICENSE": "text", "token": "binary"})

			out := t.TempDir()
			pathA, pathB := filepath.Join(out, "a"), filepath.Join(out, "b")
			if err := writeArchive(pathA, a, modTime, useZip); err != nil {
				t.Fatal(err)
			}
			if err := writeArchive(pathB, b, modTime, useZip); err != nil {
				t.Fatal(err)
			}
			bytesA, err := os.ReadFile(pathA)
			if err != nil {
				t.Fatal(err)
			}
			bytesB, err := os.ReadFile(pathB)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(bytesA, bytesB) {
				t.Fatalf("two builds of the same content produced different archives (%d vs %d bytes)", len(bytesA), len(bytesB))
			}

			// A different commit time must produce different bytes, or the timestamp
			// is not being recorded at all and the check above proves nothing.
			pathC := filepath.Join(out, "c")
			if err := writeArchive(pathC, a, modTime.Add(time.Hour), useZip); err != nil {
				t.Fatal(err)
			}
			bytesC, err := os.ReadFile(pathC)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Equal(bytesA, bytesC) {
				t.Fatal("archives at different commit times are identical: the timestamp is not recorded")
			}
		})
	}
}

// TestTarGzHeadersCarryNoLocalState checks the individual fields that would otherwise
// leak the build machine into the artifact: the uid/gid of whoever ran the build, their
// username, and the moment they ran it.
func TestTarGzHeadersCarryNoLocalState(t *testing.T) {
	t.Parallel()
	modTime := time.Unix(1_700_000_000, 0).UTC()
	entries := stageFiles(t, map[string]string{"token": "binary", "LICENSE": "text"})
	out := filepath.Join(t.TempDir(), "a.tar.gz")
	if err := writeArchive(out, entries, modTime, false); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}

	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if gz.Name != "" || !gz.ModTime.IsZero() {
		t.Errorf("gzip header leaks state: name=%q modtime=%v", gz.Name, gz.ModTime)
	}

	tr := tar.NewReader(gz)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
		if h.Uid != 0 || h.Gid != 0 || h.Uname != "" || h.Gname != "" {
			t.Errorf("%s: archive records the building user (uid=%d gid=%d uname=%q)", h.Name, h.Uid, h.Gid, h.Uname)
		}
		if !h.ModTime.Equal(modTime) {
			t.Errorf("%s: modtime = %v, want the commit time %v", h.Name, h.ModTime, modTime)
		}
		if h.Mode != 0o755 {
			t.Errorf("%s: mode = %o, want 755 so the binary is executable when unpacked", h.Name, h.Mode)
		}
	}
	// Sorted, so the order does not depend on how the map or the matrix was iterated.
	if len(names) != 2 || names[0] != "LICENSE" || names[1] != "token" {
		t.Errorf("entries = %v, want [LICENSE token] in sorted order", names)
	}
}

func TestZipEntriesAreOrderedAndExecutable(t *testing.T) {
	t.Parallel()
	modTime := time.Unix(1_700_000_000, 0).UTC()
	entries := stageFiles(t, map[string]string{"token.exe": "binary", "LICENSE": "text"})
	out := filepath.Join(t.TempDir(), "a.zip")
	if err := writeArchive(out, entries, modTime, true); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.OpenReader(out)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = zr.Close() }()

	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
		if !f.Modified.Equal(modTime) {
			t.Errorf("%s: modified = %v, want the commit time %v", f.Name, f.Modified, modTime)
		}
	}
	if len(names) != 2 || names[0] != "LICENSE" || names[1] != "token.exe" {
		t.Errorf("entries = %v, want [LICENSE token.exe] in sorted order", names)
	}
}

func TestArchiveRejectsDuplicateEntries(t *testing.T) {
	t.Parallel()
	// Two extra_files with the same basename would flatten onto each other, and one
	// would silently win. That must be an error, not a coin flip.
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	entries := []archiveEntry{{Name: "LICENSE", Path: p}, {Name: "LICENSE", Path: p}}
	err := writeArchive(filepath.Join(dir, "a.tar.gz"), entries, time.Unix(0, 0), false)
	if err == nil {
		t.Fatal("writeArchive accepted a duplicate entry name")
	}
}
