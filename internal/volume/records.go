package volume

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Wire constants. The vendor media types and the schema string are matched
// verbatim by the server and by other clients, so they are literals rather
// than anything derived.
const (
	// ManifestSchema is the value of the manifest header's manifest_schema.
	ManifestSchema = "v1"

	// ModeMask is the set of permission bits a manifest records. The setuid,
	// setgid, and sticky bits are inside it: a container root filesystem
	// legitimately carries them, and dropping them would silently change the
	// tree a pull reproduces.
	ModeMask = 0o7777

	// ProvenanceFingerprint and ProvenanceFingerprintType are the literals a
	// local push records. They are inside the digested bytes, so they are part
	// of the wire format rather than free-form metadata.
	ProvenanceFingerprint     = "local"
	ProvenanceFingerprintType = "local_push"
)

// FileKind selects how a file entry's bytes are reconstructed.
type FileKind string

const (
	// FileKindChunk carries its single chunk inline in the file record. Valid
	// for files up to one chunk in size, empty files included.
	FileKindChunk FileKind = "chunk"

	// FileKindChunkmap names a chunkmap object listing the file's chunks.
	FileKindChunkmap FileKind = "chunkmap"

	// FileKindSlabmap names a slabmap object packing several small files into
	// one shared chunk. Push never emits it and pull cannot materialize it;
	// the kind exists so a manifest carrying one fails with a clear message
	// rather than a parse error.
	FileKindSlabmap FileKind = "slabmap"
)

// ChunkRef locates one chunk of a file: its digest, where it sits in the file,
// and the object holding its bytes.
type ChunkRef struct {
	Digest Digest
	Length uint64
	Offset uint64
	Target Target
}

// DirectoryEntry is a directory in the tree.
type DirectoryEntry struct {
	Path string
	Mode uint16

	// MTime is the directory's modification time from the scan. Zero means
	// unknown, and the encoder omits the key for it.
	MTime time.Time
}

// SymlinkEntry is a symbolic link. Its target is stored verbatim as readlink
// reported it, and its mode is always 0777: a symlink's own permission bits
// are not meaningful and are never applied on pull.
type SymlinkEntry struct {
	Path   string
	Target string
	Mode   uint16

	// MTime is the link's own modification time from the scan — the link's,
	// never the target's, because the walk records links without following
	// them. Zero means unknown, and the encoder omits the key for it.
	MTime time.Time
}

// FileEntry is a regular file. Which of the trailing fields carry meaning
// depends on Kind.
type FileEntry struct {
	Path string
	Mode uint16
	Kind FileKind

	// Size is the file's length in bytes. For FileKindChunk it equals
	// Chunk.Length and is not carried separately on the wire.
	Size uint64

	// Chunk is the inline chunk of a FileKindChunk entry.
	Chunk ChunkRef

	// Digest and Target name the chunkmap or slabmap object of the other two
	// kinds.
	Digest Digest
	Target Target

	// FileDigest is the whole-file digest a FileKindSlabmap entry carries for
	// per-file integrity, since its chunk is shared with other files.
	FileDigest Digest

	// MTime is the file's modification time from the scan. Zero means
	// unknown, and the encoder omits the key for it.
	MTime time.Time
}

// Provenance records where a manifest's bytes came from. A local push writes
// fixed literals plus the source directory URI, and no resolved_at timestamp:
// a timestamp would make every push of an unchanged tree a new digest.
type Provenance struct {
	SourceFingerprint     string
	SourceFingerprintType string
	SourceURI             string
}

// Manifest is the decoded content of a manifest object: the complete
// description of one version of a volume.
type Manifest struct {
	Provenance  Provenance
	Directories []DirectoryEntry
	Files       []FileEntry
	Symlinks    []SymlinkEntry

	// NormalizedPaths lists entry paths that decoding normalized from the
	// root-anchored form manifests published before the containment rule can
	// carry — the raw spelling the wire carried and the normalized form the
	// decoded entries use, recorded together at the one point the rule is
	// applied so nothing downstream restates it. CheckManifestContainment
	// reports each as a WarningPathNormalized finding.
	NormalizedPaths []NormalizedPath
}

// NormalizedPath is one decode-time path normalization: Raw as the wire
// spelled it, Path as the decoded entry carries it.
type NormalizedPath struct {
	Raw  string
	Path string
}

// EntryCount is the manifest header's entry_count: every directory, file, and
// symlink, and nothing else.
func (m *Manifest) EntryCount() uint64 {
	return uint64(len(m.Directories) + len(m.Files) + len(m.Symlinks))
}

// TotalSize is the manifest header's total_size: the sum of file sizes.
func (m *Manifest) TotalSize() uint64 {
	var total uint64
	for _, f := range m.Files {
		total += f.Size
	}
	return total
}

// Chunkmap is the decoded content of a chunkmap object: the ordered chunks of
// one file.
type Chunkmap struct {
	FileSize uint64
	Chunks   []ChunkRef
}

