package download

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/modullar/blade-runner/internal/diag"
)

// ExtractTarGz unpacks a .tar.gz into destDir. It refuses entries that would land outside
// destDir (absolute paths, "..", symlinks that point out, writes through such symlinks),
// refuses hard links and device files, and strips setuid/setgid/sticky bits.
func ExtractTarGz(archive, destDir string) error {
	f, err := os.Open(archive)
	if err != nil {
		return extractErr(err, "cannot open the archive")
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return extractErr(err, "the archive is not a valid .tar.gz")
	}
	defer gz.Close()
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return extractErr(err, "cannot create "+destDir)
	}
	root, err := filepath.EvalSymlinks(destDir)
	if err != nil {
		return extractErr(err, "cannot resolve "+destDir)
	}

	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return extractErr(err, "the archive is corrupt")
		}
		name := path.Clean(h.Name)
		if name == "." {
			continue
		}
		if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return extractErr(fmt.Errorf("entry %q escapes the destination", h.Name), "the archive is unsafe")
		}
		target := filepath.Join(root, filepath.FromSlash(name))
		perm := os.FileMode(h.Mode) & 0o777

		switch h.Typeflag {
		case tar.TypeDir:
			if err := safeParent(root, target); err != nil {
				return err
			}
			if err := os.MkdirAll(target, perm|0o700); err != nil {
				return extractErr(err, "cannot create "+name)
			}
		case tar.TypeReg:
			if err := safeParent(root, target); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return extractErr(err, "cannot create the directory for "+name)
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
			if err != nil {
				return extractErr(err, "cannot create "+name)
			}
			_, copyErr := io.Copy(out, tr)
			closeErr := out.Close()
			if copyErr != nil || closeErr != nil {
				return extractErr(errors.Join(copyErr, closeErr), "cannot write "+name)
			}
			if err := os.Chmod(target, perm); err != nil { // umask must not strip the exec bit
				return extractErr(err, "cannot set the mode of "+name)
			}
		case tar.TypeSymlink:
			if err := safeParent(root, target); err != nil {
				return err
			}
			link := h.Linkname
			if path.IsAbs(link) {
				return extractErr(fmt.Errorf("symlink %q points outside the destination (%q)", h.Name, link), "the archive is unsafe")
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return extractErr(err, "cannot create the directory for "+name)
			}
			// Resolve against the real directory the link will live in: earlier symlinked
			// directories must not make a lexically harmless target point out.
			realDir, err := filepath.EvalSymlinks(filepath.Dir(target))
			if err != nil {
				return extractErr(err, "cannot resolve the directory for "+name)
			}
			resolved := filepath.Join(realDir, filepath.FromSlash(link))
			if resolved != root && !strings.HasPrefix(resolved, root+string(filepath.Separator)) {
				return extractErr(fmt.Errorf("symlink %q points outside the destination (%q)", h.Name, link), "the archive is unsafe")
			}
			if err := os.Symlink(link, target); err != nil {
				return extractErr(err, "cannot create symlink "+name)
			}
		default:
			return extractErr(fmt.Errorf("entry %q has unsupported type %q", h.Name, string(h.Typeflag)), "the archive contains hard links or devices")
		}
	}
}

// safeParent checks that target's existing parent directories, with symlinks resolved, are
// still inside root: an earlier symlink entry must not redirect a later write.
func safeParent(root, target string) error {
	dir := filepath.Dir(target)
	for {
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			if resolved != root && !strings.HasPrefix(resolved, root+string(filepath.Separator)) {
				return extractErr(fmt.Errorf("%q resolves outside the destination", dir), "the archive is unsafe")
			}
			return nil
		}
		next := filepath.Dir(dir)
		if next == dir {
			return nil
		}
		dir = next
	}
}

func extractErr(err error, what string) error {
	return diag.Wrap(err, diag.CodeDownloadFailed, what, "the download is damaged or the archive is not what GitHub published",
		"delete the runner download cache and re-run `bladerunner apply`; if it persists, report it")
}
