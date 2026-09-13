package localmodel

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// maxMemberBytes bounds one extracted archive member. The largest thing in a
// llama.cpp release is the server implementation library at roughly 9 MB, so
// 256 MiB is far above anything legitimate while still refusing an archive
// that would fill the disk. The shift form is a power-of-two byte count:
// 256 << 20 is 256 multiplied by 2^20.
const maxMemberBytes = 256 << 20

// extractServer installs llama-server and the shared libraries shipped beside
// it from a release archive into destDir. Every other executable in the
// archive is skipped: a release carries a dozen CLI tools CairnOS never runs,
// and leaving them out keeps the install to what the sidecar actually starts.
// limit is the per-member byte ceiling, maxMemberBytes in every command path
// and a small figure in the test that proves an oversized member is refused.
func extractServer(archivePath, destDir string, build *Build, limit int64) error {
	if err := os.MkdirAll(destDir, dirPerm); err != nil {
		return fmt.Errorf("create %s: %w", destDir, err)
	}

	var (
		installed int
		err       error
	)

	if build.Format == formatZip {
		installed, err = extractZip(archivePath, destDir, build, limit)
	} else {
		installed, err = extractTarGz(archivePath, destDir, build, limit)
	}

	if err != nil {
		return err
	}

	if installed == 0 {
		return fmt.Errorf("%w: %s", ErrNoServerInArchive, build.Asset)
	}

	return nil
}

// extractTarGz walks a gzipped tarball, writing the members the server needs.
// It returns how many times the server binary itself was seen, so the caller
// can tell an archive that carried it from one that did not.
func extractTarGz(archivePath, destDir string, build *Build, limit int64) (int, error) {
	// #nosec G304 -- archivePath is the managed download location
	file, err := os.Open(archivePath)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", archivePath, err)
	}
	defer func() { _ = file.Close() }()

	zipped, err := gzip.NewReader(file)
	if err != nil {
		return 0, fmt.Errorf("read %s as gzip: %w", archivePath, err)
	}
	defer func() { _ = zipped.Close() }()

	return walkTar(tar.NewReader(zipped), destDir, build, limit)
}

// walkTar applies one tar stream's wanted members to destDir.
func walkTar(reader *tar.Reader, destDir string, build *Build, limit int64) (int, error) {
	var servers int

	for {
		header, err := reader.Next()
		if err == io.EOF {
			return servers, nil
		}

		if err != nil {
			return 0, fmt.Errorf("read tar entry: %w", err)
		}

		name, want := memberName(header.Name, build)
		if !want {
			continue
		}

		if err := writeTarMember(reader, header, destDir, name, limit); err != nil {
			return 0, err
		}

		if name == build.Binary {
			servers++
		}
	}
}

// writeTarMember installs one wanted tar entry: a symlink is recreated as a
// symlink so the library soname chain survives, and a regular file is copied
// with the executable bit the loader and the supervisor both need.
func writeTarMember(reader io.Reader, header *tar.Header, destDir, name string, limit int64) error {
	target := filepath.Join(destDir, name)

	if header.Typeflag == tar.TypeSymlink {
		return writeSymlink(header.Linkname, target)
	}

	if header.Typeflag != tar.TypeReg {
		return nil
	}

	return copyMember(reader, target, limit)
}

// writeSymlink recreates a library symlink. A link target naming anything but
// a sibling file is refused: the archives only ever point one soname at
// another in the same directory, and honoring a path would let an archive
// write outside the runtime directory.
func writeSymlink(linkname, target string) error {
	if linkname == "" || strings.ContainsAny(linkname, `/\`) {
		return fmt.Errorf("%w: link target %q is not a sibling file", ErrArchiveMember, linkname)
	}

	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("replace %s: %w", target, err)
	}

	if err := os.Symlink(linkname, target); err != nil {
		return fmt.Errorf("link %s: %w", target, err)
	}

	return nil
}

// extractZip walks a zip archive, writing the members the server needs.
func extractZip(archivePath, destDir string, build *Build, limit int64) (int, error) {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return 0, fmt.Errorf("open %s as zip: %w", archivePath, err)
	}
	defer func() { _ = reader.Close() }()

	var servers int

	for _, entry := range reader.File {
		name, want := memberName(entry.Name, build)

		// A zip records a symlink as a regular-looking entry whose body is the
		// link text, so copying it would install a file full of a path with the
		// execute bit set. The tar path already refuses everything but a file
		// and a symlink, and this is the same refusal for the same reason.
		if !want || !entry.FileInfo().Mode().IsRegular() {
			continue
		}

		if err := copyZipMember(entry, filepath.Join(destDir, name), limit); err != nil {
			return 0, err
		}

		if name == build.Binary {
			servers++
		}
	}

	return servers, nil
}

// copyZipMember writes one zip entry to target.
func copyZipMember(entry *zip.File, target string, limit int64) error {
	source, err := entry.Open()
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrArchiveMember, entry.Name, err)
	}
	defer func() { _ = source.Close() }()

	return copyMember(source, target, limit)
}

// copyMember writes one archive member to target with the executable bit set.
// Everything installed here is either the server or a library the loader opens,
// and both need the bit.
//
// A member over the limit is an error, not a truncation. Copying the first
// limit bytes and reporting success would install a cut-off shared library
// with the execute bit on it, and the failure would surface later as the
// loader refusing a corrupt object rather than here where the cause is plain.
func copyMember(source io.Reader, target string, limit int64) error {
	// #nosec G304 -- target is destDir joined with a separator-free name that
	// memberName vetted, so an archive entry cannot steer it outside the runtime dir.
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, execPerm)
	if err != nil {
		return fmt.Errorf("create %s: %w", target, err)
	}
	defer func() { _ = file.Close() }()

	// The reader is capped one byte past the limit so a member that is exactly
	// at it still installs while anything larger announces itself in the count.
	written, err := io.Copy(file, io.LimitReader(source, limit+1))
	if err != nil {
		return fmt.Errorf("write %s: %w", target, err)
	}

	if written > limit {
		return fmt.Errorf("%w: %s is larger than the %d byte member ceiling", ErrArchiveMember, target, limit)
	}

	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", target, err)
	}

	return nil
}

// memberName decides whether one archive entry belongs in the install and
// returns the name to write it under.
//
// An entry must sit directly under the archive's wrapper directory, which is
// Build.Prefix for the POSIX tarballs and empty for the flat Windows zip.
// Anything nested deeper, anywhere else, or reached through a parent traversal
// is refused rather than flattened: a name that survives here carries no path
// separator at all, so joining it with the destination cannot land outside.
func memberName(entryName string, build *Build) (string, bool) {
	name, inside := strings.CutPrefix(path.Clean(entryName), build.Prefix)
	if !inside || name == "" || name == "." || name == ".." || strings.Contains(name, "/") {
		return "", false
	}

	if name == build.Binary || isSharedLib(name) {
		return name, true
	}

	return "", false
}

// isSharedLib reports whether a file name is a shared library. Versioned
// sonames put the version after the extension ("libllama.so.0.4.0") on Linux
// and before it ("libggml.0.23.0.dylib") on macOS, so the check looks for the
// extension anywhere rather than only at the end.
func isSharedLib(name string) bool {
	// The extensions cover both the libraries llama-server links against and
	// the CPU backends ggml opens at runtime, which no linker tool reports
	// because nothing links them.
	for _, suffix := range []string{".dylib", ".dll", ".so"} {
		if strings.HasSuffix(name, suffix) || strings.Contains(name, suffix+".") {
			return true
		}
	}

	return false
}
