package client

import (
	"io"
	"time"
)

// VolumeEntryKind says what an entry is.
type VolumeEntryKind string

const (
	VolumeEntryKindFile      VolumeEntryKind = "file"
	VolumeEntryKindDirectory VolumeEntryKind = "directory"
	VolumeEntryKindSymlink   VolumeEntryKind = "symlink"
)

// VolumeEntry describes one entry of a version of a volume: what it is and
// what it looks like, not how its bytes are stored.
//
// There is deliberately no content digest. A version's own digest names the
// whole tree, but a per-file one is not something every entry has: a file
// small enough to fit one chunk carries that chunk's digest, which is its
// content, while a larger file carries the digest of the document listing its
// chunks, which is not. A field meaning two different things and absent for
// the common case would be worse than none.
type VolumeEntry struct {
	// Path is the entry's full path within the version, slash-prefixed and
	// with no trailing slash, spelled exactly as [VolumeRef.Path] spells one.
	// So building a ref for an entry is an assignment and needs no separator
	// of its own:
	//
	//	ref := manifest.Ref
	//	ref.Path = entry.Path
	//	fmt.Println(ref) // bdn:weights/llama@a1b2c3/config/model.json
	//
	// A directory carries no trailing slash either; Kind is what says it is
	// one, and a trailing slash would not survive the round trip through a ref.
	Path string

	Kind VolumeEntryKind

	// Size is a file's length in bytes, and zero for the other kinds: a
	// directory has no size of its own, and a symlink's would be its target's,
	// which a volume does not record.
	Size int64

	// Mode is the recorded permission bits, including setuid, setgid, and
	// sticky, which a container root filesystem legitimately carries. Zero
	// when nothing in the volume describes the entry, which happens for a
	// directory implied only by the paths beneath it.
	Mode uint32

	// ModTime is the modification time recorded when the version was
	// published, and zero when none was recorded.
	ModTime time.Time

	// LinkTarget is a symlink's target exactly as it was recorded, and empty
	// for every other kind. It may point outside the volume, so a caller
	// reproducing it somewhere is responsible for whatever that means there.
	LinkTarget string
}

// VolumePulledEntry is one entry delivered to a
// [PullVolumeOptions.EntryHandler], together with the means to read it.
type VolumePulledEntry struct {
	VolumeEntry

	// Reader is the entry's contents. It is never nil, and reports EOF
	// immediately for a directory or a symlink, so a handler that copies
	// before switching on Kind produces nothing rather than panicking.
	//
	// Valid only for the duration of the handler call: the buffer behind it
	// and its share of the transfer's in-flight byte budget are reclaimed as
	// soon as the handler returns, so a retained Reader reads freed memory.
	// There is nothing to close, and a handler that reads none of it, or stops
	// partway, costs nothing and leaks nothing.
	Reader io.Reader
}
