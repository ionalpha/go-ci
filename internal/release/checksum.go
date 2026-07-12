package release

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ChecksumsFile is the name of the digest manifest in every release.
const ChecksumsFile = "checksums.txt"

// WriteChecksums hashes each archive and writes a checksums.txt in the format
// `sha256sum -c` reads, then returns it as an artifact.
//
// This one file is the release's root of trust. It is the only thing signed, and it
// commits to every archive by digest, so a consumer verifies one signature and then
// checks any archive it downloads against the line for that archive. Adding an archive
// to a release without re-signing is therefore detectable, which is the property that
// lets flynn install an extension binary it did not build.
func WriteChecksums(dist string, archives []Artifact) (Artifact, error) {
	lines := make([]string, 0, len(archives))
	for _, a := range archives {
		sum, err := sha256File(a.Path)
		if err != nil {
			return Artifact{}, err
		}
		// Two spaces, then the bare filename: the archives sit beside checksums.txt in
		// the release, so a path would not resolve for anyone who downloaded them.
		lines = append(lines, sum+"  "+a.Name)
	}
	sort.Strings(lines) // stable regardless of the order the matrix built in

	out := filepath.Join(dist, ChecksumsFile)
	body := strings.Join(lines, "\n") + "\n"
	// 0644: checksums.txt is a public release artifact, meant to be world-readable.
	if err := os.WriteFile(out, []byte(body), 0o644); err != nil { //nolint:gosec // G306: a published artifact is not a secret
		return Artifact{}, err
	}
	return Artifact{Name: ChecksumsFile, Path: out, Kind: KindChecksums}, nil
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // G304: path is an artifact this tool just wrote
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