// mtimeFloor and mtimeCap bound the modification times a manifest carries:
// the RFC 3339 wire form holds exactly four year digits, and Go's formatter
// silently renders a time outside them into a string its own parser rejects
// — Format never errors — so an out-of-range time from a user-supplied tree
// would put unreadable bytes under the digest. Out-of-range times are
// clamped rather than refused, which is what the format's producers do: a
// push should not fail because one inode carries a corrupt timestamp.
var (
	mtimeFloor = time.Date(0, time.January, 1, 0, 0, 0, 0, time.UTC)
	mtimeCap   = time.Date(9999, time.December, 31, 23, 59, 59, 999999999, time.UTC)
)

// clampMTime bounds t to the wire-representable range. The zero time is kept
// as is: it means unknown, and the encoder omits the key for it. The floor is
// year zero, which is below the zero time's year one, so no clamped value can
// collide with the omit sentinel.
func clampMTime(t time.Time) time.Time {
	switch {
	case t.IsZero():
		return t
	case t.Before(mtimeFloor):
		return mtimeFloor
	case t.After(mtimeCap):
		return mtimeCap
	}
	return t
}

// pathComponentCompare orders manifest entry paths canonically: each byte
// maps to a key — '/' to 0, any other byte c to c+1 — and the two key
// sequences compare lexicographically, a shorter prefix first. Mapping the
// separator below every byte puts a directory immediately before its children
// and keeps every subtree contiguous; plain bytewise order does not (it puts
// "a.txt" between "a" and "a/b", splitting the subtree).
//
// The +1 keeps a NUL byte distinct from the separator. ValidatePath refuses
// NUL on push, but DecodeManifest is deliberately lenient, so a
// decoded-then-re-encoded manifest can carry one, and without the offset it
// would sort as if it were '/'.
func pathComponentCompare(a, b string) int {
	key := func(c byte) int {
		if c == '/' {
			return 0
		}
		return int(c) + 1
	}
	for i := 0; i < len(a) && i < len(b); i++ {
		if d := cmp.Compare(key(a[i]), key(b[i])); d != 0 {
			return d
		}
	}
	return cmp.Compare(len(a), len(b))
}

// EncodeManifest renders the manifest as canonical JSONL: the header, the
// provenance, then every entry — directories, files and symlinks together —
// as one run in canonical path order (see pathComponentCompare). The bytes
// depend only on the manifest's content and never on the order a walk or a
// caller produced them in, and EncodeManifest does not mutate the manifest.
//
// The scanner's walk already yields entries in this order, but manifests are
// also built from decoded priors, from subsets, and by arbitrary callers, so
// the order is imposed here rather than inherited from the walk. Duplicate
// paths are rejected upstream — the format forbids them outright — and for a
// hand-built manifest carrying them anyway the stable sort keeps their
// relative order, so the bytes are deterministic either way. DecodeManifest
// stays lenient about the order it accepts.
func EncodeManifest(m *Manifest) []byte {
	type entryRef struct {
		path  string
		kind  uint8 // 0 directory, 1 file, 2 symlink
		index int
	}
	refs := make([]entryRef, 0, len(m.Directories)+len(m.Files)+len(m.Symlinks))
	for i, d := range m.Directories {
		refs = append(refs, entryRef{path: d.Path, kind: 0, index: i})
	}
	for i, f := range m.Files {
		refs = append(refs, entryRef{path: f.Path, kind: 1, index: i})
	}
	for i, s := range m.Symlinks {
		refs = append(refs, entryRef{path: s.Path, kind: 2, index: i})
	}
	slices.SortStableFunc(refs, func(a, b entryRef) int { return pathComponentCompare(a.path, b.path) })

	out := make([]byte, 0, 256*len(refs)+256)

	out = appendType(out, "manifest_header")
	out = appendUint(out, "entry_count", m.EntryCount())
	out = appendString(out, "manifest_schema", ManifestSchema)
	out = appendUint(out, "total_size", m.TotalSize())
	out = endRecord(out)

	out = appendType(out, "provenance")
	out = appendString(out, "source_fingerprint", m.Provenance.SourceFingerprint)
	out = appendString(out, "source_fingerprint_type", m.Provenance.SourceFingerprintType)
	out = appendString(out, "source_uri", m.Provenance.SourceURI)
	out = endRecord(out)

	for _, ref := range refs {
		switch ref.kind {
		case 0:
			d := m.Directories[ref.index]
			out = appendType(out, "directory")
			out = appendMode(out, d.Mode)
			out = appendMTime(out, d.MTime)
			out = appendString(out, "path", d.Path)
			out = endRecord(out)
		case 1:
			out = appendFileRecord(out, m.Files[ref.index])
		default:
			s := m.Symlinks[ref.index]
			out = appendType(out, "symlink")
			out = appendMode(out, s.Mode)
			out = appendMTime(out, s.MTime)
			out = appendString(out, "path", s.Path)
			out = appendString(out, "target", s.Target)
			out = endRecord(out)
		}
	}
	return out
}

