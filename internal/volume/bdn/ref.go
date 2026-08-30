package bdn

import (
	"fmt"
	"strings"
)

// RefScheme is the URI scheme a fully written ref carries.
const RefScheme = "bdn://"

// Ref names a version of a volume.
//
// A ref with no selector means the volume's head, which moves. A ref with a
// tag means whatever that tag currently points at, which also moves. Only a
// ref pinned to a digest names one fixed version, which is why a pull resolves
// once and then works from the pin: a tag that moves mid-download would
// otherwise produce a tree that is half of one version and half of another.
type Ref struct {
	Namespace string
	Volume    string

	// Tag is the tag to resolve, empty for the head.
	Tag string

	// Digest is the pinned digest, or a prefix of one, empty when not pinned.
	// The service accepts a prefix of at least twelve hex characters.
	//
	// Its shape is not checked here: anything after the "@" is passed on, and
	// a digest that is too short or not hex comes back as a 400 naming the
	// problem. The reference CLI rejects those locally instead, so the same
	// mistake costs a round trip here.
	Digest string
}

// ParseRef reads "namespace/volume", optionally followed by ":tag" or
// "@digest", with or without the scheme. Namespace and volume are lowercased,
// as the service does at every boundary; tags are case-sensitive.
func ParseRef(ref string) (Ref, error) {
	rest := strings.TrimPrefix(strings.TrimSpace(ref), RefScheme)

	var parsed Ref
	if at := strings.IndexByte(rest, '@'); at >= 0 {
		parsed.Digest, rest = rest[at+1:], rest[:at]
		if parsed.Digest == "" {
			return Ref{}, fmt.Errorf("ref %q: no digest after @", ref)
		}
	} else if colon := strings.IndexByte(rest, ':'); colon >= 0 {
		parsed.Tag, rest = rest[colon+1:], rest[:colon]
		if parsed.Tag == "" {
			return Ref{}, fmt.Errorf("ref %q: no tag after :", ref)
		}
	}

	namespace, vol, ok := strings.Cut(rest, "/")
	if !ok || namespace == "" || vol == "" || strings.Contains(vol, "/") {
		return Ref{}, fmt.Errorf("ref %q: want namespace/volume", ref)
	}
	parsed.Namespace, parsed.Volume = strings.ToLower(namespace), strings.ToLower(vol)
	return parsed, nil
}

// String renders the ref in the form the service parses.
func (r Ref) String() string {
	ref := RefScheme + r.Namespace + "/" + r.Volume
	switch {
	case r.Digest != "":
		return ref + "@" + r.Digest
	case r.Tag != "":
		return ref + ":" + r.Tag
	default:
		return ref
	}
}

// Pinned returns the same volume pinned to a digest, which names one version
// that cannot change underneath a transfer.
func (r Ref) Pinned(digest string) Ref {
	return Ref{Namespace: r.Namespace, Volume: r.Volume, Digest: digest}
}
