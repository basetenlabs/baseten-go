package client

import (
	"fmt"
	"net/url"
	"strings"
)

const (
	// volumeRefScheme is the ref scheme: a scheme and a path with no
	// authority, the way "mailto:" and "file:" relative forms work. A ref is
	// relative to an org, which is never in the ref and arrives with the
	// authenticated request.
	volumeRefScheme = "bdn:"

	// volumeRefObjectScheme names a stored object rather than a volume. This
	// package takes no object refs, so it is recognized only to reject it with
	// a message saying so rather than as an unknown scheme.
	volumeRefObjectScheme = "bdn+obj://"

	// volumeRefDigestAlgorithm names the hash a digest was taken with. There is
	// one hash today, so it is optional on input and carried on every digest
	// this package writes for itself.
	volumeRefDigestAlgorithm = "b3:"
)

// Limits on the components of a ref.
const (
	volumeRefNameMin   = 2
	volumeRefNameMax   = 256
	volumeRefTagMax    = 128
	volumeRefDigestMax = 64

	// volumeRefDigestShorthand is how much of a digest [VolumeRef.ShorthandString]
	// keeps. It is the shortest prefix the service will resolve, so what the
	// shorthand prints can be pasted back in as a ref.
	volumeRefDigestShorthand = 12
)

// Names the router matches as literal path segments before it matches a name,
// so a namespace or volume so named could never be reached.
var (
	volumeRefReservedNamespaces = []string{"namespaces", "resolve"}
	volumeRefReservedVolumes    = []string{"manifests", "objects"}
)

// VolumeRefLevel is the most specific component a ref names. Every operation
// that takes a ref accepts some levels and rejects others, so the level is
// what decides whether a ref is usable before anything is fetched.
type VolumeRefLevel int

const (
	// VolumeRefLevelNamespace: "bdn:ns".
	VolumeRefLevelNamespace VolumeRefLevel = iota + 1
	// VolumeRefLevelVolume: "bdn:ns/vol", naming the volume itself.
	VolumeRefLevelVolume
	// VolumeRefLevelPoint: "bdn:ns/vol:tag" or "bdn:ns/vol@digest", naming one
	// point within the volume.
	VolumeRefLevelPoint
	// VolumeRefLevelPath: "bdn:ns/vol/x/y" or "bdn:ns/vol:tag/x/y", naming a
	// position within the tree the point presents.
	VolumeRefLevelPath
)

func (l VolumeRefLevel) String() string {
	switch l {
	case VolumeRefLevelNamespace:
		return "namespace"
	case VolumeRefLevelVolume:
		return "volume"
	case VolumeRefLevelPoint:
		return "point"
	case VolumeRefLevelPath:
		return "path"
	}
	return fmt.Sprintf("VolumeRefLevel(%d)", int(l))
}

// VolumeRef names a namespace, a volume, a point within a volume, or a
// position within the tree that point presents.
//
// A ref with no selector names the volume's head, which moves. A ref with a
// tag names whatever that tag currently points at, which also moves. Only a
// ref pinned to a digest names one fixed version, which is why a transfer
// resolves once and then works from the pin: a tag that moved partway through
// would otherwise produce a tree assembled from two versions.
type VolumeRef struct {
	// Namespace is set on every parsed ref. The zero VolumeRef is not a ref.
	Namespace string

	// Volume is empty on a namespace ref.
	Volume string

	// Tag and Digest are the selector, and at most one is set. Digest is 1 to
	// 64 lowercase hex characters, optionally preceded by "b3:", and it is
	// held and rendered in whichever of those two spellings it was read in.
	// Every digest this package writes for itself carries the prefix and the
	// whole 64, which is the form a REST response carries and so is directly
	// comparable to one. Fewer than 64 characters is a prefix; whether one is
	// long enough to accept, and whether it matches more than one version, are
	// decided where it is resolved, so a prefix this type accepts can still be
	// refused by the service.
	Tag    string
	Digest string

	// Path is the position within the point, spelled the way a URL spells a
	// path rather than the way a manifest entry does: it is slash-prefixed,
	// so the empty string means the ref names no path at all and "/" means
	// the tree's root. Segments are percent-decoded, and neither "." nor
	// ".." can appear. A trailing slash is accepted on input and dropped,
	// since no operation distinguishes "x" from "x/".
	//
	// [VolumeEntry.Path] is spelled the same way, so assigning one here builds
	// the ref for that entry with no separator to add.
	Path string
}