// appendFileRecord writes one file record. The keys after the two
// discriminators are lexicographic, which puts them in a different order for
// each kind.
func appendFileRecord(out []byte, f FileEntry) []byte {
	out = appendType(out, "file")
	out = append(out, `,"_kind":`...)
	out = appendJSONString(out, string(f.Kind))
	switch f.Kind {
	case FileKindChunk:
		out = append(out, `,"chunk":`...)
		out = appendChunkObject(out, f.Chunk)
		out = appendMode(out, f.Mode)
		out = appendMTime(out, f.MTime)
		out = appendString(out, "path", f.Path)
	case FileKindChunkmap:
		out = appendString(out, "digest", f.Digest.String())
		out = appendMode(out, f.Mode)
		out = appendMTime(out, f.MTime)
		out = appendString(out, "path", f.Path)
		out = appendUint(out, "size", f.Size)
		out = appendTarget(out, f.Target)
	case FileKindSlabmap:
		out = appendString(out, "digest", f.Digest.String())
		out = appendString(out, "file_digest", f.FileDigest.String())
		out = appendMode(out, f.Mode)
		out = appendMTime(out, f.MTime)
		out = appendString(out, "path", f.Path)
		out = appendUint(out, "size", f.Size)
		out = appendTarget(out, f.Target)
	default:
		// Unreachable by construction: a file entry is built by the push
		// engine or read by decodeFile, and both settle the kind. Falling
		// through would emit a record missing everything after the
		// discriminators, and that record's bytes would become a digest.
		panic("volume: file entry with kind " + string(f.Kind))
	}
	return endRecord(out)
}

// EncodeChunkmap renders a chunkmap as canonical JSONL. Chunks are emitted in
// offset order whatever order they arrive in, and EncodeChunkmap does not
// mutate the chunkmap: the sort works on a copy, so a caller's slice keeps
// its own order — the same promise the manifest encoder makes. A caller that
// built the chunks by reading the file forward already has them in emission
// order, and validation requires that order outright.
func EncodeChunkmap(c *Chunkmap) []byte {
	chunks := slices.Clone(c.Chunks)
	slices.SortFunc(chunks, func(a, b ChunkRef) int { return cmp.Compare(a.Offset, b.Offset) })

	out := make([]byte, 0, 160*len(chunks)+64)
	out = appendType(out, "chunkmap_header")
	out = appendUint(out, "chunk_count", uint64(len(chunks)))
	out = appendUint(out, "file_size", c.FileSize)
	out = endRecord(out)

	for _, ch := range chunks {
		out = appendType(out, "chunk")
		out = appendString(out, "digest", ch.Digest.String())
		out = appendUint(out, "length", ch.Length)
		out = appendUint(out, "offset", ch.Offset)
		out = appendTarget(out, ch.Target)
		out = endRecord(out)
	}
	return out
}

// ValidateChunkmap enforces the invariants the server re-checks at commit: the
// chunks tile the file exactly, in order, with no gaps, overlaps, or empty
// chunks.
func ValidateChunkmap(c *Chunkmap) error {
	if len(c.Chunks) == 0 {
		return fmt.Errorf("chunkmap: no chunks")
	}
	var offset uint64
	for i, ch := range c.Chunks {
		if ch.Length == 0 {
			return fmt.Errorf("chunkmap: chunk %d has zero length", i)
		}
		if ch.Offset != offset {
			return fmt.Errorf("chunkmap: chunk %d starts at offset %d, want %d", i, ch.Offset, offset)
		}
		offset += ch.Length
	}
	if offset != c.FileSize {
		return fmt.Errorf("chunkmap: chunks cover %d bytes, file_size is %d", offset, c.FileSize)
	}
	return nil
}

// Canonical JSON emission.
//
// Key order is not why these helpers hand-roll the encoding: encoding/json
// emits struct fields in declaration order, which is all the format needs.
// (The order is _type first and _kind second, then the content keys
// lexicographically. _kind is the one carve-out — it sorts before _type, k
// preceding t, yet is emitted after it.)
//
// What encoding/json cannot do is leave the bytes alone. It escapes U+2028 and
// U+2029 unconditionally, even with HTML escaping disabled, and it silently
// replaces invalid UTF-8 with U+FFFD instead of failing. Both change bytes the
// digest covers: the first forks dedup against every other client, and the
// second publishes a path that is not the one on disk. Hand-rolling costs a
// few lines and makes the whole escape set something this file states outright.

// appendType opens a record with its _type discriminator.
func appendType(out []byte, typ string) []byte {
	out = append(out, `{"_type":`...)
	return appendJSONString(out, typ)
}

// endRecord closes a record and terminates its line. Every line ends with a
// newline, the last one included.
func endRecord(out []byte) []byte {
	return append(out, '}', '\n')
}

func appendString(out []byte, key, val string) []byte {
	out = appendComma(out, key)
	return appendJSONString(out, val)
}

