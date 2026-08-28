package volume

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"unicode/utf8"
)

type sourceSymlinkPolicy uint8

const (
	sourceSymlinksPreserved sourceSymlinkPolicy = iota
	sourceSymlinksUnsupported
)

type sourceSnapshot struct {
	stat rootedFileStat
	size uint64
	mode uint16
}

func sourceSnapshotFromStat(stat rootedFileStat) (sourceSnapshot, error) {
	if !stat.mode.IsRegular() {
		return sourceSnapshot{}, filesystemError(
			"inspect source",
			"source entry is not an available regular file",
		)
	}
	if stat.size < 0 {
		return sourceSnapshot{}, filesystemError("inspect source", "source file has an invalid size")
	}
	return sourceSnapshot{
		stat: stat,
		size: uint64(stat.size),
		mode: sourceFileMode(stat.mode),
	}, nil
}

func (snapshot sourceSnapshot) verify(stat rootedFileStat) error {
	if !stat.mode.IsRegular() ||
		stat.size < 0 ||
		uint64(stat.size) != snapshot.size ||
		sourceFileMode(stat.mode) != snapshot.mode ||
		stat.modified != snapshot.stat.modified ||
		!sameRootedSnapshot(snapshot.stat, stat) {
		return preconditionError("read source", "source file changed while it was being read")
	}
	return nil
}

type sourceTree struct {
	root        *rootedDirectory
	directories map[string]rootedFileStat
}

func (tree *sourceTree) close() error {
	if tree == nil || tree.root == nil {
		return nil
	}
	root := tree.root
	tree.root = nil
	return root.close()
}

func (tree *sourceTree) verifyDirectory(path string) error {
	var (
		current rootedFileStat
		err     error
	)
	if path == "" {
		current, err = tree.root.currentStat()
	} else {
		current, err = tree.root.lstat(path, tree.directories)
	}
	if err != nil {
		return preconditionError("verify source", "source directory changed during transfer")
	}
	expected, ok := tree.directories[path]
	if !ok || !sameRootedSnapshot(expected, current) {
		return preconditionError("verify source", "source directory changed during transfer")
	}
	return nil
}

func (tree *sourceTree) verifyParents(path string) error {
	parentPath := ""
	if separator := strings.LastIndexByte(path, '/'); separator >= 0 {
		parentPath = path[:separator]
	}
	if err := tree.root.verifyDirectoryChain(parentPath, tree.directories); err != nil {
		return preconditionError("verify source", "source directory changed during transfer")
	}
	return nil
}

type sourceFile struct {
	tree         *sourceTree
	relativePath string
	snapshot     sourceSnapshot
}

type sourceSymlink struct {
	entry    symlinkEntry
	snapshot rootedFileStat
}

type pushInputs struct {
	tree        *sourceTree
	files       []sourceFile
	directories []directoryEntry
	symlinks    []symlinkEntry
	linkSources []sourceSymlink
	totalBytes  uint64
}

func (inputs *pushInputs) close() error {
	if inputs == nil || inputs.tree == nil {
		return nil
	}
	tree := inputs.tree
	inputs.tree = nil
	return tree.close()
}

func collectPushInputs(ctx context.Context, root string, maxFiles int) (pushInputs, error) {
	return collectPushInputsWithHook(
		ctx,
		root,
		maxFiles,
		defaultPortablePathLimits(),
		nil,
	)
}

