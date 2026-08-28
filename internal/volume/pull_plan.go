package volume

import (
	"context"
	"errors"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"syscall"
)

type plannedFile struct {
	mode   uint16
	path   string
	size   uint64
	chunks []chunkEntry
}

type pullPlan struct {
	directories []directoryEntry
	files       []plannedFile
	symlinks    []symlinkEntry
	totalSize   uint64
	chunkCount  int
}

func manifestObjectKinds(
	manifestDigest Digest,
	manifest validatedManifest,
) (map[Digest]ObjectKind, error) {
	kinds := make(map[Digest]ObjectKind)
	if err := addSemanticObjectKind(
		kinds,
		manifestDigest,
		ObjectKindManifest,
		"validate content graph",
	); err != nil {
		return nil, err
	}
	for _, file := range manifest.Files {
		kind := ObjectKindChunkmap
		if file.Kind == fileKindChunk {
			kind = ObjectKindChunk
		}
		if err := addSemanticObjectKind(
			kinds,
			file.digest(),
			kind,
			"validate content graph",
		); err != nil {
			return nil, err
		}
	}
	return kinds, nil
}

func normalizeIncludePaths(
	values []string,
	maxSelectors int,
	maxSelectorBytes uint64,
	pathLimits portablePathLimits,
) ([]string, error) {
	if len(values) > maxSelectors {
		return nil, invalidError(
			"validate include selector",
			"include selector count exceeds the configured entry limit",
		)
	}
	normalized := make([]string, 0, len(values))
	seen := make(map[string]bool)
	var selectorBytes uint64
	for _, value := range values {
		value = strings.TrimSuffix(value, "/")
		if err := validatePortablePath(value, pathLimits); err != nil {
			return nil, invalidError(
				"validate include selector",
				"include selector exceeds path limits or is not a portable relative path",
			)
		}
		if !seen[value] {
			nextSelectorBytes, overflow := addUint64(selectorBytes, uint64(len(value)))
			if overflow || nextSelectorBytes > maxSelectorBytes {
				return nil, invalidError(
					"validate include selector",
					"include selector bytes exceed the configured metadata limit",
				)
			}
			selectorBytes = nextSelectorBytes
			seen[value] = true
			normalized = append(normalized, value)
		}
	}
	sort.Strings(normalized)
	return normalized, nil
}

type pathSelectorNode struct {
	selected bool
	children map[string]*pathSelectorNode
}

func newPathSelectorMatcher(selectors []string) *pathSelectorNode {
	root := &pathSelectorNode{children: make(map[string]*pathSelectorNode)}
	for _, selector := range selectors {
		node := root
		for _, component := range strings.Split(selector, "/") {
			next := node.children[component]
			if next == nil {
				next = &pathSelectorNode{children: make(map[string]*pathSelectorNode)}
				node.children[component] = next
			}
			node = next
		}
		node.selected = true
	}
	return root
}

func (root *pathSelectorNode) matches(path string) bool {
	node := root
	for _, component := range strings.Split(path, "/") {
		node = node.children[component]
		if node == nil {
			return false
		}
		if node.selected {
			return true
		}
	}
	return false
}

