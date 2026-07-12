// Package release builds, signs and publishes the independently versioned components of
// a Go monorepo. See cmd/relkit for the tool that drives it.
package release

import (
	"archive/tar"
	"archive/zip"
	"compress/flate"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"sort"
	"time"
)

// archiveEntry is one file to pack.
type archiveEntry struct {
	Name string      // path inside the archive
	Path string      // source file on disk
	Mode os.FileMode // permission bits recorded in the archive
}

// writeArchive packs entries into a .tar.gz (or .zip, for Windows) at out.
//
// Every field that could vary between two builds of the same commit is pinned: entries
// are written in a fixed order, timestamps come from the commit rather than the clock,
// ownership is recorded as root:root with no names, and the gzip header carries no
// filename or timestamp. Byte-identical output is not a nicety here: the release signs a
// checksum file, so if the archive bytes drifted between two builds of the same commit,
// nobody could reproduce the release and check that the signature covers what the source
// actually produces.
func writeArchive(out string, entries []archiveEntry, modTime time.Time, useZip bool) error {
	sorted := append([]archiveEntry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	for i := 1; i < len(sorted); i++ {
		if sorted[i].Name == sorted[i-1].Name {
			return fmt.Errorf("duplicate entry %q", sorted[i].Name)
		}
	}

	// The path is a filename this tool composed from the manifest and the tag, not
	// user input reaching an arbitrary location.
	f, err := os.Create(out) //nolint:gosec // G304: out is a release artifact path we built
	if err != nil {
		return err
	}
	// Close is deferred for the error path; the success path closes explicitly and
	// checks the error, because a failed flush on close would silently truncate the
	// archive we are about to sign.
	defer func() { _ = f.Close() }()

	if useZip {
		err = writeZip(f, sorted, modTime.UTC())
	} else {
		err = writeTarGz(f, sorted, modTime.UTC())
	}
	if err != nil {
		return err
	}
	return f.Close()
}

func writeTarGz(w io.Writer, entries []archiveEntry, modTime time.Time) error {
	// Level and header are both fixed: gzip's default header would otherwise stamp the
	// current time into the file, and the compression level changes the bytes.
	gz, err := gzip.NewWriterLevel(w, gzip.BestCompression)
	if err != nil {
		return err
	}
	gz.Header = gzip.Header{OS: 255} // 255 = unknown; no Name, no Comment, zero ModTime

	tw := tar.NewWriter(gz)
	for _, e := range entries {
		data, err := os.ReadFile(e.Path)
		if err != nil {
			return err
		}
		hdr := &tar.Header{
			Name:     e.Name,
			Mode:     int64(e.Mode.Perm()),
			Size:     int64(len(data)),
			ModTime:  modTime,
			Typeflag: tar.TypeReg,
			Uid:      0,
			Gid:      0,
			Uname:    "",
			Gname:    "",
			// USTAR carries no sub-second times or extended attributes, so there is
			// nothing left in the header for the local environment to leak into.
			Format: tar.FormatUSTAR,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := tw.Write(data); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

func writeZip(w io.Writer, entries []archiveEntry, modTime time.Time) error {
	zw := zip.NewWriter(w)
	// zip's default deflate level tracks the compress/flate default; pin it so the
	// bytes cannot move if that default ever changes.
	zw.RegisterCompressor(zip.Deflate, func(out io.Writer) (io.WriteCloser, error) {
		return flate.NewWriter(out, flate.BestCompression)
	})
	for _, e := range entries {
		data, err := os.ReadFile(e.Path)
		if err != nil {
			return err
		}
		hdr := &zip.FileHeader{Name: e.Name, Method: zip.Deflate}
		hdr.SetMode(e.Mode.Perm())
		hdr.Modified = modTime
		fw, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		if _, err := fw.Write(data); err != nil {
			return err
		}
	}
	return zw.Close()
}