func collectPushInputsWithHook(
	ctx context.Context,
	path string,
	maxFiles int,
	pathLimits portablePathLimits,
	afterLstat func(string),
) (pushInputs, error) {
	if err := ctx.Err(); err != nil {
		return pushInputs{}, canceledError("scan source", err)
	}
	root, err := openRootedDirectory(path)
	if err != nil {
		return pushInputs{}, invalidError("scan source", "push source must be a real directory")
	}
	rootStat, err := root.currentStat()
	if err != nil || !rootStat.mode.IsDir() {
		root.close()
		return pushInputs{}, invalidError("scan source", "push source must be a real directory")
	}
	tree := &sourceTree{
		root:        root,
		directories: map[string]rootedFileStat{"": rootStat},
	}
	inputs := pushInputs{tree: tree}
	succeeded := false
	defer func() {
		if !succeeded {
			_ = inputs.close()
		}
	}()

	entryCount := 0
	var walk func(string) error
	walk = func(directory string) error {
		if err := ctx.Err(); err != nil {
			return canceledError("scan source", err)
		}
		remaining := maxFiles - entryCount
		names, err := root.readDirNames(directory, tree.directories, remaining)
		if err != nil {
			if errors.Is(err, syscall.EFBIG) {
				return preconditionError(
					"scan source",
					"source exceeds the configured entry limit",
				)
			}
			return filesystemError("scan source", "source directory cannot be read safely")
		}
		for _, name := range names {
			if err := ctx.Err(); err != nil {
				return canceledError("scan source", err)
			}
			relative := name
			if directory != "" {
				relative = directory + "/" + name
			}
			if !utf8.ValidString(relative) {
				return invalidError("scan source", "source contains a path that is not valid UTF-8")
			}
			if err := validatePortablePath(relative, pathLimits); err != nil {
				if IsCode(err, ErrorPreconditionFailed) {
					return preconditionError(
						"scan source",
						"source path exceeds the configured path limits",
					)
				}
				return invalidError("scan source", "source contains a non-portable path")
			}
			entryCount++
			if entryCount > maxFiles {
				return preconditionError("scan source", "source exceeds the configured entry limit")
			}
			before, err := root.lstat(relative, tree.directories)
			if err != nil {
				return filesystemError("scan source", "source entry cannot be inspected safely")
			}
			if afterLstat != nil {
				afterLstat(relative)
			}
			switch {
			case before.mode.IsDir():
				tree.directories[relative] = before
				handle, opened, err := root.openDirectory(relative, tree.directories)
				if err != nil || !sameRootedSnapshot(before, opened) {
					delete(tree.directories, relative)
					if handle != nil {
						handle.Close()
					}
					return preconditionError(
						"scan source",
						"source directory changed during traversal",
					)
				}
				if err := handle.Close(); err != nil {
					return filesystemError("scan source", "source directory cannot be closed")
				}
				inputs.directories = append(
					inputs.directories,
					directoryEntry{Mode: 0o755, Path: relative},
				)
				if err := walk(relative); err != nil {
					return err
				}
			case before.mode&os.ModeSymlink != 0:
				entry, err := sourceSymlinkForPush(
					root,
					relative,
					tree.directories,
					pathLimits,
				)
				if err != nil {
					return err
				}
				after, statErr := root.lstat(relative, tree.directories)
				if statErr != nil || !sameRootedSnapshot(before, after) {
					return preconditionError(
						"scan source",
						"source symlink changed during traversal",
					)
				}
				if entry != nil {
					inputs.symlinks = append(inputs.symlinks, *entry)
					inputs.linkSources = append(inputs.linkSources, sourceSymlink{
						entry: *entry, snapshot: before,
					})
				}
			case before.mode.IsRegular():
				file, opened, err := root.openRegular(
					relative,
					os.O_RDONLY,
					0,
					tree.directories,
				)
				if err != nil {
					return filesystemError("scan source", "source file cannot be opened safely")
				}
				snapshot, snapshotErr := sourceSnapshotFromStat(opened)
				closeErr := file.Close()
				if snapshotErr != nil || !sameRootedSnapshot(before, opened) {
					return preconditionError(
						"scan source",
						"source file changed during traversal",
					)
				}
				if closeErr != nil {
					return filesystemError("scan source", "source file cannot be closed")
				}
				total, overflow := addUint64(inputs.totalBytes, snapshot.size)
				if overflow {
					return preconditionError("scan source", "source byte count overflows")
				}
				inputs.totalBytes = total
				inputs.files = append(inputs.files, sourceFile{
					tree:         tree,
					relativePath: relative,
					snapshot:     snapshot,
				})
			default:
				return unsupportedError("scan source", "source contains an unsupported special file")
			}
		}
		if err := tree.verifyDirectory(directory); err != nil {
			return err
		}
		return nil
	}
	if err := walk(""); err != nil {
		return pushInputs{}, err
	}
	sort.Slice(inputs.files, func(i, j int) bool {
		return inputs.files[i].relativePath < inputs.files[j].relativePath
	})
	sort.Slice(inputs.directories, func(i, j int) bool {
		return inputs.directories[i].Path < inputs.directories[j].Path
	})
	sort.Slice(inputs.symlinks, func(i, j int) bool {
		return inputs.symlinks[i].Path < inputs.symlinks[j].Path
	})
	sort.Slice(inputs.linkSources, func(i, j int) bool {
		return inputs.linkSources[i].entry.Path < inputs.linkSources[j].entry.Path
	})
	if err := validatePushInputStructure(inputs, maxFiles, pathLimits); err != nil {
		if IsCode(err, ErrorPreconditionFailed) {
			return pushInputs{}, preconditionError(
				"scan source",
				"source exceeds the configured path or total-entry limits",
			)
		}
		return pushInputs{}, invalidError(
			"scan source",
			"source contains an unsafe symlink or path graph",
		)
	}
	succeeded = true
	return inputs, nil
}