func appendUint(out []byte, key string, val uint64) []byte {
	out = appendComma(out, key)
	return strconv.AppendUint(out, val, 10)
}

// appendMode writes a permission mode as the octal string the format uses,
// zero-padded to at least four digits.
//
// The mode is masked to the bits the format records. These bytes are inside
// the digest, so a caller that hands in a value carrying anything above them
// — a Go file mode's type bits, say — would otherwise produce a manifest no
// other client can reproduce for the same tree.
func appendMode(out []byte, mode uint16) []byte {
	mode &= ModeMask
	out = appendComma(out, "mode")
	out = append(out, '"')
	// Zero-padded to four digits, and wider when the mode needs it, without
	// the allocation a Sprintf would cost on every entry of a large manifest.
	// Formatted once: the scratch buffer is what gets appended, not just
	// measured.
	var scratch [8]byte
	digits := strconv.AppendUint(scratch[:0], uint64(mode), 8)
	for pad := len(digits); pad < 4; pad++ {
		out = append(out, '0')
	}
	out = append(out, digits...)
	return append(out, '"')
}

// appendMTime writes the mtime key for a non-zero time and nothing for a
// zero one: absent means unknown, and an epoch key would assert 1970 as
// fact. The key sorts between mode and path, which is where every record
// writer above places it.
//
// The time is clamped here as well as at the scan, because this is where the
// invariant lives: no unreadable bytes under a digest is a property of the
// bytes, and a manifest built without a scan — by hand, from decoded pieces
// — reaches this writer with whatever time its builder set. The scan-site
// clamps stay for a different job, keeping the in-memory entries truthful.
// clampMTime is idempotent — clamping a clamped time changes nothing — so
// the second application cannot drift from the first; there is one clamp
// function and one pair of bounds, applied twice.
func appendMTime(out []byte, t time.Time) []byte {
	if t.IsZero() {
		return out
	}
	return appendString(out, "mtime", clampMTime(t).UTC().Format(time.RFC3339Nano))
}

func appendTarget(out []byte, t Target) []byte {
	out = appendComma(out, "target")
	return appendTargetObject(out, t)
}

func appendTargetObject(out []byte, t Target) []byte {
	out = append(out, `{"relative_key":`...)
	out = appendJSONString(out, t.RelativeKey)
	return append(out, '}')
}

func appendChunkObject(out []byte, c ChunkRef) []byte {
	out = append(out, `{"digest":`...)
	out = appendJSONString(out, c.Digest.String())
	out = appendUint(out, "length", c.Length)
	out = appendUint(out, "offset", c.Offset)
	out = appendTarget(out, c.Target)
	return append(out, '}')
}

func appendComma(out []byte, key string) []byte {
	out = append(out, ',')
	out = appendJSONString(out, key)
	return append(out, ':')
}

// appendJSONString writes s as a JSON string literal, escaping exactly the
// canonical set: the quote, the backslash, and the C0 control characters. Everything else, "<" and "&" and U+2028 included, passes
// through as its own UTF-8 bytes. The caller is responsible for s being valid
// UTF-8; path validation rejects the rest before a manifest is built.
func appendJSONString(out []byte, s string) []byte {
	out = append(out, '"')
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '"' && c != '\\' && c >= 0x20 {
			continue
		}
		out = append(out, s[start:i]...)
		switch c {
		case '"':
			out = append(out, '\\', '"')
		case '\\':
			out = append(out, '\\', '\\')
		case '\b':
			out = append(out, '\\', 'b')
		case '\f':
			out = append(out, '\\', 'f')
		case '\n':
			out = append(out, '\\', 'n')
		case '\r':
			out = append(out, '\\', 'r')
		case '\t':
			out = append(out, '\\', 't')
		default:
			out = append(out, `\u00`...)
			const hexDigits = "0123456789abcdef"
			out = append(out, hexDigits[c>>4], hexDigits[c&0xf])
		}
		start = i + 1
	}
	out = append(out, s[start:]...)
	return append(out, '"')
}

// Decoding.
//
// Decoding is deliberately looser than encoding: unknown keys and unknown
// record types are ignored so a newer server can add either without breaking
// this client. What is checked is what a wrong answer would corrupt, namely
// the digests, the modes, and the header's accounting of the entries.

// wireDiscriminator reads just enough of a record to dispatch it. It is only
// for a record that would not parse in its document's combined shape; see
// skippableRecord.
type wireDiscriminator struct {
	Type string `json:"_type"`
}