// ParseVolumeRef reads a ref in the grammar the volume service documents.
//
// Namespace and volume are accepted in any case and folded to lowercase, as
// the service folds them before validation, storage, and authorization. Tags
// are never folded and are case-sensitive.
func ParseVolumeRef(s string) (VolumeRef, error) {
	trimmed := strings.TrimSpace(s)

	switch {
	case strings.HasPrefix(trimmed, volumeRefObjectScheme):
		return VolumeRef{}, fmt.Errorf(
			"ref %q: object refs name a stored object rather than a volume and are not accepted here", s)
	// Tested before the scheme itself, since "bdn:" is a prefix of "bdn://"
	// and would otherwise read one of these as a ref whose namespace begins
	// with two slashes.
	case strings.HasPrefix(trimmed, "bdn://"):
		return VolumeRef{}, fmt.Errorf("ref %q: bdn:// is not supported; write %sns/vol", s, volumeRefScheme)
	case !strings.HasPrefix(trimmed, volumeRefScheme):
		return VolumeRef{}, fmt.Errorf("ref %q: want a ref beginning %q", s, volumeRefScheme)
	}

	// The first segment is the namespace and the second the volume, and
	// everything after the second slash is the path, so no rule about what a
	// name may contain is needed to find the boundaries.
	segments := strings.SplitN(strings.TrimPrefix(trimmed, volumeRefScheme), "/", 3)

	ref, err := volumeRefNamespace(s, segments[0])
	if err != nil {
		return VolumeRef{}, err
	}
	// "bdn:ns" and "bdn:ns/" are the same namespace ref: after a namespace a
	// trailing slash is merely optional, and only after a volume does it carry
	// meaning.
	if len(segments) == 1 || (len(segments) == 2 && segments[1] == "") {
		return ref, nil
	}
	if err := volumeRefVolume(s, segments[1], &ref); err != nil {
		return VolumeRef{}, err
	}
	if len(segments) == 3 {
		if ref.Path, err = parseVolumeRefPath(s, segments[2]); err != nil {
			return VolumeRef{}, err
		}
	}
	return ref, nil
}

// volumeRefNamespace validates the namespace segment. A selector on it is
// called out separately: the name rule would reject it anyway, but as an
// invalid identifier rather than as the misplaced selector it is.
func volumeRefNamespace(input, segment string) (VolumeRef, error) {
	if strings.ContainsAny(segment, ":@") {
		return VolumeRef{}, fmt.Errorf("ref %q: a namespace ref takes no selector", input)
	}
	name, err := volumeRefName(input, "namespace", segment, volumeRefReservedNamespaces)
	if err != nil {
		return VolumeRef{}, err
	}
	return VolumeRef{Namespace: name}, nil
}

// volumeRefVolume splits the selector off the volume segment and validates
// both. The selector is searched for only here, which is what lets a path
// carry "@" and ":" freely.
func volumeRefVolume(input, segment string, ref *VolumeRef) error {
	// The first "@" wins over any ":", and the search stops at the first of
	// whichever it is, so "vol:a:b" is volume "vol" with the invalid tag
	// "a:b" rather than volume "vol:a" with tag "b".
	if at := strings.IndexByte(segment, '@'); at >= 0 {
		digest, err := volumeRefDigest(input, segment[at+1:])
		if err != nil {
			return err
		}
		ref.Digest, segment = digest, segment[:at]
	} else if colon := strings.IndexByte(segment, ':'); colon >= 0 {
		tag, err := volumeRefTag(input, segment[colon+1:])
		if err != nil {
			return err
		}
		ref.Tag, segment = tag, segment[:colon]
	}

	name, err := volumeRefName(input, "volume", segment, volumeRefReservedVolumes)
	if err != nil {
		return err
	}
	ref.Volume = name
	return nil
}