func selectManifest(
	manifest validatedManifest,
	selectors []string,
	maxTotalEntries int,
	pathLimits portablePathLimits,
) (validatedManifest, error) {
	if len(selectors) == 0 {
		if err := validateManifestStructure(manifest, maxTotalEntries, pathLimits); err != nil {
			return validatedManifest{}, err
		}
		return manifest, nil
	}
	var selected validatedManifest
	matched := false
	matcher := newPathSelectorMatcher(selectors)
	directoriesSelected := make(map[string]bool)
	requiredParents := make(map[string]bool)
	addRequiredParents := func(path string) {
		for index, character := range path {
			if character == '/' {
				requiredParents[path[:index]] = true
			}
		}
	}
	for _, directory := range manifest.Directories {
		if matcher.matches(directory.Path) {
			matched = true
			directoriesSelected[directory.Path] = true
			addRequiredParents(directory.Path)
		}
	}
	for _, file := range manifest.Files {
		if matcher.matches(file.Path) {
			matched = true
			selected.Files = append(selected.Files, file)
			addRequiredParents(file.Path)
			next, overflow := addUint64(selected.TotalSize, file.Size)
			if overflow {
				return validatedManifest{}, protocolError(
					"select manifest",
					"selected manifest size overflows",
				)
			}
			selected.TotalSize = next
		}
	}
	for _, symlink := range manifest.Symlinks {
		if matcher.matches(symlink.Path) {
			matched = true
			selected.Symlinks = append(selected.Symlinks, symlink)
			addRequiredParents(symlink.Path)
		}
	}
	if !matched {
		return validatedManifest{}, preconditionError(
			"select manifest",
			"no include selector matched a manifest entry",
		)
	}
	for _, directory := range manifest.Directories {
		if directoriesSelected[directory.Path] || requiredParents[directory.Path] {
			selected.Directories = append(selected.Directories, directory)
		}
	}
	if err := validateManifestStructure(selected, maxTotalEntries, pathLimits); err != nil {
		return validatedManifest{}, err
	}
	return selected, nil
}