func verifyPushInputs(ctx context.Context, inputs pushInputs) error {
	if inputs.tree == nil || inputs.tree.root == nil {
		return preconditionError("verify source", "source root is unavailable")
	}
	for path := range inputs.tree.directories {
		if err := ctx.Err(); err != nil {
			return canceledError("verify source", err)
		}
		if err := inputs.tree.verifyDirectory(path); err != nil {
			return err
		}
	}
	for _, source := range inputs.files {
		if err := ctx.Err(); err != nil {
			return canceledError("verify source", err)
		}
		stat, err := inputs.tree.root.lstat(source.relativePath, inputs.tree.directories)
		if err != nil {
			return preconditionError("verify source", "source file changed during transfer")
		}
		if err := source.snapshot.verify(stat); err != nil {
			return err
		}
	}
	for _, source := range inputs.linkSources {
		if err := ctx.Err(); err != nil {
			return canceledError("verify source", err)
		}
		stat, err := inputs.tree.root.lstat(source.entry.Path, inputs.tree.directories)
		if err != nil || !sameRootedSnapshot(source.snapshot, stat) {
			return preconditionError("verify source", "source symlink changed during transfer")
		}
		target, err := inputs.tree.root.readlink(source.entry.Path, inputs.tree.directories)
		if err != nil || filepath.ToSlash(target) != source.entry.Target {
			return preconditionError("verify source", "source symlink changed during transfer")
		}
	}
	return nil
}

type chunkSource struct {
	source sourceFile
	offset uint64
	length uint64
	digest Digest
}

func sourceChunkCount(size uint64) uint64 {
	if size == 0 {
		return 1
	}
	return 1 + (size-1)/uint64(ChunkSize)
}