// volumeRefName folds and validates a namespace or volume. Both take the same
// rule: lowercase ASCII letters, digits, and hyphens, beginning with a letter.
func volumeRefName(input, kind, segment string, reserved []string) (string, error) {
	if segment == "" {
		return "", fmt.Errorf("ref %q: no %s", input, kind)
	}
	name := strings.ToLower(segment)
	if len(name) < volumeRefNameMin || len(name) > volumeRefNameMax {
		return "", fmt.Errorf("ref %q: %s %q must be %d to %d characters",
			input, kind, segment, volumeRefNameMin, volumeRefNameMax)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z':
		case i > 0 && (c >= '0' && c <= '9' || c == '-'):
		default:
			return "", fmt.Errorf(
				"ref %q: %s %q must begin with a letter and hold only lowercase letters, digits, and hyphens",
				input, kind, segment)
		}
	}
	for _, r := range reserved {
		if name == r {
			return "", fmt.Errorf("ref %q: %s %q is reserved", input, kind, segment)
		}
	}
	return name, nil
}

// volumeRefTag validates a tag against the OCI distribution tag grammar. Tags
// are case-sensitive, so nothing is folded here.
func volumeRefTag(input, tag string) (string, error) {
	if tag == "" {
		return "", fmt.Errorf("ref %q: no tag after %q", input, ":")
	}
	if len(tag) > volumeRefTagMax {
		return "", fmt.Errorf("ref %q: tag %q is longer than %d characters", input, tag, volumeRefTagMax)
	}
	for i := 0; i < len(tag); i++ {
		c := tag[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_':
		case i > 0 && (c == '.' || c == '-'):
		default:
			return "", fmt.Errorf(
				"ref %q: tag %q must begin with a letter, digit, or underscore and hold only those plus dots and hyphens",
				input, tag)
		}
	}
	return tag, nil
}

// volumeRefDigest validates a digest and folds its hex to lowercase. The
// "b3:" prefix is optional and is kept exactly as it was written: a full
// digest is spelled with it everywhere it is read or written, while a prefix
// of one is conventionally written bare, and a ref that says either means the
// same version.
func volumeRefDigest(input, digest string) (string, error) {
	lowered := strings.ToLower(digest)
	if lowered == "" {
		return "", fmt.Errorf("ref %q: no digest after %q", input, "@")
	}
	hex, tagged := strings.CutPrefix(lowered, volumeRefDigestAlgorithm)
	if hex == "" && tagged {
		return "", fmt.Errorf("ref %q: no digest after %q", input, volumeRefDigestAlgorithm)
	}
	if len(hex) > volumeRefDigestMax {
		return "", fmt.Errorf("ref %q: digest %q is longer than %d hex characters",
			input, digest, volumeRefDigestMax)
	}
	for i := 0; i < len(hex); i++ {
		c := hex[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return "", fmt.Errorf("ref %q: digest %q is not hex", input, digest)
		}
	}
	return lowered, nil
}

// parseVolumeRefPath reads everything after the second slash. The argument is
// what followed that slash, so an empty one is the tree's root.
func parseVolumeRefPath(input, rest string) (string, error) {
	// A trailing slash means the entry is a directory. Nothing here
	// distinguishes "x" from "x/", so it is dropped rather than recorded, and
	// the root keeps its slash only because that is the whole of its spelling.
	trimmed := strings.TrimSuffix(rest, "/")
	if trimmed == "" {
		return "/", nil
	}

	var path strings.Builder
	for _, segment := range strings.Split(trimmed, "/") {
		if segment == "" {
			return "", fmt.Errorf("ref %q: path has an empty segment", input)
		}
		decoded, err := url.PathUnescape(segment)
		if err != nil {
			return "", fmt.Errorf("ref %q: path segment %q: %w", input, segment, err)
		}
		// An encoded slash would join two segments into one after decoding,
		// which puts the checks below out of reach: "a%2F.." decodes to
		// "a/..", which is neither "." nor "..", and would land a traversal
		// in the path. No entry name can hold a slash either, so there is
		// nothing this could have named.
		if strings.Contains(decoded, "/") {
			return "", fmt.Errorf("ref %q: path segment %q decodes to a slash", input, segment)
		}
		// Checked after decoding, so an encoded "%2e%2e" cannot slip a
		// traversal past the check. A ref is not a filesystem path, so these
		// are refused rather than normalized away.
		if decoded == "." || decoded == ".." {
			return "", fmt.Errorf("ref %q: path segment %q is not allowed", input, decoded)
		}
		path.WriteByte('/')
		path.WriteString(decoded)
	}
	return path.String(), nil
}