func (c *Client) buildPullPlan(
	ctx context.Context,
	reader ObjectReader,
	manifest validatedManifest,
	progress *progressReporter,
	budget *byteGate,
	objectKinds map[Digest]ObjectKind,
) (pullPlan, error) {
	if err := validateManifestStructure(
		manifest,
		c.maxFiles,
		c.effectivePortablePathLimits(),
	); err != nil {
		return pullPlan{}, err
	}
	if objectKinds == nil {
		objectKinds = make(map[Digest]ObjectKind)
		for _, file := range manifest.Files {
			kind := ObjectKindChunkmap
			if file.Kind == fileKindChunk {
				kind = ObjectKindChunk
			}
			if err := addSemanticObjectKind(
				objectKinds,
				file.digest(),
				kind,
				"validate content graph",
			); err != nil {
				return pullPlan{}, err
			}
		}
	}
	maxChunkmapFanout := max(c.maxManifestBytes/chunkmapFanoutBudgetBytes, uint64(1))
	maxGraphChunks := max(c.maxManifestBytes/contentGraphChunkBudgetBytes, uint64(1))
	maxInt := uint64(^uint(0) >> 1)
	maxChunkmapFanout = min(maxChunkmapFanout, maxInt)
	type chunkmapCacheKey struct {
		digest Digest
		target objectTarget
	}
	chunkmaps := make(map[chunkmapCacheKey]chunkmap)
	var decodedMetadataBytes uint64
	var graphChunks uint64
	var validatedBytes uint64
	plan := pullPlan{
		directories: manifest.Directories,
		symlinks:    manifest.Symlinks,
		totalSize:   manifest.TotalSize,
		files:       make([]plannedFile, 0, len(manifest.Files)),
	}
	for _, file := range manifest.Files {
		if err := ctx.Err(); err != nil {
			return pullPlan{}, canceledError("validate content graph", err)
		}
		planned := plannedFile{mode: file.Mode, path: file.Path, size: file.Size}
		switch file.Kind {
		case fileKindChunk:
			if file.Chunk.Length != 0 {
				planned.chunks = []chunkEntry{file.Chunk}
			}
		case fileKindChunkmap:
			key := chunkmapCacheKey{digest: file.Digest, target: file.Target}
			decoded, ok := chunkmaps[key]
			if !ok {
				if decodedMetadataBytes == c.maxManifestBytes {
					return pullPlan{}, preconditionError(
						"validate content graph",
						"decoded chunkmap metadata exceeds the aggregate limit",
					)
				}
				remainingMetadataBytes := c.maxManifestBytes - decodedMetadataBytes
				objectBody, err := c.readVerifiedObject(
					ctx,
					reader,
					file.Digest,
					expectedObject{
						kind:            ObjectKindChunkmap,
						maxDecodedBytes: remainingMetadataBytes,
					},
					budget,
				)
				if err != nil {
					return pullPlan{}, err
				}
				nextMetadataBytes, overflow := addUint64(
					decodedMetadataBytes,
					uint64(len(objectBody.data)),
				)
				if overflow || nextMetadataBytes > c.maxManifestBytes {
					objectBody.release()
					return pullPlan{}, preconditionError(
						"validate content graph",
						"decoded chunkmap metadata exceeds the aggregate limit",
					)
				}
				decodedMetadataBytes = nextMetadataBytes
				decoded, err = decodeChunkmap(
					objectBody.data,
					remainingMetadataBytes,
					file.Size,
					int(maxChunkmapFanout),
				)
				objectBody.release()
				if err != nil {
					return pullPlan{}, err
				}
				chunkmaps[key] = decoded
			} else if decoded.FileSize != file.Size {
				return pullPlan{}, protocolError(
					"validate content graph",
					"cached chunkmap size does not match the manifest",
				)
			}
			planned.chunks = decoded.Chunks
		default:
			return pullPlan{}, unsupportedError(
				"validate content graph",
				"file kind is not supported",
			)
		}
		for _, chunk := range planned.chunks {
			if err := addSemanticObjectKind(
				objectKinds,
				chunk.Digest,
				ObjectKindChunk,
				"validate content graph",
			); err != nil {
				return pullPlan{}, err
			}
		}
		nextGraphChunks, overflow := addUint64(graphChunks, uint64(len(planned.chunks)))
		if overflow || nextGraphChunks > maxGraphChunks {
			return pullPlan{}, preconditionError(
				"validate content graph",
				"content graph exceeds the aggregate chunk limit",
			)
		}
		graphChunks = nextGraphChunks
		nextValidatedBytes, overflow := addUint64(validatedBytes, file.Size)
		if overflow {
			return pullPlan{}, protocolError(
				"validate content graph",
				"validated file bytes overflow",
			)
		}
		validatedBytes = nextValidatedBytes
		plan.files = append(plan.files, planned)
		progress.emit(ProgressEvent{
			Phase:          ProgressValidate,
			CompletedItems: uint64(len(plan.files)),
			TotalItems:     totalPointer(uint64(len(manifest.Files))),
			CompletedBytes: validatedBytes,
			TotalBytes:     totalPointer(manifest.TotalSize),
		})
	}
	sort.Slice(plan.files, func(i, j int) bool {
		return plan.files[i].path < plan.files[j].path
	})
	plan.chunkCount = int(graphChunks)
	if err := validatePullPlanStructure(
		plan,
		c.maxFiles,
		c.effectivePortablePathLimits(),
	); err != nil {
		return pullPlan{}, err
	}
	return plan, nil
}

type hostEntryKey struct {
	device uint64
	inode  uint64
}

type hostAliasRegistry map[hostEntryKey]string

func (registry hostAliasRegistry) add(path string, stat rootedFileStat) error {
	if !stat.identity.available {
		return unsupportedError(
			"prepare staging",
			"host filesystem identity inspection is unavailable",
		)
	}
	key := hostEntryKey{device: stat.identity.device, inode: stat.identity.inode}
	if previous, exists := registry[key]; exists && previous != path {
		return integrityError(
			"prepare staging",
			"distinct manifest paths alias one host filesystem entry",
		)
	}
	registry[key] = path
	return nil
}

