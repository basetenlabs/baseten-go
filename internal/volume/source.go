package volume

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// SourceFile is a regular file found by a scan, before its contents have been
// read or chunked.
type SourceFile struct {
	// Path is relative to the source root and slash-separated.
	Path string
	Mode uint16
	Size uint64

	// MTime is the file's modification time from the walk's lstat, clamped
	// to the wire-representable range.
	MTime time.Time
}

// Source is the result of scanning a directory to push: every entry the
// manifest will describe, with the file contents still on disk.
type Source struct {
	// Root is the directory that was scanned, as the caller named it.
	Root string

	Directories []DirectoryEntry
	Files       []SourceFile
	Symlinks    []SymlinkEntry

	// TotalBytes is the sum of file sizes, for progress reporting.
	TotalBytes uint64
}

// ScanSource walks root and collects the entries of a push. Symlinks are
// recorded but never followed, so the scan sees exactly the tree as it is
// rather than whatever it points at.
//
// Anything that is not a directory, a regular file, or a symlink — a socket,
// a device node, a named pipe — fails the scan. The format cannot describe
// them, and a push that silently dropped them would publish a tree that does
// not match the source.
//
// Refusing is the same choice made for a path that cannot round-trip — say what cannot be
// done rather than quietly do something else.
func ScanSource(root string) (*Source, error) {
	walkRoot, err := scanRoot(root)
	if err != nil {
		return nil, err
	}

	src := &Source{Root: root}
	// Directories are collected by path so that an ancestor synthesized for a
	// nested entry can be replaced by the real directory when the walk reaches
	// it. Being top-down, the walk always does — but the manifest must be
	// complete either way, since a missing directory record loses a mode.
	dirs := map[string]DirectoryEntry{}

	err = filepath.WalkDir(walkRoot, func(abs string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := relPath(walkRoot, abs)
		if err != nil || rel == "" {
			return err
		}
		if err := ValidatePath(rel); err != nil {
			return err
		}

		switch {
		case d.IsDir():
			info, err := entryInfo(abs)
			if err != nil {
				return err
			}
			dirs[rel] = DirectoryEntry{Path: rel, Mode: entryMode(info), MTime: clampMTime(info.ModTime())}
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(abs)
			if err != nil {
				return fmt.Errorf("read symlink %s: %w", abs, err)
			}
			if target, err = NormalizeSymlinkTarget(target); err != nil {
				return fmt.Errorf("symlink %s: %w", rel, err)
			}
			// The walk never follows links and neither does Lstat, so the
			// recorded time is the link's own, not the target's.
			info, err := entryInfo(abs)
			if err != nil {
				return err
			}
			addAncestors(dirs, rel)
			src.Symlinks = append(src.Symlinks, SymlinkEntry{Path: rel, Target: target, Mode: SymlinkMode, MTime: clampMTime(info.ModTime())})
		case d.Type().IsRegular():
			info, err := entryInfo(abs)
			if err != nil {
				return err
			}
			addAncestors(dirs, rel)
			size := uint64(info.Size())
			src.Files = append(src.Files, SourceFile{Path: rel, Mode: entryMode(info), Size: size, MTime: clampMTime(info.ModTime())})
			src.TotalBytes += size
		default:
			return fmt.Errorf("%s is a %s, which a volume cannot describe", rel, d.Type())
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan %s: %w", root, err)
	}

	for _, entry := range dirs {
		src.Directories = append(src.Directories, entry)
	}
	// These sorts make the scanner's output deterministic — the directory set
	// is collected in a map — and decide nothing about the wire: the manifest's
	// entry order is computed at encode time, over the complete set.
	slices.SortFunc(src.Directories, func(a, b DirectoryEntry) int { return strings.Compare(a.Path, b.Path) })
	slices.SortFunc(src.Files, func(a, b SourceFile) int { return strings.Compare(a.Path, b.Path) })
	slices.SortFunc(src.Symlinks, func(a, b SymlinkEntry) int { return strings.Compare(a.Path, b.Path) })

	if err := src.checkPaths(); err != nil {
		return nil, fmt.Errorf("scan %s: %w", root, err)
	}
	return src, nil
}

// scanRoot resolves the directory to walk. A source directory reached through
// a symlink is a normal arrangement, and filepath.WalkDir will not descend
// into one, so the link is resolved here rather than producing a push of
// nothing.
func scanRoot(root string) (string, error) {
	info, err := os.Stat(root)
	if err != nil {
		return "", fmt.Errorf("scan %s: %w", root, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("scan %s: not a directory", root)
	}
	link, err := os.Lstat(root)
	if err != nil {
		return "", fmt.Errorf("scan %s: %w", root, err)
	}
	if link.Mode()&fs.ModeSymlink == 0 {
		return root, nil
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("scan %s: %w", root, err)
	}
	return resolved, nil
}

// checkPaths rejects a tree whose entries collide with one another, across all
// three kinds at once: the format requires paths to be unique over the whole
// manifest, not within a kind.
//
// Case-equal paths are refused even though this machine's filesystem evidently
// holds both, because a push publishes for everyone: the same manifest pulled
// onto macOS or Windows would land one file on top of the other. The scan is
// the last place that ambiguity is still local.
func (s *Source) checkPaths() error {
	index := newPathIndex(false)
	for _, d := range s.Directories {
		if err := index.add(d.Path); err != nil {
			return err
		}
	}
	for _, f := range s.Files {
		if err := index.add(f.Path); err != nil {
			return err
		}
	}
	for _, l := range s.Symlinks {
		if err := index.add(l.Path); err != nil {
			return err
		}
	}
	return nil
}

// relPath returns abs as a slash-separated path relative to root. On Windows
// this is where the separator changes; on unix it is a no-op, which is what
// leaves a backslash in a file's name intact for ValidatePath to reject.
func relPath(root, abs string) (string, error) {
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "", err
	}
	if rel == "." {
		return "", nil
	}
	return filepath.ToSlash(rel), nil
}

// entryInfo reads an entry's metadata with a fresh Lstat rather than through
// the walk's own DirEntry.Info. On unix the two are the same call FOR THIS
// WALK — Info is a lazy lstat only while the directory was opened outside an
// os.Root and the dirent's type was known; Go fills it eagerly otherwise, at
// enumeration time, in its own words because "we cannot use a lazy lstat".
// This scan is a plain WalkDir with known types, so the lazy path holds
// here, and the condition needs restating if the scan ever moves under
// os.Root — the pull side already lives there. On Windows, by contrast,
// Info returns the directory enumeration's
// cached copy of the entry's metadata, and Windows documents that copy as
// lazily updated: two scans of an untouched tree can read two different
// modification times from it as the cache settles, and a time-bearing
// manifest digest changes with them. The direct query reads the entry's own
// live record on every platform, which is what makes an unchanged tree scan
// to unchanged bytes.
func entryInfo(abs string) (fs.FileInfo, error) {
	return os.Lstat(abs)
}

// entryMode reads the permission bits a manifest records: the low twelve, so
// setuid, setgid, and sticky survive a round trip.
func entryMode(info fs.FileInfo) uint16 {
	return uint16(info.Mode().Perm()) | uint16(specialBits(info.Mode()))
}

// specialBits maps Go's portable setuid, setgid, and sticky flags back onto
// their unix mode bits, which is where the format keeps them.
func specialBits(mode fs.FileMode) uint16 {
	var bits uint16
	if mode&fs.ModeSetuid != 0 {
		bits |= 0o4000
	}
	if mode&fs.ModeSetgid != 0 {
		bits |= 0o2000
	}
	if mode&fs.ModeSticky != 0 {
		bits |= 0o1000
	}
	return bits
}

// addAncestors seeds every directory above path with the default mode and no
// modification time, unless the walk has already recorded the directory
// itself. Like the map's replacement rule above, the synthesized entry is a
// guard rather than a live path — the walk is top-down, so it reaches every
// real directory and records it before anything beneath it needs an ancestor
// — and the zero time means the encoder would omit the key rather than
// assert a time nothing measured.
func addAncestors(dirs map[string]DirectoryEntry, path string) {
	for _, parent := range parentPaths(path) {
		if _, ok := dirs[parent]; !ok {
			dirs[parent] = DirectoryEntry{Path: parent, Mode: DefaultDirMode}
		}
	}
}

// NewManifest assembles the manifest for a push from the scanned tree and the
// file entries that uploading each file produced. The directory and symlink
// records come straight from the scan; only the files needed the network.
func NewManifest(src *Source, sourceURI string, files []FileEntry) *Manifest {
	return &Manifest{
		Provenance: Provenance{
			SourceFingerprint:     ProvenanceFingerprint,
			SourceFingerprintType: ProvenanceFingerprintType,
			SourceURI:             sourceURI,
		},
		// The manifest owns its slices: every one is copied, so nothing the
		// caller does to its own afterwards can change what gets encoded, and
		// nothing done here reaches back into the caller's.
		Directories: slices.Clone(src.Directories),
		Files:       slices.Clone(files),
		Symlinks:    slices.Clone(src.Symlinks),
	}
}

// SourceURIForDir builds the provenance URI a push records for a local source
// directory. It is inside the bytes the manifest digest covers, so two pushes
// of identical trees from different paths are different versions.
func SourceURIForDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	return fileURI(filepath.ToSlash(abs)), nil
}

// fileURI builds the file URI for an absolute path that has already been
// converted to forward slashes.
//
// The leading slash is what separates the empty authority from the path. A
// unix absolute path already starts with one, so this is a no-op there and
// the bytes are unchanged — which matters because the URI is inside the
// digest, and changing it for existing sources would invalidate every
// manifest they produced. A windows path starts with its drive letter
// instead, and without the added slash the drive would be read as the
// authority: file://C:/data names a host, file:///C:/data names a path.
func fileURI(slashed string) string {
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed
	}
	return "file://" + slashed
}
