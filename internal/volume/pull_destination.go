package volume

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"unicode/utf8"
)

type destinationPreflight struct {
	parentPath     string
	parent         *rootedDirectory
	parentIdentity rootedFileStat
	destination    string
	name           string
	existed        bool
	info           rootedFileStat
}

func (destination *destinationPreflight) close() error {
	if destination == nil || destination.parent == nil {
		return nil
	}
	parent := destination.parent
	destination.parent = nil
	return parent.close()
}

func preflightDestination(destination string) (destinationPreflight, error) {
	if destination == "" {
		return destinationPreflight{}, invalidError("prepare pull destination", "Destination is required")
	}
	if !utf8.ValidString(destination) {
		return destinationPreflight{}, invalidError(
			"prepare pull destination",
			"destination must be valid UTF-8",
		)
	}
	clean := filepath.Clean(destination)
	base := filepath.Base(clean)
	if base == "." || base == string(filepath.Separator) || base == "" {
		return destinationPreflight{}, invalidError(
			"prepare pull destination",
			"destination must name a directory",
		)
	}
	if err := validatePortablePathSyntax(filepath.ToSlash(base)); err != nil {
		return destinationPreflight{}, invalidError(
			"prepare pull destination",
			"destination name is not a portable path component",
		)
	}
	parentInput := filepath.Dir(clean)
	parentPath, err := filepath.EvalSymlinks(parentInput)
	if err != nil {
		return destinationPreflight{}, preconditionError(
			"prepare pull destination",
			"destination parent is unavailable",
		)
	}
	parentPath, err = filepath.Abs(parentPath)
	if err != nil {
		return destinationPreflight{}, filesystemError(
			"prepare pull destination",
			"destination parent cannot be resolved",
		)
	}
	parent, err := openRootedDirectory(parentPath)
	if err != nil {
		return destinationPreflight{}, preconditionError(
			"prepare pull destination",
			"destination parent is not a real directory",
		)
	}
	succeeded := false
	defer func() {
		if !succeeded {
			_ = parent.close()
		}
	}()
	parentIdentity, err := parent.currentStat()
	if err != nil {
		return destinationPreflight{}, filesystemError(
			"prepare pull destination",
			"destination parent cannot be inspected",
		)
	}
	resolvedDestination := filepath.Join(parentPath, base)
	name := filepath.ToSlash(base)
	info, err := parent.lstat(name, nil)
	switch {
	case err == nil:
		if !info.mode.IsDir() {
			return destinationPreflight{}, preconditionError(
				"prepare pull destination",
				"destination must be nonexistent or an empty directory",
			)
		}
		directory, opened, openErr := parent.openDirectory(name, nil)
		if openErr != nil || !sameRootedObject(info, opened) {
			if directory != nil {
				directory.Close()
			}
			return destinationPreflight{}, preconditionError(
				"prepare pull destination",
				"destination changed during inspection",
			)
		}
		entries, readErr := directory.ReadDir(1)
		closeErr := directory.Close()
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return destinationPreflight{}, filesystemError(
				"prepare pull destination",
				"destination cannot be read",
			)
		}
		if closeErr != nil {
			return destinationPreflight{}, filesystemError(
				"prepare pull destination",
				"destination cannot be closed",
			)
		}
		if len(entries) != 0 {
			return destinationPreflight{}, preconditionError(
				"prepare pull destination",
				"destination is not empty",
			)
		}
		succeeded = true
		return destinationPreflight{
			parentPath:     parentPath,
			parent:         parent,
			parentIdentity: parentIdentity,
			destination:    resolvedDestination,
			name:           name,
			existed:        true,
			info:           info,
		}, nil
	case isNotExist(err):
		succeeded = true
		return destinationPreflight{
			parentPath:     parentPath,
			parent:         parent,
			parentIdentity: parentIdentity,
			destination:    resolvedDestination,
			name:           name,
		}, nil
	default:
		return destinationPreflight{}, filesystemError(
			"prepare pull destination",
			"destination cannot be inspected",
		)
	}
}

func revalidateDestinationParent(destination destinationPreflight) error {
	if destination.parent == nil {
		return preconditionError("publish pull", "destination parent is unavailable")
	}
	current, err := destination.parent.currentStat()
	if err != nil ||
		!sameRootedObject(destination.parentIdentity, current) ||
		destination.parentIdentity.mode != current.mode {
		return preconditionError("publish pull", "destination parent changed during transfer")
	}
	ambientInfo, err := os.Lstat(destination.parentPath)
	if err != nil || ambientInfo.Mode()&os.ModeSymlink != 0 || !ambientInfo.IsDir() {
		return preconditionError("publish pull", "destination parent changed during transfer")
	}
	ambient, err := rootedFileStatFromInfo(ambientInfo)
	if err != nil ||
		!sameRootedObject(destination.parentIdentity, ambient) ||
		destination.parentIdentity.mode != ambient.mode {
		return preconditionError("publish pull", "destination parent changed during transfer")
	}
	return nil
}

func revalidateDestination(destination destinationPreflight) error {
	if err := revalidateDestinationParent(destination); err != nil {
		return err
	}
	info, err := destination.parent.lstat(destination.name, nil)
	if destination.existed {
		if err != nil || !info.mode.IsDir() || !sameRootedSnapshot(destination.info, info) {
			return preconditionError("publish pull", "destination changed during transfer")
		}
		directory, opened, err := destination.parent.openDirectory(destination.name, nil)
		if err != nil || !sameRootedSnapshot(destination.info, opened) {
			if directory != nil {
				directory.Close()
			}
			return preconditionError("publish pull", "destination changed during transfer")
		}
		entries, readErr := directory.ReadDir(1)
		closeErr := directory.Close()
		if (readErr != nil && !errors.Is(readErr, io.EOF)) ||
			closeErr != nil ||
			len(entries) != 0 {
			return preconditionError(
				"publish pull",
				"destination changed and is no longer empty",
			)
		}
		return nil
	}
	if err == nil {
		return preconditionError("publish pull", "destination appeared during transfer")
	}
	if !isNotExist(err) {
		return filesystemError("publish pull", "destination cannot be inspected")
	}
	return nil
}

func (c *Client) ensureDestinationCapacity(parent string, contentBytes uint64) error {
	required, overflow := addUint64(contentBytes, c.destinationReserveBytes)
	if overflow {
		return preconditionError(
			"check destination capacity",
			"content bytes plus destination free-space reserve overflow",
		)
	}
	available, err := c.availableSpace(parent)
	if err != nil {
		if errors.Is(err, errDestinationSpaceUnsupported) {
			return unsupportedError(
				"check destination capacity",
				"destination free-space inspection is unavailable on this platform",
			)
		}
		if errors.Is(err, errDestinationSpaceOverflow) {
			return filesystemError(
				"check destination capacity",
				"destination free-space result overflows",
			)
		}
		return filesystemError(
			"check destination capacity",
			"destination free space cannot be inspected",
		)
	}
	if available < required {
		return preconditionError(
			"check destination capacity",
			"destination does not have enough free space for content and the configured reserve",
		)
	}
	return nil
}
