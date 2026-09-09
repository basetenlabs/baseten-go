package volume

import (
	"slices"
	"time"
)

// EntryKind says which of a manifest's three record types an entry came from.
type EntryKind uint8

const (
	EntryKindFile EntryKind = iota + 1
	EntryKindDirectory
	EntryKindSymlink
)

// Entry is one manifest entry of any kind, flattened to what describes it
// rather than to how its bytes are stored.
//
// A manifest decodes into three slices of three shapes, because the wire
// records differ. Anything walking a version's contents wants one ordered
// sequence instead, and wants nothing to do with chunks, chunkmaps, or storage
// targets, so this carries what a reader of the tree needs and the chunk
// machinery stays with FileEntry.
type Entry struct {
	Path string
	Kind EntryKind

	// Size is the file's length, and zero for the other kinds: a directory has
	// no size of its own, and a symlink's size would be its target's, which a
	// manifest does not record and a reader must not assume.
	Size uint64

	// Mode is the recorded permission bits, masked to ModeMask so the setuid,
	// setgid, and sticky bits survive. Zero when nothing records the entry,
	// which happens for a directory inferred from the paths beneath it.
	Mode uint16

	// MTime is zero when the manifest recorded none.
	MTime time.Time

	// Target is a symlink's target, stored verbatim as readlink reported it,
	// and empty for every other kind.
	Target string
}

// Entries flattens a manifest into one sequence in canonical path order, which
// is the order the wire carried and the order a tree is reproduced in: every
// entry follows its parent, and a subtree's entries are contiguous.
func (m *Manifest) Entries() []Entry {
	entries := make([]Entry, 0, m.EntryCount())
	for _, d := range m.Directories {
		entries = append(entries, DirectoryEntryOf(d))
	}
	for _, f := range m.Files {
		entries = append(entries, FileEntryOf(f))
	}
	for _, s := range m.Symlinks {
		entries = append(entries, SymlinkEntryOf(s))
	}
	SortEntries(entries)
	return entries
}

// The three flatteners, one per record type. A decode that filters entries as
// it reads them judges each one as an Entry before it has a manifest to
// flatten, so the conversion lives here rather than inline in Entries.

func DirectoryEntryOf(d DirectoryEntry) Entry {
	return Entry{Path: d.Path, Kind: EntryKindDirectory, Mode: d.Mode, MTime: d.MTime}
}

func FileEntryOf(f FileEntry) Entry {
	return Entry{Path: f.Path, Kind: EntryKindFile, Size: f.Size, Mode: f.Mode, MTime: f.MTime}
}

func SymlinkEntryOf(s SymlinkEntry) Entry {
	return Entry{Path: s.Path, Kind: EntryKindSymlink, Mode: s.Mode, MTime: s.MTime, Target: s.Target}
}

// SortEntries puts entries in canonical path order, the same comparison the
// encoder sorts by, so a decoded manifest flattens back into the order it
// arrived in.
func SortEntries(entries []Entry) {
	slices.SortStableFunc(entries, func(a, b Entry) int { return pathComponentCompare(a.Path, b.Path) })
}