// wireRecord is every manifest record in one shape: the header's accounting,
// the provenance, and the three entry kinds. A record is parsed ONCE and
// dispatched on Type afterwards, rather than being parsed to find its type
// and then parsed again to read it. A manifest is one record per entry, so on
// a large volume that second pass is the decode's largest cost and it reads
// bytes already in hand.
//
// Which fields carry meaning depends on Type, and the dispatch is what says
// so. A key two record types spell differently has to stay raw, which is why
// Target is: a symlink's is a string and a file's is an object.
type wireRecord struct {
	Type string `json:"_type"`

	EntryCount uint64 `json:"entry_count"`
	TotalSize  uint64 `json:"total_size"`

	SourceFingerprint     string `json:"source_fingerprint"`
	SourceFingerprintType string `json:"source_fingerprint_type"`
	SourceURI             string `json:"source_uri"`

	Kind       string          `json:"_kind"`
	Chunk      *wireChunk      `json:"chunk"`
	Digest     string          `json:"digest"`
	FileDigest string          `json:"file_digest"`
	Mode       string          `json:"mode"`
	MTime      json.RawMessage `json:"mtime"`
	Path       string          `json:"path"`
	Size       uint64          `json:"size"`
	Target     json.RawMessage `json:"target"`
}

// manifestRecordTypes are the record types the manifest decoder reads. A type
// outside this set is skipped, and skipping it is what lets it carry keys
// these shapes spell differently.
var manifestRecordTypes = map[string]bool{
	"manifest_header": true,
	"provenance":      true,
	"directory":       true,
	"symlink":         true,
	"file":            true,
}

// wireChunk is one chunk, whether inline in a file record or standing as a
// chunkmap's own record.
type wireChunk struct {
	Digest string `json:"digest"`
	Length uint64 `json:"length"`
	Offset uint64 `json:"offset"`
	Target Target `json:"target"`
}

// wireChunkmapRecord is every chunkmap record in one shape, parsed once and
// dispatched like wireRecord. A chunkmap is one record per chunk of one file,
// which is the read a download does most.
type wireChunkmapRecord struct {
	Type       string `json:"_type"`
	ChunkCount uint64 `json:"chunk_count"`
	FileSize   uint64 `json:"file_size"`
	wireChunk
}

var chunkmapRecordTypes = map[string]bool{
	"chunkmap_header": true,
	"chunk":           true,
}

// skippableRecord decides what a record that would not parse in its
// document's combined shape means: nothing when it is a type the decoder does
// not read, and the parse error when it is one it does.
//
// Parsing once is what makes this necessary. A record type this client does
// not know yet can spell a key these shapes also use with another JSON type,
// and ignoring it outright is what parsing the type first bought for free.
func skippableRecord(line []byte, known map[string]bool) bool {
	var d wireDiscriminator
	if err := json.Unmarshal(line, &d); err != nil {
		return false
	}
	return !known[d.Type]
}

// DecodeManifest parses canonical JSONL manifest bytes.
func DecodeManifest(body []byte) (*Manifest, error) {
	d := &manifestDecoder{m: &Manifest{}}
	if err := eachRecord(body, d.record); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	if err := d.finish(); err != nil {
		return nil, err
	}
	return d.m, nil
}

// ManifestTotals is a manifest header's own accounting of the version: every
// entry the document carried and the sum of the file sizes. They are the
// numbers BEFORE any filter, which is why a filtered decode returns them
// rather than leaving the caller to sum what it kept.
//
// Returned beside the manifest rather than carried on it. A Manifest is also
// built by a scan and by SelectEntries, neither of which has read a header,
// so on those the fields would read as zero totals rather than absent ones,
// and EncodeManifest derives the header it writes from the entries: a
// decoded manifest re-encoded after any edit would carry a claim that
// disagrees with its own bytes. What the entries add up to is
// Manifest.EntryCount and Manifest.TotalSize; this is what the document said.
type ManifestTotals struct {
	EntryCount uint64
	TotalSize  uint64
}

// DecodeManifestStream decodes a manifest from a reader, keeping only the
// entries keep returns true for. A nil keep keeps everything, which makes
// this DecodeManifest reading a stream.
//
// keep is called once per entry, in the order the records arrive rather than
// canonical order, so it must not depend on entries it has not seen; the
// entries in the returned manifest are canonically ordered as always. It
// saves memory and nothing else: the whole stream is read either way, since a
// manifest's digest covers all of it and a prefix's digest matches nothing.
//
// The reader is read to EOF even when nothing is kept, so a caller hashing
// what it feeds in ends up with the digest of the whole object.
func DecodeManifestStream(r io.Reader, keep func(Entry) bool) (*Manifest, ManifestTotals, error) {
	d := &manifestDecoder{m: &Manifest{}, keep: keep}
	if err := eachRecordStream(r, d.record); err != nil {
		return nil, ManifestTotals{}, fmt.Errorf("decode manifest: %w", err)
	}
	if err := d.finish(); err != nil {
		return nil, ManifestTotals{}, err
	}
	return d.m, ManifestTotals{EntryCount: d.entryCount, TotalSize: d.totalSize}, nil
}

// manifestDecoder accumulates a manifest one record at a time, so the byte
// and stream decoders parse identically rather than each carrying a copy of
// the same switch.
//
// entryCount and totalSize are tracked over every entry the records carried,
// not over the ones kept, which is what lets the header's accounting be
// checked even when keep discarded most of the manifest.
type manifestDecoder struct {
	m    *Manifest
	keep func(Entry) bool

	header         *wireRecord
	seenProvenance bool
	entryCount     uint64
	totalSize      uint64
}

