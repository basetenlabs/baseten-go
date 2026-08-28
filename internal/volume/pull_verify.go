//go:build linux || darwin

package volume

import (
	"context"
	"errors"
	"io"
	"os"
)

type stagedEntryKind uint8

const (
	stagedDirectory stagedEntryKind = iota
	stagedFile
	stagedSymlink
)

type stagedExpectedEntry struct {
	kind    stagedEntryKind
	mode    uint16
	file    *plannedFile
	symlink *symlinkEntry
}

type stagedVerifiedEntry struct {
	kind          stagedEntryKind
	stat          rootedFileStat
	symlinkTarget string
}

type stagedTreeSnapshot struct {
	root    rootedFileStat
	entries map[string]stagedVerifiedEntry
}

func expectedStagedEntries(
	plan pullPlan,
	maxTotalEntries int,
	pathLimits portablePathLimits,
) (map[string]stagedExpectedEntry, error) {
	expected := make(map[string]stagedExpectedEntry)
	directories, err := stagingDirectoryPaths(plan, maxTotalEntries, pathLimits)
	if err != nil {
		return nil, err
	}
	for _, path := range directories {
		expected[path] = stagedExpectedEntry{kind: stagedDirectory, mode: 0o700}
	}
	for _, directory := range plan.directories {
		expected[directory.Path] = stagedExpectedEntry{
			kind: stagedDirectory,
			mode: directory.Mode,
		}
	}
	for index := range plan.files {
		file := &plan.files[index]
		expected[file.path] = stagedExpectedEntry{
			kind: stagedFile,
			mode: file.mode,
			file: file,
		}
	}
	for index := range plan.symlinks {
		symlink := &plan.symlinks[index]
		expected[symlink.Path] = stagedExpectedEntry{
			kind:    stagedSymlink,
			mode:    symlink.Mode,
			symlink: symlink,
		}
	}
	return expected, nil
}

func (c *Client) verifyStagingAfterExtraction(
	ctx context.Context,
	root *rootedDirectory,
	directories map[string]rootedFileStat,
	plan pullPlan,
	progress *progressReporter,
) (stagedTreeSnapshot, error) {
	return c.verifyStagingTree(ctx, root, directories, plan, nil, false, progress)
}

func (c *Client) verifyStagingForPublication(
	ctx context.Context,
	root *rootedDirectory,
	directories map[string]rootedFileStat,
	plan pullPlan,
	previous stagedTreeSnapshot,
) (stagedTreeSnapshot, error) {
	return c.verifyStagingTree(ctx, root, directories, plan, &previous, true, nil)
}