func stagingDirectoryPaths(
	plan pullPlan,
	maxTotalEntries int,
	pathLimits portablePathLimits,
) ([]string, error) {
	if err := validatePullPlanStructure(plan, maxTotalEntries, pathLimits); err != nil {
		return nil, err
	}
	directories := make(map[string]int)
	addPath := func(path string) {
		depth := 1
		for index, character := range path {
			if character == '/' {
				directories[path[:index]] = depth
				depth++
			}
		}
	}
	for _, directory := range plan.directories {
		directories[directory.Path] = strings.Count(directory.Path, "/") + 1
		addPath(directory.Path)
	}
	for _, file := range plan.files {
		addPath(file.path)
	}
	for _, symlink := range plan.symlinks {
		addPath(symlink.Path)
	}
	ordered := make([]string, 0, len(directories))
	for directory := range directories {
		ordered = append(ordered, directory)
	}
	sort.Slice(ordered, func(i, j int) bool {
		leftDepth := directories[ordered[i]]
		rightDepth := directories[ordered[j]]
		if leftDepth == rightDepth {
			return ordered[i] < ordered[j]
		}
		return leftDepth < rightDepth
	})
	return ordered, nil
}

func prepareStagingDirectories(
	ctx context.Context,
	root *rootedDirectory,
	plan pullPlan,
	aliases hostAliasRegistry,
	maxTotalEntries int,
	pathLimits portablePathLimits,
) (map[string]rootedFileStat, error) {
	rootStat, err := root.currentStat()
	if err != nil || validateOwnedPrivateDirectory(rootStat) != nil {
		return nil, filesystemError("prepare staging", "staging root is not private")
	}
	directories := map[string]rootedFileStat{"": rootStat}
	if err := aliases.add("", rootStat); err != nil {
		return nil, err
	}
	ordered, err := stagingDirectoryPaths(plan, maxTotalEntries, pathLimits)
	if err != nil {
		return nil, err
	}
	for _, directory := range ordered {
		if err := ctx.Err(); err != nil {
			return nil, canceledError("prepare staging", err)
		}
		info, err := root.lstat(directory, directories)
		existed := err == nil
		switch {
		case err == nil:
			if !info.mode.IsDir() || !rootedOwnedByCurrentUser(info) {
				return nil, protocolError(
					"prepare staging",
					"staged path conflicts with a directory",
				)
			}
		case isNotExist(err):
			if err := root.mkdir(directory, 0o700, directories); err != nil {
				if errors.Is(err, syscall.ESTALE) {
					return nil, integrityError(
						"prepare staging",
						"manifest directory aliases an existing host path",
					)
				}
				return nil, filesystemError(
					"prepare staging",
					"staging directory cannot be created",
				)
			}
		default:
			if errors.Is(err, syscall.ESTALE) {
				return nil, integrityError(
					"prepare staging",
					"manifest directory aliases an existing host path",
				)
			}
			return nil, filesystemError(
				"prepare staging",
				"staging directory cannot be inspected",
			)
		}
		if !existed {
			info, err = root.lstat(directory, directories)
			if err != nil || !info.mode.IsDir() || !rootedOwnedByCurrentUser(info) {
				return nil, filesystemError(
					"prepare staging",
					"new staged directory cannot be inspected safely",
				)
			}
		}
		if err := aliases.add(directory, info); err != nil {
			return nil, err
		}
		info, err = root.chmodNoFollow(directory, 0o700, directories, info)
		if err != nil {
			return nil, filesystemError(
				"prepare staging",
				"staged directory cannot be made private safely",
			)
		}
		directories[directory] = info
		handle, opened, err := root.openDirectory(directory, directories)
		if err != nil || existed && !sameRootedObject(info, opened) {
			if handle != nil {
				handle.Close()
			}
			return nil, filesystemError(
				"prepare staging",
				"staged directory cannot be opened safely",
			)
		}
		if !rootedOwnedByCurrentUser(opened) {
			handle.Close()
			return nil, filesystemError("prepare staging", "staged directory has an invalid owner")
		}
		opened, err = rootedFileStatFromFile(handle)
		if err != nil || validateOwnedPrivateDirectory(opened) != nil {
			handle.Close()
			return nil, filesystemError("prepare staging", "staged directory is not private")
		}
		if err := handle.Close(); err != nil {
			return nil, filesystemError("prepare staging", "staged directory cannot be closed")
		}
		directories[directory] = opened
	}
	for path := range directories {
		var stat rootedFileStat
		if path == "" {
			stat, err = root.currentStat()
		} else {
			stat, err = root.lstat(path, directories)
		}
		if err != nil || !sameRootedObject(directories[path], stat) {
			return nil, filesystemError(
				"prepare staging",
				"staged directory changed during preparation",
			)
		}
		directories[path] = stat
	}
	return directories, nil
}