func (d *manifestDecoder) record(line []byte) error {
	var w wireRecord
	if err := json.Unmarshal(line, &w); err != nil {
		if skippableRecord(line, manifestRecordTypes) {
			return nil
		}
		return err
	}

	switch w.Type {
	case "manifest_header":
		if d.header != nil {
			return fmt.Errorf("more than one manifest_header record")
		}
		header := w
		d.header = &header
		return nil
	case "provenance":
		// The format allows exactly one. Two would mean the manifest
		// disagrees with itself about where it came from, and taking
		// either would be a guess. Zero is tolerated: provenance describes
		// the manifest rather than its contents, so a manifest without it
		// still materializes correctly.
		if d.seenProvenance {
			return fmt.Errorf("more than one provenance record")
		}
		d.seenProvenance = true
		d.m.Provenance = Provenance{
			SourceFingerprint:     w.SourceFingerprint,
			SourceFingerprintType: w.SourceFingerprintType,
			SourceURI:             w.SourceURI,
		}
		return nil
	case "directory":
		mode, err := parseMode(w.Mode)
		if err != nil {
			return err
		}
		mtime, err := parseMTime(w.MTime)
		if err != nil {
			return fmt.Errorf("directory %q: %w", w.Path, err)
		}
		entry := DirectoryEntry{Path: d.normalizeEntryPath(w.Path), Mode: mode, MTime: mtime}
		d.entryCount++
		if d.kept(DirectoryEntryOf(entry)) {
			d.m.Directories = append(d.m.Directories, entry)
		}
		return nil
	case "symlink":
		mode, err := parseMode(w.Mode)
		if err != nil {
			return err
		}
		mtime, err := parseMTime(w.MTime)
		if err != nil {
			return fmt.Errorf("symlink %q: %w", w.Path, err)
		}
		target, err := decodeSymlinkTarget(w.Target)
		if err != nil {
			return fmt.Errorf("symlink %q: %w", w.Path, err)
		}
		entry := SymlinkEntry{Path: d.normalizeEntryPath(w.Path), Target: target, Mode: mode, MTime: mtime}
		d.entryCount++
		if d.kept(SymlinkEntryOf(entry)) {
			d.m.Symlinks = append(d.m.Symlinks, entry)
		}
		return nil
	case "file":
		f, err := decodeFile(w)
		if err != nil {
			return err
		}
		f.Path = d.normalizeEntryPath(f.Path)
		d.entryCount++
		d.totalSize += f.Size
		if d.kept(FileEntryOf(f)) {
			d.m.Files = append(d.m.Files, f)
		}
		return nil
	default:
		// An unknown record type is a field this client does not need
		// yet, not a corrupt manifest.
		return nil
	}
}

// kept asks the filter, if there is one.
func (d *manifestDecoder) kept(entry Entry) bool {
	return d.keep == nil || d.keep(entry)
}

// normalizeEntryPath is the manifest's own normalization, recorded on the
// manifest whether or not the entry survives the filter: what the wire
// spelled is a property of the document, and a caller filtering to one
// subtree should still hear that the version carries pre-rule paths.
func (d *manifestDecoder) normalizeEntryPath(path string) string {
	return d.m.normalizeEntryPath(path)
}

// finish checks what only a complete document can be checked against.
func (d *manifestDecoder) finish() error {
	if d.header == nil {
		return fmt.Errorf("decode manifest: no manifest_header record")
	}
	if d.entryCount != d.header.EntryCount {
		return fmt.Errorf("decode manifest: header claims %d entries, found %d", d.header.EntryCount, d.entryCount)
	}
	if d.totalSize != d.header.TotalSize {
		return fmt.Errorf("decode manifest: header claims %d total bytes, files sum to %d", d.header.TotalSize, d.totalSize)
	}
	return nil
}

// normalizeEntryPath strips the leading slashes an entry path can carry in a
// manifest published before the containment rule, so pre-rule volumes still
// materialize: readers normalize the path rather than refuse the volume. The
// wire bytes are untouched — the digest still covers what was written — and
// the manifest records what it normalized, which the containment check
// reports as a typed warning. Reporting is a deliberate choice: readers
// whose warnings feed command-line output stay silent about this
// normalization, but a download result here carries a typed warning list
// whose charter is exactly this class — findings that did not stop the
// download, written out faithfully and reported rather than silently — and a
// quietly rewritten path would be the one silent mutation in that channel's
// domain. Decode is the one place normalization happens, so the containment
// walk, the link namespace, and materialization all see the same normalized
// form. Push stays strict: a scan never produces a root-anchored path, and
// validation still refuses one.
func (m *Manifest) normalizeEntryPath(path string) string {
	if !strings.HasPrefix(path, "/") {
		return path
	}
	normalized := strings.TrimLeft(path, "/")
	m.NormalizedPaths = append(m.NormalizedPaths, NormalizedPath{Raw: path, Path: normalized})
	return normalized
}