func (c *Client) verifyStagingTree(
	ctx context.Context,
	root *rootedDirectory,
	directories map[string]rootedFileStat,
	plan pullPlan,
	previous *stagedTreeSnapshot,
	finalizeModes bool,
	progress *progressReporter,
) (stagedTreeSnapshot, error) {
	expected, err := expectedStagedEntries(
		plan,
		c.maxFiles,
		c.effectivePortablePathLimits(),
	)
	if err != nil {
		return stagedTreeSnapshot{}, err
	}
	seen := make(map[string]bool, len(expected))
	aliases := make(hostAliasRegistry, len(expected)+1)
	verified := stagedTreeSnapshot{
		entries: make(map[string]stagedVerifiedEntry, len(expected)),
	}
	rootStat, err := root.currentStat()
	if err != nil || validateOwnedPrivateDirectory(rootStat) != nil {
		return stagedTreeSnapshot{}, integrityError(
			"verify staging",
			"staging root is not the expected private directory",
		)
	}
	expectedRoot, ok := directories[""]
	if !ok || !sameRootedSnapshot(expectedRoot, rootStat) {
		return stagedTreeSnapshot{}, integrityError(
			"verify staging",
			"staging root changed during extraction",
		)
	}
	if previous != nil {
		if len(previous.entries) != len(expected) ||
			!sameRootedSnapshot(previous.root, rootStat) {
			return stagedTreeSnapshot{}, integrityError(
				"verify staging",
				"staging root changed after verification",
			)
		}
	}
	if err := aliases.add("", rootStat); err != nil {
		return stagedTreeSnapshot{}, err
	}
	var verifiedFiles uint64
	var verifiedBytes uint64

	var walk func(string) error
	walk = func(directory string) error {
		if err := ctx.Err(); err != nil {
			return canceledError("verify staging", err)
		}
		names, err := root.readDirNames(directory, directories, len(expected)-len(seen))
		if err != nil {
			return integrityError("verify staging", "staged directory cannot be read exactly")
		}
		for _, name := range names {
			if err := ctx.Err(); err != nil {
				return canceledError("verify staging", err)
			}
			path := name
			if directory != "" {
				path = directory + "/" + name
			}
			entry, ok := expected[path]
			if !ok || seen[path] {
				return integrityError("verify staging", "staging contains an unexpected entry")
			}
			seen[path] = true
			stat, err := root.lstat(path, directories)
			if err != nil {
				return integrityError("verify staging", "staged entry cannot be inspected safely")
			}
			var prior stagedVerifiedEntry
			if previous != nil {
				var exists bool
				prior, exists = previous.entries[path]
				if !exists ||
					prior.kind != entry.kind ||
					!sameRootedSnapshot(prior.stat, stat) {
					return integrityError(
						"verify staging",
						"staged entry changed after verification",
					)
				}
			}
			if err := aliases.add(path, stat); err != nil {
				return err
			}
			switch entry.kind {
			case stagedDirectory:
				if !stat.mode.IsDir() ||
					stat.uid != uint32(os.Geteuid()) ||
					stat.mode.Perm() != 0o700 {
					return integrityError(
						"verify staging",
						"staged directory metadata does not match the verified plan",
					)
				}
				planned, exists := directories[path]
				if !exists || !sameRootedSnapshot(planned, stat) {
					return integrityError(
						"verify staging",
						"staged directory changed during extraction",
					)
				}
				if err := walk(path); err != nil {
					return err
				}
				handle, opened, err := root.openDirectory(path, directories)
				if err != nil || !sameRootedSnapshot(stat, opened) {
					if handle != nil {
						handle.Close()
					}
					return integrityError(
						"verify staging",
						"staged directory changed during verification",
					)
				}
				if finalizeModes {
					finalMode := os.FileMode(entry.mode & 0o777)
					if err := handle.Chmod(finalMode); err != nil {
						handle.Close()
						return filesystemError(
							"verify staging",
							"staged directory mode cannot be finalized",
						)
					}
				}
				if err := handle.Sync(); err != nil {
					handle.Close()
					return filesystemError(
						"verify staging",
						"staged directory metadata cannot be synchronized",
					)
				}
				finalStat, err := rootedFileStatFromFile(handle)
				closeErr := handle.Close()
				expectedMode := os.FileMode(0o700)
				if finalizeModes {
					expectedMode = os.FileMode(entry.mode & 0o777)
				}
				if err != nil ||
					closeErr != nil ||
					!sameRootedObject(stat, finalStat) ||
					finalStat.mode.Perm() != expectedMode {
					return integrityError(
						"verify staging",
						"staged directory mode finalization was not stable",
					)
				}
				pathStat, err := root.lstat(path, directories)
				if err != nil || !sameRootedSnapshot(finalStat, pathStat) {
					return integrityError(
						"verify staging",
						"staged directory changed during verification",
					)
				}
				directories[path] = finalStat
				verified.entries[path] = stagedVerifiedEntry{
					kind: stagedDirectory,
					stat: finalStat,
				}
			case stagedFile:
				if !stat.mode.IsRegular() ||
					stat.uid != uint32(os.Geteuid()) ||
					stat.nlink != 1 ||
					stat.size < 0 ||
					uint64(stat.size) != entry.file.size ||
					stat.mode.Perm() != 0o600 {
					return integrityError(
						"verify staging",
						"staged file is not private before final verification",
					)
				}
				file, opened, err := root.openRegular(path, os.O_RDWR, 0, directories)
				if err != nil || !sameRootedSnapshot(stat, opened) {
					if file != nil {
						file.Close()
					}
					return integrityError(
						"verify staging",
						"staged file changed before final verification",
					)
				}
				verifyErr := c.verifyStagedFileContent(ctx, file, *entry.file)
				afterContent, statErr := rootedFileStatFromFile(file)
				if verifyErr == nil &&
					(statErr != nil || !sameRootedSnapshot(opened, afterContent)) {
					verifyErr = integrityError(
						"verify staging",
						"staged file changed during content verification",
					)
				}
				if verifyErr == nil && finalizeModes {
					finalMode := os.FileMode(entry.mode & 0o777)
					if err := file.Chmod(finalMode); err != nil {
						verifyErr = filesystemError(
							"verify staging",
							"staged file mode cannot be finalized",
						)
					}
				}
				if verifyErr == nil {
					if err := file.Sync(); err != nil {
						verifyErr = filesystemError(
							"verify staging",
							"staged file metadata cannot be synchronized",
						)
					}
				}
				finalStat, finalStatErr := rootedFileStatFromFile(file)
				closeErr := file.Close()
				if verifyErr != nil {
					return verifyErr
				}
				expectedMode := os.FileMode(0o600)
				if finalizeModes {
					expectedMode = os.FileMode(entry.mode & 0o777)
				}
				if finalStatErr != nil ||
					!sameRootedObject(stat, finalStat) ||
					finalStat.mode.Perm() != expectedMode {
					return integrityError(
						"verify staging",
						"staged file mode finalization was not stable",
					)
				}
				if closeErr != nil {
					return filesystemError(
						"verify staging",
						"verified staged file cannot be closed",
					)
				}
				after, err := root.lstat(path, directories)
				if err != nil || !sameRootedSnapshot(finalStat, after) {
					return integrityError(
						"verify staging",
						"staged file changed during final verification",
					)
				}
				verified.entries[path] = stagedVerifiedEntry{
					kind: stagedFile,
					stat: finalStat,
				}
				nextVerifiedBytes, overflow := addUint64(verifiedBytes, entry.file.size)
				if overflow {
					return protocolError("verify staging", "verified file bytes overflow")
				}
				verifiedFiles++
				verifiedBytes = nextVerifiedBytes
				if progress != nil {
					progress.emit(ProgressEvent{
						Phase:          ProgressVerify,
						CompletedItems: verifiedFiles,
						TotalItems:     totalPointer(uint64(len(plan.files))),
						CompletedBytes: verifiedBytes,
						TotalBytes:     totalPointer(plan.totalSize),
					})
				}
			case stagedSymlink:
				if stat.mode&os.ModeSymlink == 0 ||
					stat.uid != uint32(os.Geteuid()) ||
					stat.nlink != 1 {
					return integrityError(
						"verify staging",
						"staged symlink metadata does not match the verified plan",
					)
				}
				target, err := root.readlink(path, directories)
				if err != nil ||
					target != entry.symlink.Target ||
					previous != nil && target != prior.symlinkTarget {
					return integrityError(
						"verify staging",
						"staged symlink target does not match the verified plan",
					)
				}
				after, err := root.lstat(path, directories)
				if err != nil || !sameRootedSnapshot(stat, after) {
					return integrityError(
						"verify staging",
						"staged symlink changed during final verification",
					)
				}
				verified.entries[path] = stagedVerifiedEntry{
					kind:          stagedSymlink,
					stat:          after,
					symlinkTarget: target,
				}
			default:
				return protocolError("verify staging", "verified plan contains an unknown entry kind")
			}
		}
		return nil
	}
	if err := walk(""); err != nil {
		return stagedTreeSnapshot{}, err
	}
	if len(seen) != len(expected) {
		return stagedTreeSnapshot{}, integrityError(
			"verify staging",
			"staging is missing a planned entry",
		)
	}
	if err := root.sync(); err != nil {
		return stagedTreeSnapshot{}, filesystemError(
			"verify staging",
			"staging root cannot be synchronized",
		)
	}
	finalRoot, err := root.currentStat()
	if err != nil || !sameRootedSnapshot(rootStat, finalRoot) {
		return stagedTreeSnapshot{}, integrityError(
			"verify staging",
			"staging root changed during verification",
		)
	}
	verified.root = finalRoot
	return verified, nil
}

