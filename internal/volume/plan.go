package volume

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// ErrUnsupportedEntry reports a manifest entry this client cannot materialize.
var ErrUnsupportedEntry = errors.New("unsupported manifest entry")

// SelectEntries narrows a manifest to the entries named by include, plus the
// directories needed to hold them.
//
// Each include is either an exact entry path or a directory whose contents are
// wanted, matched on slash boundaries so "model" never selects "models". An
// include that matches nothing is an error rather than an empty result: a
// caller who misspelled a path wants to hear about it, not to be told the pull
// succeeded.
//
// Matching is on recorded paths alone. A symlink inside a selected directory
// is copied as a link, and whatever it points at is selected only if its own
// path was.
//
// An empty include returns the manifest unchanged.
func SelectEntries(m *Manifest, include []string) (*Manifest, error) {
	if len(include) == 0 {
		return m, nil
	}
	prefixes, err := normalizeInclude(include)
	if err != nil {
		return nil, err
	}
	matched := make([]bool, len(prefixes))

	// A directory is kept when it is selected outright, and also when
	// something below it is: a file cannot be written into a directory that
	// was left out of the plan.
	needed := map[string]bool{}
	selected := &Manifest{Provenance: m.Provenance}

	for _, file := range m.Files {
		if markMatches(prefixes, matched, file.Path) {
			selected.Files = append(selected.Files, file)
			markAncestors(needed, file.Path)
		}
	}
	for _, link := range m.Symlinks {
		if markMatches(prefixes, matched, link.Path) {
			selected.Symlinks = append(selected.Symlinks, link)
			markAncestors(needed, link.Path)
		}
	}
	for _, dir := range m.Directories {
		if markMatches(prefixes, matched, dir.Path) {
			needed[dir.Path] = true
			// A selected directory needs its ancestors as much as a selected
			// file does. Without this, selecting an empty directory leaves its
			// parent out of the plan, so nothing records the parent's mode and
			// it is created with whatever the umask allows.
			markAncestors(needed, dir.Path)
		}
	}

	// Carry over the recorded mode of every needed directory. One the
	// manifest does not describe takes the default, which is the same
	// treatment a push gives a directory it had to synthesize.
	described := map[string]DirectoryEntry{}
	for _, dir := range m.Directories {
		described[dir.Path] = dir
	}
	for path := range needed {
		if dir, ok := described[path]; ok {
			selected.Directories = append(selected.Directories, dir)
		} else {
			selected.Directories = append(selected.Directories, DirectoryEntry{Path: path, Mode: DefaultDirMode})
		}
	}
	slices.SortFunc(selected.Directories, func(a, b DirectoryEntry) int { return strings.Compare(a.Path, b.Path) })

	for i, ok := range matched {
		if !ok {
			return nil, fmt.Errorf("nothing in the volume matches %q", include[i])
		}
	}
	return selected, nil
}

// normalizeInclude cleans and deduplicates the selectors.
func normalizeInclude(include []string) ([]string, error) {
	prefixes := make([]string, 0, len(include))
	for _, raw := range include {
		path := strings.Trim(strings.TrimSpace(raw), "/")
		if path == "" {
			// The whole volume, which is what an empty include already means.
			return nil, fmt.Errorf("include %q selects everything; omit it instead", raw)
		}
		if err := ValidatePath(path); err != nil {
			return nil, fmt.Errorf("include %q: %w", raw, err)
		}
		prefixes = append(prefixes, path)
	}
	return prefixes, nil
}

// markMatches records every selector covering path and reports whether any
// did. Every one is marked, not just the first: overlapping selectors are a
// normal thing to write — a directory and a file inside it — and reporting the
// inner one as matching nothing would be wrong.
func markMatches(prefixes []string, matched []bool, path string) bool {
	hit := false
	for i, prefix := range prefixes {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			matched[i] = true
			hit = true
		}
	}
	return hit
}

// markAncestors records every directory above path as needed.
func markAncestors(needed map[string]bool, path string) {
	for _, parent := range parentPaths(path) {
		needed[parent] = true
	}
}

// CheckPlan reports whether a manifest can be written to a local filesystem
// exactly as recorded. It runs before anything is created, so a tree that
// cannot be reproduced faithfully fails with nothing written rather than
// halfway through.
//
// caseSensitive says whether the destination filesystem distinguishes paths
// that differ only in case. When it does not, such a pair is refused: both
// entries would resolve to one file, and the download would report success
// having written one of them over the other. On a filesystem that does
// distinguish them the pair is perfectly materializable, and a tree pushed
// from Linux is entitled to come back on Linux.
func CheckPlan(m *Manifest, caseSensitive bool) error {
	index := newPathIndex(caseSensitive)
	for _, dir := range m.Directories {
		if err := checkEntry(index, dir.Path); err != nil {
			return err
		}
	}
	for _, file := range m.Files {
		if err := checkEntry(index, file.Path); err != nil {
			return err
		}
		switch file.Kind {
		case FileKindChunk, FileKindChunkmap:
		case FileKindSlabmap:
			// A slabmap shares one stored chunk between several small files.
			// Reading one means slicing a range out of that chunk and checking
			// it against the file's own digest, which this client does not do.
			// Writing a file of the wrong bytes would be far worse than
			// refusing.
			return fmt.Errorf("%w: %s is packed into a slabmap, which this client cannot read", ErrUnsupportedEntry, file.Path)
		default:
			return fmt.Errorf("%w: %s has kind %q", ErrUnsupportedEntry, file.Path, file.Kind)
		}
	}
	for _, link := range m.Symlinks {
		if err := checkEntry(index, link.Path); err != nil {
			return err
		}
		if link.Target == "" {
			return fmt.Errorf("symlink %s has no target", link.Path)
		}
	}
	return nil
}

func checkEntry(index *pathIndex, path string) error {
	if err := ValidatePath(path); err != nil {
		return err
	}
	return index.add(path)
}