// ValidateObjectTarget mirrors the checks the service applies to a
// relative_key before it will build a storage key from one. The digest
// verification and lease scoping already bound what a hostile manifest can
// do with a target, but a key escaping the namespace prefix would aim reads
// outside it — so a record carrying one is refused where it is decoded
// rather than fetched from.
func ValidateObjectTarget(t Target) error {
	key := t.RelativeKey
	switch {
	case key == "":
		return fmt.Errorf("object target is empty")
	case strings.HasPrefix(key, "/"):
		return fmt.Errorf("object target %q is anchored at the root rather than the namespace", key)
	case strings.Contains(key, ".."):
		return fmt.Errorf("object target %q contains %q", key, "..")
	case strings.ContainsRune(key, 0):
		return fmt.Errorf("object target contains a NUL byte")
	case !strings.HasPrefix(key, "objects/b3/"):
		return fmt.Errorf("object target %q does not name a content-addressed object under objects/b3/", key)
	}
	return nil
}

// decodeSymlinkTarget reads a symlink's target, which the wire spells as a
// string where a file record spells its target as an object, which is why the
// combined record holds the field raw. An absent target is the empty string,
// as it was when the field was typed.
func decodeSymlinkTarget(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var target string
	if err := json.Unmarshal(raw, &target); err != nil {
		return "", err
	}
	return target, nil
}

func decodeFile(w wireRecord) (FileEntry, error) {
	mode, err := parseMode(w.Mode)
	if err != nil {
		return FileEntry{}, err
	}
	mtime, err := parseMTime(w.MTime)
	if err != nil {
		return FileEntry{}, fmt.Errorf("file %q: %w", w.Path, err)
	}
	// The file-level target is read only for the kinds that name an object
	// with it, and that is load-bearing rather than an economy: the encoder
	// never emits one for a FileKindChunk entry and no read path consults it,
	// since the inline chunk's own validated target is what a read follows. So an
	// encoder that ever starts emitting it for chunk entries has to add both
	// the read and its validation here in the same change.
	f := FileEntry{Path: w.Path, Mode: mode, Kind: FileKind(w.Kind), Size: w.Size, MTime: mtime}
	switch f.Kind {
	case FileKindChunk:
		if w.Chunk == nil {
			return FileEntry{}, fmt.Errorf("file %q: chunk entry has no chunk", w.Path)
		}
		if f.Chunk, err = decodeChunkRef(*w.Chunk); err != nil {
			return FileEntry{}, fmt.Errorf("file %q: %w", w.Path, err)
		}
		f.Size = f.Chunk.Length
	case FileKindChunkmap, FileKindSlabmap:
		if f.Digest, err = ParseDigest(w.Digest); err != nil {
			return FileEntry{}, fmt.Errorf("file %q: %w", w.Path, err)
		}
		if len(w.Target) > 0 {
			if err := json.Unmarshal(w.Target, &f.Target); err != nil {
				return FileEntry{}, fmt.Errorf("file %q: target: %w", w.Path, err)
			}
		}
		if err := ValidateObjectTarget(f.Target); err != nil {
			return FileEntry{}, fmt.Errorf("file %q: %w", w.Path, err)
		}
		if f.Kind == FileKindSlabmap {
			if f.FileDigest, err = ParseDigest(w.FileDigest); err != nil {
				return FileEntry{}, fmt.Errorf("file %q: %w", w.Path, err)
			}
		}
	default:
		return FileEntry{}, fmt.Errorf("file %q: unknown kind %q", w.Path, w.Kind)
	}
	return f, nil
}

func decodeChunkRef(w wireChunk) (ChunkRef, error) {
	digest, err := ParseDigest(w.Digest)
	if err != nil {
		return ChunkRef{}, err
	}
	if err := ValidateObjectTarget(w.Target); err != nil {
		return ChunkRef{}, err
	}
	return ChunkRef{Digest: digest, Length: w.Length, Offset: w.Offset, Target: w.Target}, nil
}

// DecodeChunkmap parses canonical JSONL chunkmap bytes and validates that the
// chunks tile the file.
func DecodeChunkmap(body []byte) (*Chunkmap, error) {
	c := &Chunkmap{}
	var header *wireChunkmapRecord
	err := eachRecord(body, func(line []byte) error {
		var w wireChunkmapRecord
		if err := json.Unmarshal(line, &w); err != nil {
			if skippableRecord(line, chunkmapRecordTypes) {
				return nil
			}
			return err
		}
		switch w.Type {
		case "chunkmap_header":
			if header != nil {
				return fmt.Errorf("more than one chunkmap_header record")
			}
			record := w
			header = &record
			return nil
		case "chunk":
			ref, err := decodeChunkRef(w.wireChunk)
			if err != nil {
				return err
			}
			c.Chunks = append(c.Chunks, ref)
			return nil
		default:
			return nil
		}
	})
	if err != nil {
		return nil, fmt.Errorf("decode chunkmap: %w", err)
	}
	if header == nil {
		return nil, fmt.Errorf("decode chunkmap: no chunkmap_header record")
	}
	if uint64(len(c.Chunks)) != header.ChunkCount {
		return nil, fmt.Errorf("decode chunkmap: header claims %d chunks, found %d", header.ChunkCount, len(c.Chunks))
	}
	c.FileSize = header.FileSize
	if err := ValidateChunkmap(c); err != nil {
		return nil, fmt.Errorf("decode chunkmap: %w", err)
	}
	return c, nil
}