func (c *Client) verifyStagedFileContent(
	ctx context.Context,
	file *os.File,
	planned plannedFile,
) error {
	const bufferSize = 256 << 10
	if len(planned.chunks) == 0 {
		digest, err := c.digest(nil)
		if err != nil {
			return err
		}
		if planned.size != 0 || digest != blake3EmptyDigest {
			return integrityError(
				"verify staging",
				"empty staged file does not match the verified plan",
			)
		}
		return nil
	}
	buffer := make([]byte, bufferSize)
	for _, chunk := range planned.chunks {
		if err := ctx.Err(); err != nil {
			return canceledError("verify staging", err)
		}
		hasher := c.newHasher()
		if hasher == nil || hasher.Size() != len(Digest{}) {
			return preconditionError(
				"verify staging",
				"hash constructor no longer returns a 32-byte hash",
			)
		}
		remaining := chunk.Length
		offset := chunk.Offset
		for remaining > 0 {
			if err := ctx.Err(); err != nil {
				return canceledError("verify staging", err)
			}
			length := min(uint64(len(buffer)), remaining)
			read, err := file.ReadAt(buffer[:length], int64(offset))
			if err != nil && !errors.Is(err, io.EOF) {
				return filesystemError("verify staging", "staged file content cannot be read")
			}
			if uint64(read) != length {
				return integrityError(
					"verify staging",
					"staged file is shorter than its verified plan",
				)
			}
			if err := writeFullHash(hasher, buffer[:read]); err != nil {
				return preconditionError(
					"verify staging",
					"hash implementation rejected staged content",
				)
			}
			offset += uint64(read)
			remaining -= uint64(read)
		}
		sum := hasher.Sum(nil)
		var digest Digest
		if len(sum) != len(digest) {
			return preconditionError(
				"verify staging",
				"hash implementation returned the wrong digest size",
			)
		}
		copy(digest[:], sum)
		if digest != chunk.Digest {
			return integrityError(
				"verify staging",
				"staged file content no longer matches the verified plan",
			)
		}
	}
	return nil
}
