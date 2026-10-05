package sandbox

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// zipEntry is one file or directory of a build context.
type zipEntry struct {
	path string
	dir  bool
	// data is a file's content, unless sourcePath is set.
	data []byte
	// sourcePath is a local file read while zipping, so a large build context
	// is never held in memory whole.
	sourcePath string
	mode       fs.FileMode
}

// writeZip zips entries, in order, into w.
func writeZip(ctx context.Context, w io.Writer, entries []zipEntry) error {
	writer := zip.NewWriter(w)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		header := &zip.FileHeader{Name: entry.path, Method: zip.Deflate}
		if entry.dir {
			header.Name += "/"
			header.Method = zip.Store
			header.SetMode(entry.mode | fs.ModeDir)
		} else {
			header.SetMode(entry.mode)
		}
		fileWriter, err := writer.CreateHeader(header)
		if err != nil {
			return err
		}
		if entry.sourcePath != "" {
			if err := copyFileTo(fileWriter, entry.sourcePath); err != nil {
				return err
			}
		} else if _, err := fileWriter.Write(entry.data); err != nil {
			return err
		}
	}
	return writer.Close()
}

func copyFileTo(w io.Writer, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = io.Copy(w, file)
	return err
}

// checkZipDockerfile checks that a caller's archive has a Dockerfile at its
// root.
func checkZipDockerfile(archive []byte) error {
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return fmt.Errorf("the zip source is not a readable zip: %w", err)
	}
	for _, file := range reader.File {
		// A link named Dockerfile would only fail later, in the build.
		if file.Name == "Dockerfile" && file.Mode().IsRegular() {
			return nil
		}
	}
	return errors.New("the zip source has no Dockerfile file at its root")
}

// filesZipEntries builds entries from in-memory files, each with mode 0644.
func filesZipEntries(files map[string][]byte) ([]zipEntry, error) {
	if _, ok := files["Dockerfile"]; !ok {
		return nil, errors.New("the files source has no Dockerfile")
	}
	paths := make([]string, 0, len(files))
	for path := range files {
		segments := strings.Split(path, "/")
		if strings.Contains(path, `\`) || slices.ContainsFunc(segments, func(s string) bool { return s == "" || s == "." || s == ".." }) {
			return nil, fmt.Errorf("files path %q must be relative, with forward slashes and no empty, . or .. segments", path)
		}
		paths = append(paths, path)
	}
	// Sorted, so the same files always zip the same way.
	slices.Sort(paths)
	entries := make([]zipEntry, 0, len(paths))
	for _, path := range paths {
		entries = append(entries, zipEntry{path: path, data: files[path], mode: 0o644})
	}
	return entries, nil
}

// directoryZipEntries builds entries from everything under a local directory
// that ignore does not exclude. Files keep their modes, a link to a file
// within the directory stores a copy of the file, a link to a file outside it
// is an error, a link to a directory stores an empty directory, and a broken
// link is left out. The root Dockerfile and .dockerignore are always kept, as
// Docker keeps them in a build context.
func directoryZipEntries(ctx context.Context, root string, ignore ImageIgnoreFileFunc) ([]zipEntry, error) {
	// Checked before reading anything, so a mistaken directory, such as a
	// home directory, fails fast instead of being read in full.
	if info, err := os.Stat(filepath.Join(root, "Dockerfile")); err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("directory %s has no Dockerfile at its root", root)
	}
	var entries []zipEntry
	if err := addDirectoryZipEntries(ctx, root, "", ignore, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// addDirectoryZipEntries adds everything under dir that ignore does not
// exclude to entries, each path prefixed with prefix. A nil ignore keeps
// everything. A link to a file outside dir is an error.
func addDirectoryZipEntries(ctx context.Context, dir, prefix string, ignore ImageIgnoreFileFunc, entries *[]zipEntry) error {
	// Resolved, so a link's resolved target can be checked against it.
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	walk := treeWalk{ctx: ctx, root: root, prefix: prefix, ignore: ignore, entries: entries}
	return walk.add(dir, "")
}

// treeWalk adds one local directory tree to a build context's entries.
type treeWalk struct {
	ctx     context.Context
	root    string
	prefix  string
	ignore  ImageIgnoreFileFunc
	entries *[]zipEntry
}

// add adds the children of dir, whose path relative to the walked directory
// is relDir ("" for the walked directory itself).
func (w *treeWalk) add(dir, relDir string) error {
	// Sorted by name, so the same directory always zips the same way.
	children, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, child := range children {
		fullPath := filepath.Join(dir, child.Name())
		relPath := relDir + child.Name()
		if w.ignore != nil && !(relDir == "" && (child.Name() == "Dockerfile" || child.Name() == dockerignoreFileName)) {
			ignored, err := w.ignore(w.ctx, ImageIgnoreFileOptions{RelPath: relPath, Entry: child})
			if err != nil {
				return err
			}
			if ignored {
				continue
			}
		}
		archivePath := w.prefix + relPath
		info, err := os.Stat(fullPath)
		if err != nil {
			if child.Type()&fs.ModeSymlink != 0 && errors.Is(err, fs.ErrNotExist) {
				// A broken link has nothing to archive.
				continue
			}
			return err
		}
		switch {
		case info.IsDir() && child.Type()&fs.ModeSymlink != 0:
			// A link to a directory is stored as an empty directory, not
			// followed, so a link cannot pull in the rest of the machine.
			*w.entries = append(*w.entries, zipEntry{path: archivePath, dir: true, mode: info.Mode().Perm()})
		case info.IsDir():
			*w.entries = append(*w.entries, zipEntry{path: archivePath, dir: true, mode: info.Mode().Perm()})
			if err := w.add(fullPath, relPath+"/"); err != nil {
				return err
			}
		case info.Mode().IsRegular():
			// A link to a file is stored as a copy of the file, but only one
			// within the directory, so a link cannot pull in a file from
			// elsewhere on the machine.
			if child.Type()&fs.ModeSymlink != 0 {
				target, err := filepath.EvalSymlinks(fullPath)
				if err != nil {
					return err
				}
				if rel, err := filepath.Rel(w.root, target); err != nil || !filepath.IsLocal(rel) {
					return fmt.Errorf("%s is a link to %s, outside %s; only links to files within it can be pushed", fullPath, target, w.root)
				}
			}
			*w.entries = append(*w.entries, zipEntry{path: archivePath, sourcePath: fullPath, mode: info.Mode().Perm()})
		}
		// Anything else, such as a socket, has no content to archive.
	}
	return nil
}