type pullDownloadProgress struct {
	mu             sync.Mutex
	reporter       *progressReporter
	remaining      map[string]int
	completedItems uint64
	completedBytes uint64
	totalItems     uint64
	totalBytes     uint64
}

func newPullDownloadProgress(plan pullPlan, reporter *progressReporter) *pullDownloadProgress {
	remaining := make(map[string]int, len(plan.files))
	var completedItems uint64
	for _, file := range plan.files {
		remaining[file.path] = len(file.chunks)
		if len(file.chunks) == 0 {
			completedItems++
		}
	}
	progress := &pullDownloadProgress{
		reporter:       reporter,
		remaining:      remaining,
		completedItems: completedItems,
		totalItems:     uint64(len(plan.files)),
		totalBytes:     plan.totalSize,
	}
	if completedItems > 0 {
		reporter.emit(ProgressEvent{
			Phase:          ProgressDownload,
			CompletedItems: completedItems,
			TotalItems:     totalPointer(progress.totalItems),
			TotalBytes:     totalPointer(progress.totalBytes),
		})
	}
	return progress
}

func (progress *pullDownloadProgress) complete(filePath string, bytes uint64) error {
	progress.mu.Lock()
	remaining, ok := progress.remaining[filePath]
	if !ok || remaining < 1 {
		progress.mu.Unlock()
		return protocolError("report download progress", "download progress item is inconsistent")
	}
	nextBytes, overflow := addUint64(progress.completedBytes, bytes)
	if overflow || nextBytes > progress.totalBytes {
		progress.mu.Unlock()
		return protocolError("report download progress", "download progress bytes overflow")
	}
	progress.completedBytes = nextBytes
	remaining--
	progress.remaining[filePath] = remaining
	if remaining == 0 {
		progress.completedItems++
	}
	event := ProgressEvent{
		Phase:          ProgressDownload,
		CompletedItems: progress.completedItems,
		TotalItems:     totalPointer(progress.totalItems),
		CompletedBytes: progress.completedBytes,
		TotalBytes:     totalPointer(progress.totalBytes),
	}
	progress.mu.Unlock()
	progress.reporter.emit(event)
	return nil
}

func prepareStagingFiles(
	ctx context.Context,
	root *rootedDirectory,
	plan pullPlan,
	directories map[string]rootedFileStat,
	aliases hostAliasRegistry,
) error {
	for _, file := range plan.files {
		if err := ctx.Err(); err != nil {
			return canceledError("prepare staged files", err)
		}
		if file.size > math.MaxInt64 {
			return preconditionError("extract file", "file is too large for this platform")
		}
		flags := os.O_RDWR
		existing, statErr := root.lstat(file.path, directories)
		if isNotExist(statErr) {
			flags |= os.O_CREATE | os.O_EXCL
		} else if statErr != nil {
			if errors.Is(statErr, syscall.ESTALE) {
				return integrityError(
					"prepare staged files",
					"manifest file aliases an existing host path",
				)
			}
			return filesystemError("extract file", "staged file cannot be inspected safely")
		} else if !existing.mode.IsRegular() {
			return integrityError("extract file", "staged file path conflicts with another entry")
		}
		if statErr == nil {
			if !rootedOwnedByCurrentUser(existing) || existing.nlink != 1 {
				return integrityError(
					"extract file",
					"staged file has an unsafe owner or hardlink alias",
				)
			}
			if err := aliases.add(file.path, existing); err != nil {
				return err
			}
			private, chmodErr := root.chmodNoFollow(
				file.path,
				0o600,
				directories,
				existing,
			)
			if chmodErr != nil {
				return filesystemError(
					"extract file",
					"staged file cannot be made private safely",
				)
			}
			existing = private
		}
		output, opened, err := root.openRegular(file.path, flags, 0o600, directories)
		if err != nil {
			return filesystemError("extract file", "staged file cannot be opened safely")
		}
		if statErr == nil && !sameRootedObject(existing, opened) {
			output.Close()
			return integrityError("extract file", "staged file changed during preparation")
		}
		if !rootedOwnedByCurrentUser(opened) || opened.nlink != 1 {
			output.Close()
			return integrityError(
				"extract file",
				"staged file has an unsafe owner or hardlink alias",
			)
		}
		if err := aliases.add(file.path, opened); err != nil {
			output.Close()
			return err
		}
		if err := output.Chmod(0o600); err != nil {
			output.Close()
			return filesystemError("extract file", "staged file cannot be made private")
		}
		if err := output.Truncate(int64(file.size)); err != nil {
			output.Close()
			return filesystemError("extract file", "staged file cannot be sized")
		}
		if err := output.Close(); err != nil {
			return filesystemError("extract file", "staged file cannot be closed")
		}
	}
	return nil
}

