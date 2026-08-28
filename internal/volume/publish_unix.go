//go:build linux || darwin

package volume

import (
	"errors"
	"io"
)

func atomicPublishDirectory(
	sourceRoot *rootedDirectory,
	sourcePath string,
	sourceIdentity rootedFileStat,
	destinationRoot *rootedDirectory,
	destinationPath string,
	destinationExists bool,
	destinationIdentity rootedFileStat,
) (bool, error) {
	source, err := sourceRoot.lstat(sourcePath, nil)
	if err != nil || !sameRootedSnapshot(sourceIdentity, source) {
		return false, errStagingPublishIdentityChanged
	}
	if !destinationExists {
		if err := sourceRoot.rename(
			sourcePath,
			destinationRoot,
			destinationPath,
			true,
		); err != nil {
			return false, err
		}
		published, err := destinationRoot.lstat(destinationPath, nil)
		if err != nil || !samePublishedDirectory(sourceIdentity, published) {
			if rollbackErr := destinationRoot.rename(
				destinationPath,
				sourceRoot,
				sourcePath,
				true,
			); rollbackErr != nil {
				return true, rollbackErr
			}
			return false, errStagingPublishIdentityChanged
		}
		return true, nil
	}
	if err := sourceRoot.exchange(
		sourcePath,
		destinationRoot,
		destinationPath,
	); err != nil {
		return false, err
	}
	published, publishedErr := destinationRoot.lstat(destinationPath, nil)
	replaced, replacedErr := sourceRoot.lstat(sourcePath, nil)
	if publishedErr == nil &&
		replacedErr == nil &&
		samePublishedDirectory(sourceIdentity, published) &&
		samePublishedDirectory(destinationIdentity, replaced) {
		directory, opened, openErr := sourceRoot.openDirectory(sourcePath, nil)
		if openErr == nil &&
			samePublishedDirectory(destinationIdentity, opened) {
			entries, readErr := directory.ReadDir(1)
			closeErr := directory.Close()
			if (readErr == nil || errors.Is(readErr, io.EOF)) &&
				closeErr == nil &&
				len(entries) == 0 {
				return true, nil
			}
		} else if directory != nil {
			directory.Close()
		}
	}
	if rollbackErr := destinationRoot.exchange(
		destinationPath,
		sourceRoot,
		sourcePath,
	); rollbackErr != nil {
		return true, rollbackErr
	}
	return false, errStagingPublishIdentityChanged
}

func samePublishedDirectory(expected, actual rootedFileStat) bool {
	return sameRootedObject(expected, actual) &&
		expected.mode == actual.mode &&
		expected.size == actual.size &&
		expected.modified == actual.modified
}
