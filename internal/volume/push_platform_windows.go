//go:build windows

package volume

import "os"

const platformSourceSymlinkPolicy = sourceSymlinksUnsupported

func platformSupportsPush() bool {
	return false
}

func sourceFileMode(_ os.FileMode) uint16 {
	return 0o644
}

func sourceSymlinkForPush(
	_ *rootedDirectory,
	_ string,
	_ map[string]rootedFileStat,
	_ portablePathLimits,
) (*symlinkEntry, error) {
	return nil, unsupportedError(
		"scan source",
		"source symlink scanning is unsupported on Windows",
	)
}