func (c *Client) hashSourceFile(ctx context.Context, source sourceFile) ([]chunkSource, error) {
	if err := ctx.Err(); err != nil {
		return nil, canceledError("hash source", err)
	}
	if source.tree == nil || source.tree.root == nil {
		return nil, preconditionError("hash source", "source root is unavailable")
	}
	if err := source.tree.verifyParents(source.relativePath); err != nil {
		return nil, err
	}
	if c.filesystemHooks != nil && c.filesystemHooks.beforePushRead != nil {
		c.filesystemHooks.beforePushRead(source.relativePath)
	}
	file, opened, err := source.tree.root.openRegular(
		source.relativePath,
		os.O_RDONLY,
		0,
		source.tree.directories,
	)
	if err != nil {
		return nil, filesystemError("hash source", "source file cannot be opened safely")
	}
	defer file.Close()
	if err := source.snapshot.verify(opened); err != nil {
		return nil, err
	}

	if source.snapshot.size == 0 {
		if err := ctx.Err(); err != nil {
			return nil, canceledError("hash source", err)
		}
		if err := verifySourceAfterRead(file, source); err != nil {
			return nil, err
		}
		return []chunkSource{{
			source: source,
			digest: blake3EmptyDigest,
		}}, nil
	}

	buffer := make([]byte, ChunkSize)
	chunkCount := sourceChunkCount(source.snapshot.size)
	if chunkCount > uint64(^uint(0)>>1) {
		return nil, preconditionError("hash source", "source chunk count is too large for this platform")
	}
	chunks := make([]chunkSource, 0, int(chunkCount))
	for offset := uint64(0); offset < source.snapshot.size; {
		if err := ctx.Err(); err != nil {
			return nil, canceledError("hash source", err)
		}
		length := min(uint64(ChunkSize), source.snapshot.size-offset)
		if _, err := io.ReadFull(file, buffer[:length]); err != nil {
			return nil, preconditionError("hash source", "source file changed while it was being read")
		}
		if err := ctx.Err(); err != nil {
			return nil, canceledError("hash source", err)
		}
		digest, err := c.digest(buffer[:length])
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, canceledError("hash source", err)
		}
		chunks = append(chunks, chunkSource{
			source: source,
			offset: offset,
			length: length,
			digest: digest,
		})
		offset += length
	}
	if err := verifySourceAfterRead(file, source); err != nil {
		return nil, err
	}
	return chunks, nil
}

func verifySourceAfterRead(file *os.File, source sourceFile) error {
	opened, err := rootedFileStatFromFile(file)
	if err != nil {
		return filesystemError("verify source", "source file cannot be inspected")
	}
	if err := source.snapshot.verify(opened); err != nil {
		return err
	}
	if err := source.tree.verifyParents(source.relativePath); err != nil {
		return err
	}
	current, err := source.tree.root.lstat(source.relativePath, source.tree.directories)
	if err != nil {
		return preconditionError("verify source", "source file changed while it was being read")
	}
	return source.snapshot.verify(current)
}

func (c *Client) readChunkSource(ctx context.Context, source chunkSource) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, canceledError("upload source", err)
	}
	if source.source.tree == nil || source.source.tree.root == nil {
		return nil, preconditionError("upload source", "source root is unavailable")
	}
	if err := source.source.tree.verifyParents(source.source.relativePath); err != nil {
		return nil, err
	}
	if c.filesystemHooks != nil && c.filesystemHooks.beforePushRead != nil {
		c.filesystemHooks.beforePushRead(source.source.relativePath)
	}
	file, opened, err := source.source.tree.root.openRegular(
		source.source.relativePath,
		os.O_RDONLY,
		0,
		source.source.tree.directories,
	)
	if err != nil {
		return nil, filesystemError("upload source", "source file cannot be opened safely")
	}
	defer file.Close()
	if err := source.source.snapshot.verify(opened); err != nil {
		return nil, err
	}
	body := make([]byte, source.length)
	if source.length > 0 {
		reader := io.NewSectionReader(file, int64(source.offset), int64(source.length))
		if _, err := io.ReadFull(reader, body); err != nil {
			return nil, preconditionError("upload source", "source file changed before upload")
		}
	}
	if err := ctx.Err(); err != nil {
		clear(body)
		return nil, canceledError("upload source", err)
	}
	digest, err := c.digest(body)
	if err != nil {
		clear(body)
		return nil, err
	}
	if digest != source.digest {
		clear(body)
		return nil, preconditionError("upload source", "source content changed before upload")
	}
	if err := verifySourceAfterRead(file, source.source); err != nil {
		clear(body)
		return nil, err
	}
	return body, nil
}
