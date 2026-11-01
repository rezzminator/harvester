package harvestpy

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	pathpkg "path"
	"path/filepath"
	"strings"
)

func extractNamedBinary(path, name, destination string) (returnErr error) {
	input, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() {
		if err := input.Close(); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close binary archive %s: %w", path, err))
		}
	}()
	gzipReader, err := gzip.NewReader(input)
	if err != nil {
		return err
	}
	defer func() {
		if err := gzipReader.Close(); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close binary gzip stream %s: %w", path, err))
		}
	}()
	archive := tar.NewReader(gzipReader)
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if filepath.Base(header.Name) != name || !header.FileInfo().Mode().IsRegular() {
			continue
		}
		root, err := os.OpenRoot(filepath.Dir(destination))
		if err != nil {
			return fmt.Errorf("open binary destination: %w", err)
		}
		defer func() {
			if err := root.Close(); err != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("close binary destination: %w", err))
			}
		}()
		base := filepath.Base(destination)
		if err := copyLimited(root, base, archive, header.Size); err != nil {
			return err
		}
		return root.Chmod(base, 0o700)
	}
	return fmt.Errorf("binary %q not found in archive", name)
}

// extractPython unpacks the Python archive under destination. Every write
// goes through an os.Root, so no entry — nor a symlink an earlier entry made —
// reaches outside destination.
func extractPython(path, destination string) (pythonPath string, returnErr error) {
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return "", fmt.Errorf("create Python extraction root: %w", err)
	}
	root, err := os.OpenRoot(destination)
	if err != nil {
		return "", fmt.Errorf("open Python extraction root: %w", err)
	}
	defer func() {
		if err := root.Close(); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close Python extraction root %s: %w", destination, err))
		}
	}()
	input, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() {
		if err := input.Close(); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close Python archive %s: %w", path, err))
		}
	}()
	gzipReader, err := gzip.NewReader(input)
	if err != nil {
		return "", err
	}
	defer func() {
		if err := gzipReader.Close(); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close Python gzip stream %s: %w", path, err))
		}
	}()
	archive := tar.NewReader(gzipReader)
	var total int64
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		name, err := safeArchiveName(header.Name)
		if err != nil {
			return "", err
		}
		switch header.Typeflag {
		case tar.TypeXGlobalHeader, tar.TypeXHeader, tar.TypeGNULongName, tar.TypeGNULongLink:
			continue
		case tar.TypeDir:
			if err := root.MkdirAll(name, header.FileInfo().Mode().Perm()); err != nil {
				return "", fmt.Errorf("create Python directory %s: %w", name, err)
			}
			continue
		case tar.TypeReg, tar.TypeSymlink, tar.TypeLink:
			if err := root.MkdirAll(pathpkg.Dir(name), 0o700); err != nil {
				return "", fmt.Errorf("create Python parent directory %s: %w", name, err)
			}
		default:
			return "", fmt.Errorf("unsupported Python archive entry %s (type %d)", name, header.Typeflag)
		}
		switch header.Typeflag {
		case tar.TypeReg:
			if header.Size < 0 || header.Size > 512<<20 || total > 2<<30-header.Size {
				return "", fmt.Errorf("python archive is too large at %d bytes", total+header.Size)
			}
			if err := copyLimited(root, name, archive, header.Size); err != nil {
				return "", fmt.Errorf("extract Python file %s: %w", name, err)
			}
			total += header.Size
			if err := root.Chmod(name, header.FileInfo().Mode().Perm()); err != nil {
				return "", fmt.Errorf("preserve Python file mode %s: %w", name, err)
			}
		case tar.TypeSymlink:
			target, err := safeSymlinkTarget(name, header.Linkname)
			if err != nil {
				return "", fmt.Errorf("unsafe Python symlink %s: %w", name, err)
			}
			if err := symlinkFreeParent(root, name); err != nil {
				return "", fmt.Errorf("unsafe Python symlink %s: %w", name, err)
			}
			if err := root.Symlink(target, name); err != nil {
				return "", fmt.Errorf("extract Python symlink %s: %w", name, err)
			}
		case tar.TypeLink:
			target, err := safeArchiveName(header.Linkname)
			if err != nil {
				return "", fmt.Errorf("unsafe Python hardlink %s: %w", name, err)
			}
			if err := root.Link(target, name); err != nil {
				return "", fmt.Errorf("extract Python hardlink %s: %w", name, err)
			}
		}
	}
	for _, relative := range []string{"python/bin/python3", "python/bin/python"} {
		if info, err := root.Stat(relative); err == nil && info.Mode().IsRegular() {
			return filepath.Join(destination, filepath.FromSlash(relative)), nil
		}
	}
	return "", errors.New("python executable not found in extracted archive")
}

func safeArchiveName(name string) (string, error) {
	name = filepath.ToSlash(name)
	if name == "" || strings.HasPrefix(name, "/") || strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("unsafe archive path %q", name)
	}
	clean := filepath.ToSlash(filepath.Clean(name))
	if clean == "." || clean != strings.TrimSuffix(name, "/") || strings.HasPrefix(clean, "../") ||
		strings.Contains(clean, "/../") {
		return "", fmt.Errorf("unsafe archive path %q", name)
	}
	return strings.TrimSuffix(clean, "/"), nil
}

func safeSymlinkTarget(entry, linkname string) (string, error) {
	linkname = filepath.ToSlash(linkname)
	if linkname == "" || strings.HasPrefix(linkname, "/") || strings.ContainsRune(linkname, 0) {
		return "", fmt.Errorf("unsafe symlink target %q", linkname)
	}
	resolved := filepath.ToSlash(filepath.Clean(filepath.Join(filepath.Dir(entry), linkname)))
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		return "", fmt.Errorf("symlink target escapes archive root: %q", linkname)
	}
	return filepath.ToSlash(filepath.Clean(linkname)), nil
}

// symlinkFreeParent refuses a symlink entry under a symlinked directory:
// safeSymlinkTarget judges the target against the entry's path as written,
// which holds only when no parent redirects it. os.Root contains every write
// but not where a new symlink points.
func symlinkFreeParent(root *os.Root, name string) error {
	for dir := pathpkg.Dir(name); dir != "."; dir = pathpkg.Dir(dir) {
		info, err := root.Lstat(dir)
		if err != nil {
			return fmt.Errorf("inspect archive parent %s: %w", dir, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("archive parent is a symlink: %s", dir)
		}
	}
	return nil
}

// copyLimited writes exactly size bytes of source to name, a new file under
// root; a short or failed copy leaves no file behind.
func copyLimited(root *os.Root, name string, source io.Reader, size int64) error {
	if size < 0 || size > 512<<20 {
		return fmt.Errorf("archive file size %d exceeds limit", size)
	}
	output, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		return err
	}
	written, err := io.Copy(output, io.LimitReader(source, size))
	if err == nil && written != size {
		err = fmt.Errorf("archive entry ended at %d bytes, want %d", written, size)
	}
	if err != nil {
		_ = output.Close()
		_ = root.Remove(name)
		return err
	}
	return output.Close()
}
