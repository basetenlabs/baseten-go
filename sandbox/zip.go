package sandbox

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// zipEntry is one file or directory of a build context.
type zipEntry struct {
	path string
	// data is nil for a directory.
	data []byte
	mode fs.FileMode
}

// writeZip zips entries, in order, into one archive.
func writeZip(entries []zipEntry) ([]byte, error) {
	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.path, Method: zip.Deflate}
		if entry.data == nil {
			header.Name += "/"
			header.Method = zip.Store
			header.SetMode(entry.mode | fs.ModeDir)
		} else {
			header.SetMode(entry.mode)
		}
		w, err := writer.CreateHeader(header)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(entry.data); err != nil {
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
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
		data := files[path]
		if data == nil {
			// A nil file is still a file, just an empty one.
			data = []byte{}
		}
		entries = append(entries, zipEntry{path: path, data: data, mode: 0o644})
	}
	return entries, nil
}

// directoryZipEntries builds entries from everything under a local
// directory. Files keep their modes, a link to a file within the directory
// stores a copy of the file, a link to a file outside it is an error, a link
// to a directory stores an empty directory, and a broken link is left out.
func directoryZipEntries(root string) ([]zipEntry, error) {
	// Checked before reading anything, so a mistaken directory, such as a
	// home directory, fails fast instead of being read in full.
	if info, err := os.Stat(filepath.Join(root, "Dockerfile")); err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("directory %s has no Dockerfile at its root", root)
	}
	var entries []zipEntry
	if err := addDirectoryZipEntries(root, "", &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// addDirectoryZipEntries adds everything under dir to entries, each path
// prefixed with prefix. A link to a file outside dir is an error.
func addDirectoryZipEntries(dir, prefix string, entries *[]zipEntry) error {
	// Resolved, so a link's resolved target can be checked against it.
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	return addTreeZipEntries(root, dir, prefix, entries)
}

func addTreeZipEntries(root, dir, prefix string, entries *[]zipEntry) error {
	// Sorted by name, so the same directory always zips the same way.
	children, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, child := range children {
		fullPath := filepath.Join(dir, child.Name())
		archivePath := prefix + child.Name()
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
			*entries = append(*entries, zipEntry{path: archivePath, mode: info.Mode().Perm()})
		case info.IsDir():
			*entries = append(*entries, zipEntry{path: archivePath, mode: info.Mode().Perm()})
			if err := addTreeZipEntries(root, fullPath, archivePath+"/", entries); err != nil {
				return err
			}
		case info.Mode().IsRegular():
			// A link to a file is stored as a copy of the file, but only
			// one within the directory, so a link cannot pull in a file
			// from elsewhere on the machine.
			if child.Type()&fs.ModeSymlink != 0 {
				target, err := filepath.EvalSymlinks(fullPath)
				if err != nil {
					return err
				}
				if rel, err := filepath.Rel(root, target); err != nil || !filepath.IsLocal(rel) {
					return fmt.Errorf("%s is a link to %s, outside %s; only links to files within it can be pushed", fullPath, target, root)
				}
			}
			data, err := os.ReadFile(fullPath)
			if err != nil {
				return err
			}
			*entries = append(*entries, zipEntry{path: archivePath, data: data, mode: info.Mode().Perm()})
		}
		// Anything else, such as a socket, has no content to archive.
	}
	return nil
}