// validate refuses a ref that contradicts itself.
//
// [ParseVolumeRef] cannot produce any of these: it reads one selector and
// reaches a path only through a volume. They are reachable only by building a
// VolumeRef by hand, which is a supported thing to do, and each one is a state
// this type would otherwise paper over rather than report. Both selectors set
// silently drops the tag, since rendering prefers the digest; a path or a
// selector with no volume is silently dropped by [VolumeRef.String], which
// stops at the namespace, while [VolumeRef.Level] still reports the deeper
// level.
//
// Grammar is not rechecked here. The parser owns it, and a name or digest that
// is merely wrong produces a ref the service refuses, which is a different
// thing from a ref that does not say what it means.
func (r VolumeRef) validate() error {
	switch {
	case r.Namespace == "":
		return fmt.Errorf("ref %s names no namespace", r)
	case r.Volume == "" && (r.Tag != "" || r.Digest != "" || r.Path != ""):
		return fmt.Errorf("ref %s names no volume to select within", r)
	case r.Tag != "" && r.Digest != "":
		return fmt.Errorf(
			"ref %s names both tag %q and digest %q, and one version cannot be both",
			r, r.Tag, r.Digest)
	}
	return nil
}

// Level reports the most specific component the ref names.
func (r VolumeRef) Level() VolumeRefLevel {
	switch {
	case r.Path != "":
		return VolumeRefLevelPath
	case r.Tag != "" || r.Digest != "":
		return VolumeRefLevelPoint
	case r.Volume != "":
		return VolumeRefLevelVolume
	default:
		return VolumeRefLevelNamespace
	}
}

// String renders the ref canonically, which is the form to print, to hand back
// in a result, and to paste into a config. A namespace ref renders with a
// trailing slash, names render lowercase, a digest renders in the spelling the
// ref holds, and path segments are percent-encoded again.
func (r VolumeRef) String() string {
	var b strings.Builder
	b.WriteString(volumeRefScheme)
	b.WriteString(r.Namespace)
	// A trailing slash is what distinguishes a namespace ref from a bare word,
	// so it is part of the canonical spelling rather than decoration.
	b.WriteByte('/')
	if r.Volume == "" {
		return b.String()
	}
	b.WriteString(r.Volume)

	switch {
	case r.Digest != "":
		b.WriteByte('@')
		b.WriteString(r.Digest)
	case r.Tag != "":
		b.WriteByte(':')
		b.WriteString(r.Tag)
	}

	for _, segment := range strings.Split(strings.TrimPrefix(r.Path, "/"), "/") {
		if segment == "" {
			// Either there is no path, or it is the root, whose spelling is the
			// separator alone.
			if r.Path != "" {
				b.WriteByte('/')
			}
			continue
		}
		b.WriteByte('/')
		b.WriteString(escapeVolumeRefSegment(segment))
	}
	return b.String()
}

// ShorthandString renders the ref the way [VolumeRef.String] does, except that
// a whole digest is shortened to its first 12 hex characters and loses its
// "b3:" prefix, which is how a digest prefix is conventionally written. It is
// for a display where the whole 64 would crowd out everything beside it; the
// result still names the same version and still parses.
//
// A digest that is already a prefix is left alone, since it is as short as the
// ref's author was willing to make it. Everything else renders identically, so
// this shortens the digest and nothing else.
func (r VolumeRef) ShorthandString() string {
	if hex := strings.TrimPrefix(r.Digest, volumeRefDigestAlgorithm); len(hex) == volumeRefDigestMax {
		r.Digest = hex[:volumeRefDigestShorthand]
	}
	return r.String()
}

// escapeVolumeRefSegment percent-encodes everything outside the URI unreserved
// set. net/url's escapers are all wrong here: PathEscape leaves the sub-delims
// alone and QueryEscape writes a space as "+".
func escapeVolumeRefSegment(segment string) string {
	var b strings.Builder
	for i := 0; i < len(segment); i++ {
		c := segment[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