// parseMTime reads a record's mtime. An ABSENT key is fine — manifests
// written before the key existed omit it, and so does an entry whose time
// was unknown — but a key that is PRESENT and not a parseable time string is
// refused, the same judgment every other checked field gets: believing it
// would materialize a tree with an invented time. Presence is judged on the
// raw message, because an explicit "" and an explicit null both decode into
// an empty Go string and would otherwise pass as absent — present-and-
// malformed accepted, against this comment's own claim.
//
// The zero instant is refused too — as a SENTINEL COLLISION, not as
// malformed, so the rule above stays exact: "0001-01-01T00:00:00Z" is a
// well-formed time that parses to exactly the value meaning "no time
// recorded" here, and carrying it would vanish silently on re-encode — the
// key dropped, the digest changed. Refusing names the collision instead of
// quietly erasing a value the producer wrote. Revisit trigger: if a real
// producer is ever measured emitting this instant, or any production path
// gains re-encode-of-decoded-entries (the re-push test is the standing
// guard on that), the acceptance shape is a presence-carrying time field —
// never document-only, which would leave the silent erasure in place.
func parseMTime(raw json.RawMessage) (time.Time, error) {
	if len(raw) == 0 {
		return time.Time{}, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil || string(raw) == "null" {
		return time.Time{}, fmt.Errorf("mtime: present but not a time string: %s", raw)
	}
	if s == "" {
		return time.Time{}, fmt.Errorf("mtime: present but empty; the format's omitted form is an absent key")
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("mtime: %w", err)
	}
	if t.IsZero() {
		return time.Time{}, fmt.Errorf("mtime: %q is this decoder's omitted-time sentinel and would be dropped on re-encode; refused rather than silently erased", s)
	}
	return t, nil
}

// eachRecord splits JSONL bytes into records and hands each to fn, which
// parses it. Empty lines are skipped, so a trailing newline is accepted and so
// is its absence.
func eachRecord(body []byte, fn func(line []byte) error) error {
	for n := 0; len(body) > 0; n++ {
		line := body
		if i := bytes.IndexByte(body, '\n'); i >= 0 {
			line, body = body[:i], body[i+1:]
		} else {
			body = nil
		}
		if len(line) == 0 {
			continue
		}
		if err := fn(line); err != nil {
			return fmt.Errorf("record %d: %w", n, err)
		}
	}
	return nil
}

// eachRecordStream is eachRecord over a reader, for a document too large to
// hold: it hands fn each record as the read reaches it, and reads to EOF.
//
// Records are taken as a stream of JSON values rather than split on newlines,
// which is what avoids inventing a maximum record length: a line is as long
// as its longest path, and no bound on that is both safe and obviously
// enough. The framing is looser than eachRecord's as a result: two records on
// one line parse here and fail there. Decoding is deliberately the lenient
// direction, and every producer writes one record per line.
func eachRecordStream(r io.Reader, fn func(line []byte) error) error {
	dec := json.NewDecoder(r)
	for n := 0; ; n++ {
		var line json.RawMessage
		if err := dec.Decode(&line); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("record %d: %w", n, err)
		}
		if err := fn(line); err != nil {
			return fmt.Errorf("record %d: %w", n, err)
		}
	}
	// Nothing is drained here, and nothing needs to be: the Decode that ends
	// the loop reads the trailing whitespace before it reports EOF, and
	// trailing bytes that are not whitespace fail that Decode instead of
	// being left behind. So a successful loop has read the reader to EOF,
	// which is what a caller hashing what it feeds in depends on
	// (TestDecodeManifestStreamReadsToEOF is the guard).
	return nil
}

// parseMode reads the octal string form of a permission mode.
func parseMode(s string) (uint16, error) {
	mode, err := strconv.ParseUint(s, 8, 16)
	if err != nil {
		return 0, fmt.Errorf("mode %q: %w", s, err)
	}
	// Refused rather than masked. Masking here would silently alter what was
	// received, and the value would then differ from the bytes whose digest
	// was verified; leaving it unmasked would let a mode the format forbids
	// reach code that assumes it is already narrowed. Both hide the problem,
	// so the decode fails instead — which is what this decoder already does
	// for every other malformed value in a known field.
	if mode > ModeMask {
		return 0, fmt.Errorf("mode %q: above %04o", s, uint64(ModeMask))
	}
	return uint16(mode), nil
}