func prepareStagingSymlinks(
	ctx context.Context,
	root *rootedDirectory,
	plan pullPlan,
	directories map[string]rootedFileStat,
	aliases hostAliasRegistry,
) error {
	for _, symlink := range plan.symlinks {
		if err := ctx.Err(); err != nil {
			return canceledError("prepare staged symlinks", err)
		}
		before, err := root.lstat(symlink.Path, directories)
		switch {
		case err == nil:
			if before.mode&os.ModeSymlink == 0 {
				return integrityError(
					"prepare staged symlinks",
					"staged symlink path conflicts with another entry",
				)
			}
			target, readErr := root.readlink(symlink.Path, directories)
			if readErr != nil || target != symlink.Target {
				return integrityError(
					"prepare staged symlinks",
					"staged symlink has an unexpected target",
				)
			}
		case isNotExist(err):
			if err := root.symlink(symlink.Target, symlink.Path, directories); err != nil {
				if errors.Is(err, syscall.ESTALE) {
					return integrityError(
						"prepare staged symlinks",
						"manifest symlink aliases an existing host path",
					)
				}
				return filesystemError(
					"prepare staged symlinks",
					"staged symlink cannot be created safely",
				)
			}
		default:
			if errors.Is(err, syscall.ESTALE) {
				return integrityError(
					"prepare staged symlinks",
					"manifest symlink aliases an existing host path",
				)
			}
			return filesystemError(
				"prepare staged symlinks",
				"staged symlink cannot be inspected safely",
			)
		}
		after, err := root.lstat(symlink.Path, directories)
		if err != nil || after.mode&os.ModeSymlink == 0 {
			return filesystemError(
				"prepare staged symlinks",
				"staged symlink changed during preparation",
			)
		}
		if before.identity.available && !sameRootedObject(before, after) {
			return integrityError(
				"prepare staged symlinks",
				"staged symlink changed during preparation",
			)
		}
		if !rootedOwnedByCurrentUser(after) || after.nlink != 1 {
			return integrityError(
				"prepare staged symlinks",
				"staged symlink has an unsafe owner or hardlink alias",
			)
		}
		if err := aliases.add(symlink.Path, after); err != nil {
			return err
		}
	}
	return nil
}

func recaptureStagingDirectories(
	root *rootedDirectory,
	directories map[string]rootedFileStat,
) error {
	for path, previous := range directories {
		var (
			current rootedFileStat
			err     error
		)
		if path == "" {
			current, err = root.currentStat()
		} else {
			current, err = root.lstat(path, directories)
		}
		if err != nil || !sameRootedObject(previous, current) {
			return filesystemError(
				"prepare staging",
				"staged directory changed during preparation",
			)
		}
		directories[path] = current
	}
	return nil
}
