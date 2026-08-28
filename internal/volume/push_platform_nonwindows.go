//go:build !windows

package volume

import (
	"os"
	"path/filepath"
	"runtime"
	"unicode/utf8"
)

const platformSourceSymlinkPolicy = sourceSymlinksPreserved

func platformSupportsPush() bool {
	return runtime.GOOS == "linux" || runtime.GOOS == "darwin"
}

func sourceFileMode(mode os.FileMode) uint16 {
	return uint16(mode.Perm())
}

func sourceSymlinkForPush(
	root *rootedDirectory,
	relative string,
	expected map[string]rootedFileStat,
	pathLimits portablePathLimits,
) (*symlinkEntry, error) {
	target, err := root.readlink(relative, expected)
	if err != nil {
		return nil, filesystemError("scan source", "source symlink cannot be read")
	}
	target = filepath.ToSlash(target)
	if !utf8.ValidString(target) {
		return nil, invalidError("scan source", "source symlink target is not valid UTF-8")
	}
	if err := validateSymlinkTarget(relative, target, pathLimits); err != nil {
		if IsCode(err, ErrorPreconditionFailed) {
			return nil, preconditionError(
				"scan source",
				"source symlink target exceeds the configured path limits",
			)
		}
		return nil, invalidError("scan source", "source contains a non-portable symlink")
	}
	return &symlinkEntry{Mode: 0o777, Path: relative, Target: target}, nil
}
