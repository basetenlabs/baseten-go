package volume

import (
	"fmt"
	"runtime"
	"strings"
	"unicode/utf8"
)

// DefaultDirMode is the mode given to a directory that is synthesized as an
// ancestor of an entry the walk reached without yielding the directory itself.
const DefaultDirMode uint16 = 0o755

// SymlinkMode is the mode recorded for every symlink. A symlink's own
// permission bits are not meaningful — there is no working lchmod, and the
// kernel reports 0777 regardless — so the manifest records the constant rather
// than whatever lstat happened to say.
const SymlinkMode uint16 = 0o777

// ValidatePath checks an entry path against the rules every reader of a
// manifest relies on: relative, slash-separated, with no segment that could
// walk out of the tree it is extracted into.
//
// A backslash is rejected rather than translated. On Windows the walk has
// already turned the separator into a slash, so a backslash that survives to
// here is a literal character in a file's name — and a tree carrying one
// cannot be reproduced on Windows at all. Failing is the honest answer; the
// reference client rewrites it to a slash and silently pushes a different
// tree.
func ValidatePath(path string) error {
	switch {
	case path == "":
		return fmt.Errorf("path is empty")
	case !utf8.ValidString(path):
		return fmt.Errorf("path %q is not valid UTF-8", path)
	case strings.ContainsRune(path, 0):
		return fmt.Errorf("path %q contains a NUL byte", path)
	case strings.HasPrefix(path, "/"):
		return fmt.Errorf("path %q is absolute, entry paths are relative to the volume root", path)
	case strings.HasSuffix(path, "/"):
		return fmt.Errorf("path %q has a trailing slash", path)
	case strings.Contains(path, `\`):
		return fmt.Errorf(`path %q contains a backslash, which cannot be reproduced on Windows`, path)
	}
	for _, segment := range strings.Split(path, "/") {
		switch segment {
		case "":
			return fmt.Errorf("path %q has an empty segment", path)
		case ".", "..":
			return fmt.Errorf("path %q has a %q segment", path, segment)
		}
	}
	return nil
}

// ValidateSourceURI checks the provenance URI a push records.
//
// It is inside the bytes the manifest digest covers, so it is held to the same
// standard as an entry path: text that cannot be encoded faithfully would be
// silently replaced on its way into the digest, and the resulting version
// would name a source that was never given.
func ValidateSourceURI(uri string) error {
	switch {
	case uri == "":
		return fmt.Errorf("source URI is empty")
	case !utf8.ValidString(uri):
		return fmt.Errorf("source URI is not valid UTF-8")
	case strings.ContainsRune(uri, 0):
		return fmt.Errorf("source URI %q contains a NUL byte", uri)
	}
	return nil
}

// NormalizeSymlinkTarget prepares a target read from the local filesystem for
// the manifest. Targets are stored verbatim, so the only change made is on
// Windows, where a relative target's separators become slashes; a unix target
// containing a literal backslash is left alone.
//
// Targets naming a Windows volume — a drive letter, a UNC share, or an
// extended-length prefix — are rejected. There is no way to write one into a
// manifest that a unix puller could reproduce, and writing it anyway would
// hand every other client a link pointing somewhere that does not exist.
func NormalizeSymlinkTarget(target string) (string, error) {
	if target == "" {
		return "", fmt.Errorf("symlink target is empty")
	}
	if strings.ContainsRune(target, 0) {
		return "", fmt.Errorf("symlink target %q contains a NUL byte", target)
	}
	if !utf8.ValidString(target) {
		return "", fmt.Errorf("symlink target %q is not valid UTF-8", target)
	}
	if err := rejectVolumeTarget(target); err != nil {
		return "", err
	}
	if runtime.GOOS == "windows" {
		target = strings.ReplaceAll(target, `\`, "/")
	}
	return target, nil
}

// rejectVolumeTarget reports targets anchored to a Windows volume. It runs on
// every platform: a manifest built on Windows is read on unix, so the check
// belongs to the format rather than to the machine that happens to run it.
func rejectVolumeTarget(target string) error {
	if strings.HasPrefix(target, `\\`) || strings.HasPrefix(target, "//") {
		return fmt.Errorf("symlink target %q names a UNC path", target)
	}
	if len(target) >= 2 && target[1] == ':' {
		if c := target[0]; (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			return fmt.Errorf("symlink target %q names a Windows drive", target)
		}
	}
	return nil
}

// pathIndex detects two entries that name the same file. It catches exact
// duplicates, which the format forbids outright, and paths differing only in
// case, which are distinct on Linux but the same file on the default macOS and
// Windows filesystems.
//
// Pushing such a tree would publish a manifest whose meaning depends on where
// it is pulled; pulling one would overwrite one file with another and report
// success. Neither is worth guessing at, so both fail.
type pathIndex struct {
	seen map[string]string

	// caseSensitive drops the case-folded comparison, for a filesystem that
	// really can hold both members of such a pair.
	caseSensitive bool
}

func newPathIndex(caseSensitive bool) *pathIndex {
	return &pathIndex{seen: make(map[string]string), caseSensitive: caseSensitive}
}

// add records path, returning an error if an equal path — or, unless the
// filesystem distinguishes them, a case-equal one — is already present.
func (p *pathIndex) add(path string) error {
	key := path
	if !p.caseSensitive {
		key = strings.ToLower(path)
	}
	if other, ok := p.seen[key]; ok {
		if other == path {
			return fmt.Errorf("path %q appears twice", path)
		}
		return fmt.Errorf("paths %q and %q differ only in case, and name the same file here", other, path)
	}
	p.seen[key] = path
	return nil
}

// parentPaths returns path's ancestor directories, deepest first, excluding
// the volume root itself.
func parentPaths(path string) []string {
	var parents []string
	for {
		i := strings.LastIndexByte(path, '/')
		if i <= 0 {
			return parents
		}
		path = path[:i]
		parents = append(parents, path)
	}
}
