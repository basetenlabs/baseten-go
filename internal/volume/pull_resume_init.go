package volume

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"unicode/utf8"
)

func newPullIdentity(
	manifestDigest Digest,
	destination string,
	selectors []string,
) (pullIdentity, error) {
	return newPullIdentityWithLimits(
		manifestDigest,
		destination,
		selectors,
		defaultMaxFiles,
		defaultMaxManifestBytes,
		defaultPortablePathLimits(),
	)
}

func newPullIdentityWithLimits(
	manifestDigest Digest,
	destination string,
	selectors []string,
	maxSelectors int,
	maxSelectorBytes uint64,
	pathLimits portablePathLimits,
) (pullIdentity, error) {
	if !utf8.ValidString(destination) {
		return pullIdentity{}, invalidError(
			"prepare pull resume",
			"canonical destination is not valid UTF-8",
		)
	}
	if err := validatePathResourceLimits(destination, pathLimits); err != nil {
		return pullIdentity{}, invalidError(
			"prepare pull resume",
			"canonical destination exceeds the configured path limits",
		)
	}
	normalized := make([]string, len(selectors))
	copy(normalized, selectors)
	sort.Strings(normalized)
	normalized = compactSortedStrings(normalized)
	identity := pullIdentity{
		FormatVersion:  pullCheckpointVersion,
		ManifestDigest: manifestDigest,
		Destination:    destination,
		Selectors:      normalized,
	}
	if err := validatePullIdentity(
		identity,
		maxSelectors,
		maxSelectorBytes,
		pathLimits,
	); err != nil {
		return pullIdentity{}, err
	}
	return identity, nil
}

func validatePullIdentity(
	identity pullIdentity,
	maxSelectors int,
	maxSelectorBytes uint64,
	pathLimits portablePathLimits,
) error {
	if identity.Destination == "" || !utf8.ValidString(identity.Destination) {
		return integrityError("prepare pull resume", "pull identity destination is invalid")
	}
	if err := validatePathResourceLimits(identity.Destination, pathLimits); err != nil {
		return integrityError(
			"prepare pull resume",
			"pull identity destination exceeds the configured path limits",
		)
	}
	if len(identity.Selectors) > maxSelectors {
		return integrityError(
			"prepare pull resume",
			"pull identity selector count exceeds the configured entry limit",
		)
	}
	var selectorBytes uint64
	for index, selector := range identity.Selectors {
		if err := validatePortablePath(selector, pathLimits); err != nil {
			return integrityError(
				"prepare pull resume",
				"pull identity contains an invalid selector",
			)
		}
		if index != 0 && selector <= identity.Selectors[index-1] {
			return integrityError(
				"prepare pull resume",
				"pull identity selectors are not canonical",
			)
		}
		nextSelectorBytes, overflow := addUint64(selectorBytes, uint64(len(selector)))
		if overflow || nextSelectorBytes > maxSelectorBytes {
			return integrityError(
				"prepare pull resume",
				"pull identity selector bytes exceed the configured metadata limit",
			)
		}
		selectorBytes = nextSelectorBytes
	}
	return nil
}

func compactSortedStrings(values []string) []string {
	if len(values) == 0 {
		return values
	}
	output := values[:1]
	for _, value := range values[1:] {
		if value != output[len(output)-1] {
			output = append(output, value)
		}
	}
	return output
}

func pullIdentityEqual(left, right pullIdentity) bool {
	if left.FormatVersion != right.FormatVersion ||
		left.ManifestDigest != right.ManifestDigest ||
		left.Destination != right.Destination ||
		len(left.Selectors) != len(right.Selectors) {
		return false
	}
	for index := range left.Selectors {
		if left.Selectors[index] != right.Selectors[index] {
			return false
		}
	}
	return true
}

func (c *Client) pullStorageKey(identity pullIdentity) (string, error) {
	encoded, err := json.Marshal(identity)
	if err != nil {
		return "", protocolError("prepare pull resume", "pull identity cannot be encoded")
	}
	sum, err := c.digest(encoded)
	if err != nil {
		return "", err
	}
	return sum.Hex(), nil
}

func expectedPullChunks(
	ctx context.Context,
	plan pullPlan,
) (map[completedPullChunk]struct{}, error) {
	expected := make(map[completedPullChunk]struct{}, plan.chunkCount)
	for _, file := range plan.files {
		for _, chunk := range file.chunks {
			if err := ctx.Err(); err != nil {
				return nil, canceledError("prepare pull resume", err)
			}
			expected[completedPullChunk{
				Path: file.path, Offset: chunk.Offset, Length: chunk.Length, Digest: chunk.Digest,
			}] = struct{}{}
		}
	}
	return expected, nil
}

func (c *Client) openPullResume(
	parentPath string,
	identity pullIdentity,
	expected map[completedPullChunk]struct{},
	restart bool,
) (*pullResume, error) {
	parent, err := openRootedDirectory(parentPath)
	if err != nil {
		return nil, filesystemError("prepare pull resume", "destination parent cannot be opened")
	}
	resume, err := c.openPullResumeAt(parent, parentPath, identity, expected, restart)
	if err != nil {
		parent.close()
		return nil, err
	}
	resume.ownsParent = true
	return resume, nil
}

func (c *Client) openPullResumeAt(
	parent *rootedDirectory,
	parentPath string,
	identity pullIdentity,
	expected map[completedPullChunk]struct{},
	restart bool,
) (*pullResume, error) {
	maxStateBytes, err := pullCheckpointSizeLimit(
		identity,
		expected,
	)
	if err != nil {
		return nil, err
	}
	storageKey, err := c.pullStorageKey(identity)
	if err != nil {
		return nil, err
	}
	stagingName := pullStagingPrefix + storageKey + pullStagingSuffix
	c.removeStalePullStatesAt(parent, parentPath, stagingName)
	if restart {
		if stat, err := parent.lstat(stagingName, nil); err == nil {
			if err := c.removeMatchingPullStateAt(
				parent,
				stagingName,
				stat,
				identity,
			); err != nil {
				return nil, err
			}
		} else if !isNotExist(err) {
			return nil, filesystemError(
				"restart pull",
				"matching resumable state cannot be inspected",
			)
		}
	}
	stat, err := parent.lstat(stagingName, nil)
	if isNotExist(err) {
		return c.initializePullResume(
			parent,
			parentPath,
			stagingName,
			storageKey,
			identity,
			maxStateBytes,
		)
	}
	if err != nil {
		return nil, filesystemError(
			"prepare pull resume",
			"matching resumable state cannot be inspected",
		)
	}
	if !stat.mode.IsDir() {
		return nil, integrityError(
			"prepare pull resume",
			"matching resumable state is not a directory",
		)
	}
	recovered, err := c.recoverIncompletePullStateAt(
		parent,
		stagingName,
		stat,
		identity,
	)
	if err != nil {
		return nil, err
	}
	if recovered {
		return c.initializePullResume(
			parent,
			parentPath,
			stagingName,
			storageKey,
			identity,
			maxStateBytes,
		)
	}
	return c.openExistingPullResume(
		parent,
		parentPath,
		stagingName,
		stat,
		identity,
		expected,
		maxStateBytes,
	)
}

func (c *Client) recoverIncompletePullStateAt(
	parent *rootedDirectory,
	stagingName string,
	expectedStat rootedFileStat,
	identity pullIdentity,
) (bool, error) {
	maxIdentityBytes, err := pullIdentitySizeLimit(identity)
	if err != nil {
		return false, err
	}
	staging, err := parent.openDirectoryRoot(stagingName, nil)
	if err != nil {
		return false, filesystemError(
			"prepare pull resume",
			"existing pull staging cannot be anchored",
		)
	}
	stagingStat, err := staging.currentStat()
	if err != nil ||
		!sameRootedObject(expectedStat, stagingStat) ||
		validateOwnedPrivateDirectory(stagingStat) != nil {
		staging.close()
		return false, integrityError(
			"prepare pull resume",
			"existing pull staging is not the expected private directory",
		)
	}
	if _, err := staging.lstat(pullCheckpointName, nil); err == nil {
		staging.close()
		return false, nil
	} else if !isNotExist(err) {
		staging.close()
		return false, integrityError(
			"prepare pull resume",
			"incomplete pull checkpoint path cannot be inspected",
		)
	}
	lock, err := openRootedPullLock(staging, false)
	if isNotExist(err) {
		lock, err = openRootedPullLock(staging, true)
	}
	if err != nil {
		staging.close()
		return false, pullLockError("prepare pull resume", err)
	}
	hasIdentity, identityErr := readPullIdentityAt(
		staging,
		identity,
		maxIdentityBytes,
		c.maxFiles,
		c.maxManifestBytes,
		c.effectivePortablePathLimits(),
	)
	if identityErr != nil {
		closePullStateLock(lock)
		staging.close()
		return false, identityErr
	}
	names, err := staging.readDirNames(
		"",
		nil,
		pullCleanupEntryLimit(c.maxFiles, c.maxManifestBytes),
	)
	if err != nil {
		closePullStateLock(lock)
		staging.close()
		return false, integrityError(
			"prepare pull resume",
			"incomplete pull state exceeds its safe cleanup bound",
		)
	}
	checkpointTempPrefix := "." + pullCheckpointName + ".tmp-"
	for _, name := range names {
		switch {
		case name == pullIdentityName && hasIdentity:
		case name == pullLockName:
		case name == pullJournalName:
			file, stat, openErr := openRootedPrivateFile(staging, name, os.O_RDONLY)
			if openErr != nil || !hasIdentity && stat.size != 0 {
				if file != nil {
					file.Close()
				}
				closePullStateLock(lock)
				staging.close()
				return false, integrityError(
					"prepare pull resume",
					"incomplete pull journal cannot be proven safe",
				)
			}
			if closeErr := file.Close(); closeErr != nil {
				closePullStateLock(lock)
				staging.close()
				return false, filesystemError(
					"prepare pull resume",
					"incomplete pull journal cannot be closed",
				)
			}
		case name == pullDataName:
			data, openErr := staging.openDirectoryRoot(name, nil)
			if openErr != nil {
				closePullStateLock(lock)
				staging.close()
				return false, integrityError(
					"prepare pull resume",
					"incomplete pull data is not a directory",
				)
			}
			dataStat, statErr := data.currentStat()
			if statErr != nil || validateOwnedPrivateDirectory(dataStat) != nil {
				data.close()
				closePullStateLock(lock)
				staging.close()
				return false, integrityError(
					"prepare pull resume",
					"incomplete pull data is not private",
				)
			}
			if !hasIdentity {
				dataNames, readErr := data.readDirNames("", nil, 0)
				if readErr != nil || len(dataNames) != 0 {
					data.close()
					closePullStateLock(lock)
					staging.close()
					return false, integrityError(
						"prepare pull resume",
						"incomplete pull data identity cannot be proven",
					)
				}
			}
			data.close()
		case strings.HasPrefix(name, checkpointTempPrefix):
			file, _, openErr := openRootedPrivateFile(staging, name, os.O_RDONLY)
			if openErr != nil {
				closePullStateLock(lock)
				staging.close()
				return false, integrityError(
					"prepare pull resume",
					"incomplete checkpoint temporary file is unsafe",
				)
			}
			file.Close()
		default:
			closePullStateLock(lock)
			staging.close()
			return false, integrityError(
				"prepare pull resume",
				"incomplete pull state contains an unrelated entry",
			)
		}
	}
	if !hasIdentity {
		key, keyErr := c.pullStorageKey(identity)
		if keyErr != nil || stagingName != pullStagingPrefix+key+pullStagingSuffix {
			closePullStateLock(lock)
			staging.close()
			return false, integrityError(
				"prepare pull resume",
				"incomplete pull state path identity cannot be proven",
			)
		}
	}
	if err := staging.close(); err != nil {
		closePullStateLock(lock)
		return false, filesystemError(
			"prepare pull resume",
			"incomplete pull staging cannot be closed",
		)
	}
	if !parent.entryMatches(stagingName, stagingStat) {
		closePullStateLock(lock)
		return false, integrityError(
			"prepare pull resume",
			"incomplete pull staging changed before cleanup",
		)
	}
	if err := parent.removeTree(
		stagingName,
		stagingStat,
		pullCleanupEntryLimit(c.maxFiles, c.maxManifestBytes),
	); err != nil {
		closePullStateLock(lock)
		return false, filesystemError(
			"prepare pull resume",
			"incomplete pull state cannot be removed safely",
		)
	}
	if err := closePullStateLock(lock); err != nil {
		return false, filesystemError(
			"prepare pull resume",
			"incomplete pull state cannot be unlocked",
		)
	}
	if err := parent.sync(); err != nil {
		return false, filesystemError(
			"prepare pull resume",
			"destination parent cannot be synchronized after recovery",
		)
	}
	return true, nil
}

func initialPullCheckpoint(identity pullIdentity) (pullCheckpoint, error) {
	now, err := currentUnixSeconds()
	if err != nil {
		return pullCheckpoint{}, err
	}
	return pullCheckpoint{
		Version:              pullCheckpointVersion,
		Identity:             identity,
		CreatedAtUnixSeconds: now,
		UpdatedAtUnixSeconds: now,
		CompletedChunks:      []completedPullChunk{},
	}, nil
}

func (c *Client) resumeInitializationHook(stage string) error {
	if c.filesystemHooks == nil || c.filesystemHooks.duringResumeInitialize == nil {
		return nil
	}
	if err := c.filesystemHooks.duringResumeInitialize(stage); err != nil {
		return filesystemError(
			"prepare pull resume",
			"pull resume initialization was interrupted",
		)
	}
	return nil
}

func createRootedPrivateFile(
	root *rootedDirectory,
	name string,
	flags int,
	body []byte,
) (*os.File, error) {
	file, stat, err := root.openRegular(
		name,
		flags|os.O_CREATE|os.O_EXCL,
		0o600,
		nil,
	)
	if err != nil {
		return nil, err
	}
	succeeded := false
	defer func() {
		if !succeeded {
			file.Close()
			_ = root.remove(name, false, nil)
		}
	}()
	if !rootedOwnedByCurrentUser(stat) || stat.nlink != 1 {
		return nil, syscall.EPERM
	}
	if err := file.Chmod(0o600); err != nil {
		return nil, err
	}
	if len(body) != 0 {
		written, err := file.Write(body)
		if err != nil || written != len(body) {
			return nil, io.ErrShortWrite
		}
	}
	if err := file.Sync(); err != nil {
		return nil, err
	}
	succeeded = true
	return file, nil
}

func (c *Client) initializePullResume(
	parent *rootedDirectory,
	parentPath string,
	stagingName string,
	storageKey string,
	identity pullIdentity,
	maxStateBytes uint64,
) (*pullResume, error) {
	checkpoint, err := initialPullCheckpoint(identity)
	if err != nil {
		return nil, err
	}
	identityBody, err := json.Marshal(identity)
	if err != nil {
		return nil, protocolError("prepare pull resume", "pull identity cannot be encoded")
	}
	checkpointBody, err := json.Marshal(checkpoint)
	if err != nil {
		return nil, protocolError("prepare pull resume", "pull checkpoint cannot be encoded")
	}
	if uint64(len(identityBody)) > maxStateBytes ||
		uint64(len(checkpointBody)) > maxStateBytes {
		return nil, preconditionError(
			"prepare pull resume",
			"pull resume metadata exceeds its byte limit",
		)
	}
	var tempName string
	for range 10 {
		token, tokenErr := newCorrelationID()
		if tokenErr != nil {
			return nil, filesystemError(
				"prepare pull resume",
				"temporary pull state identity cannot be created",
			)
		}
		tempName = pullInitPrefix + storageKey + "-" + token + pullInitSuffix
		if err := parent.mkdir(tempName, 0o700, nil); err == nil {
			break
		} else if !errors.Is(err, fs.ErrExist) {
			return nil, filesystemError(
				"prepare pull resume",
				"temporary pull state cannot be created",
			)
		}
		tempName = ""
	}
	if tempName == "" {
		return nil, filesystemError(
			"prepare pull resume",
			"unique temporary pull state cannot be created",
		)
	}
	temp, err := parent.openDirectoryRoot(tempName, nil)
	if err != nil {
		return nil, filesystemError(
			"prepare pull resume",
			"temporary pull state cannot be anchored",
		)
	}
	tempStat, err := temp.currentStat()
	if err != nil {
		temp.close()
		return nil, filesystemError(
			"prepare pull resume",
			"temporary pull state cannot be inspected",
		)
	}
	installed := false
	succeeded := false
	var data *rootedDirectory
	var lock, journal *os.File
	defer func() {
		if succeeded {
			return
		}
		if journal != nil {
			_ = journal.Close()
		}
		if lock != nil {
			_ = closePullStateLock(lock)
		}
		if data != nil {
			_ = data.close()
		}
		_ = temp.close()
		if !installed && parent.entryMatches(tempName, tempStat) {
			_ = parent.removeTree(tempName, tempStat, 16)
			_ = parent.sync()
		}
	}()
	if err := temp.chmod(0o700); err != nil {
		return nil, filesystemError(
			"prepare pull resume",
			"temporary pull state cannot be made private",
		)
	}
	tempStat, err = temp.currentStat()
	if err != nil || validateOwnedPrivateDirectory(tempStat) != nil {
		return nil, filesystemError(
			"prepare pull resume",
			"temporary pull state is not private",
		)
	}
	if err := c.resumeInitializationHook("temporary-created"); err != nil {
		return nil, err
	}
	identityFile, err := createRootedPrivateFile(
		temp,
		pullIdentityName,
		os.O_RDWR,
		identityBody,
	)
	if err != nil {
		return nil, filesystemError("prepare pull resume", "pull identity cannot be persisted")
	}
	if err := identityFile.Close(); err != nil {
		return nil, filesystemError("prepare pull resume", "pull identity cannot be closed")
	}
	checkpointFile, err := createRootedPrivateFile(
		temp,
		pullCheckpointName,
		os.O_RDWR,
		checkpointBody,
	)
	if err != nil {
		return nil, filesystemError("prepare pull resume", "pull checkpoint cannot be persisted")
	}
	if err := checkpointFile.Close(); err != nil {
		return nil, filesystemError("prepare pull resume", "pull checkpoint cannot be closed")
	}
	journal, err = createRootedPrivateFile(
		temp,
		pullJournalName,
		os.O_RDWR|os.O_APPEND,
		nil,
	)
	if err != nil {
		return nil, filesystemError("prepare pull resume", "pull journal cannot be persisted")
	}
	lock, err = createRootedPrivateFile(temp, pullLockName, os.O_RDWR, nil)
	if err != nil {
		return nil, filesystemError("prepare pull resume", "pull lock cannot be persisted")
	}
	if err := lockOpenedPullState(lock); err != nil {
		return nil, pullLockError("prepare pull resume", err)
	}
	if err := temp.mkdir(pullDataName, 0o700, nil); err != nil {
		return nil, filesystemError(
			"prepare pull resume",
			"private pull data directory cannot be created",
		)
	}
	data, err = temp.openDirectoryRoot(pullDataName, nil)
	if err != nil {
		return nil, filesystemError(
			"prepare pull resume",
			"private pull data directory cannot be anchored",
		)
	}
	if err := data.chmod(0o700); err != nil {
		return nil, filesystemError(
			"prepare pull resume",
			"private pull data directory cannot be made private",
		)
	}
	dataStat, err := data.currentStat()
	if err != nil || validateOwnedPrivateDirectory(dataStat) != nil {
		return nil, filesystemError(
			"prepare pull resume",
			"private pull data directory is not private",
		)
	}
	if err := data.sync(); err != nil {
		return nil, filesystemError(
			"prepare pull resume",
			"private pull data directory cannot be synchronized",
		)
	}
	if err := temp.sync(); err != nil {
		return nil, filesystemError(
			"prepare pull resume",
			"temporary pull state cannot be synchronized",
		)
	}
	if err := c.resumeInitializationHook("scaffolding-synced"); err != nil {
		return nil, err
	}
	if err := parent.rename(tempName, parent, stagingName, true); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil, preconditionError(
				"prepare pull resume",
				"matching resumable state appeared during initialization",
			)
		}
		return nil, filesystemError(
			"prepare pull resume",
			"pull state cannot be atomically initialized",
		)
	}
	installed = true
	if err := parent.sync(); err != nil {
		return nil, filesystemError(
			"prepare pull resume",
			"destination parent cannot be synchronized",
		)
	}
	stagingStat, err := temp.currentStat()
	if err != nil || !parent.entryMatches(stagingName, stagingStat) {
		return nil, filesystemError(
			"prepare pull resume",
			"initialized pull state cannot be revalidated",
		)
	}
	stagingRoot := filepath.Join(parentPath, stagingName)
	resume := &pullResume{
		stagingRoot:    stagingRoot,
		dataRoot:       filepath.Join(stagingRoot, pullDataName),
		checkpointPath: filepath.Join(stagingRoot, pullCheckpointName),
		journalPath:    filepath.Join(stagingRoot, pullJournalName),
		lockPath:       filepath.Join(stagingRoot, pullLockName),
		lock:           lock,
		journal:        journal,
		created:        true,
		parent:         parent,
		staging:        temp,
		data:           data,
		stagingName:    stagingName,
		stagingStat:    stagingStat,
		dataStat:       dataStat,
		checkpoint:     checkpoint,
		completed:      make(map[completedPullChunk]struct{}),
		maxEntries:     pullCleanupEntryLimit(c.maxFiles, c.maxManifestBytes),
		maxStateBytes:  maxStateBytes,
		hooks:          c.filesystemHooks,
	}
	succeeded = true
	return resume, nil
}

func pullLockError(operation string, err error) error {
	switch {
	case errors.Is(err, errPullStateLocked):
		return preconditionError(operation, "another pull is already using the matching resumable state")
	case errors.Is(err, errPullLockUnsupported):
		return unsupportedError(operation, "safe process-lifetime pull locking is unavailable")
	default:
		return filesystemError(operation, "pull state cannot be locked")
	}
}

func openRootedPrivateFile(
	root *rootedDirectory,
	name string,
	flags int,
) (*os.File, rootedFileStat, error) {
	file, stat, err := root.openRegular(name, flags, 0, nil)
	if err != nil {
		return nil, rootedFileStat{}, err
	}
	if validateOwnedRegular(stat, 0o600) != nil {
		file.Close()
		return nil, rootedFileStat{}, syscall.EPERM
	}
	return file, stat, nil
}

func readRootedPrivateFile(
	root *rootedDirectory,
	name string,
	maxBytes uint64,
) ([]byte, error) {
	if maxBytes >= math.MaxInt64 {
		return nil, syscall.EFBIG
	}
	file, stat, err := openRootedPrivateFile(root, name, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if stat.size < 0 || uint64(stat.size) > maxBytes {
		return nil, syscall.EFBIG
	}
	return readBounded(file, int64(maxBytes))
}

func readPullIdentityAt(
	root *rootedDirectory,
	expected pullIdentity,
	maxBytes uint64,
	maxSelectors int,
	maxSelectorBytes uint64,
	pathLimits portablePathLimits,
) (bool, error) {
	body, err := readRootedPrivateFile(root, pullIdentityName, maxBytes)
	if isNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, integrityError(
			"prepare pull resume",
			"pull identity cannot be read safely",
		)
	}
	var identity pullIdentity
	if err := decodeStrictJSON(body, &identity); err != nil {
		return false, integrityError(
			"prepare pull resume",
			"pull identity is malformed",
		)
	}
	if err := validatePullIdentity(
		identity,
		maxSelectors,
		maxSelectorBytes,
		pathLimits,
	); err != nil {
		return false, err
	}
	canonical, err := json.Marshal(identity)
	if err != nil || !bytes.Equal(body, canonical) {
		return false, integrityError(
			"prepare pull resume",
			"pull identity is not canonically encoded",
		)
	}
	if !pullIdentityEqual(identity, expected) {
		return false, integrityError(
			"prepare pull resume",
			"pull identity does not match its staging path",
		)
	}
	return true, nil
}

func openRootedPullLock(root *rootedDirectory, create bool) (*os.File, error) {
	flags := os.O_RDWR
	var (
		lock *os.File
		err  error
	)
	if create {
		lock, err = createRootedPrivateFile(root, pullLockName, flags, nil)
	} else {
		lock, _, err = openRootedPrivateFile(root, pullLockName, flags)
	}
	if err != nil {
		return nil, err
	}
	if err := lockOpenedPullState(lock); err != nil {
		lock.Close()
		return nil, err
	}
	return lock, nil
}

func openOrCreatePullJournalAt(root *rootedDirectory) (*os.File, error) {
	journal, _, err := openRootedPrivateFile(
		root,
		pullJournalName,
		os.O_RDWR|os.O_APPEND,
	)
	if err == nil {
		return journal, nil
	}
	if !isNotExist(err) {
		return nil, filesystemError("prepare pull resume", "pull journal cannot be opened")
	}
	journal, err = createRootedPrivateFile(
		root,
		pullJournalName,
		os.O_RDWR|os.O_APPEND,
		nil,
	)
	if err != nil {
		return nil, filesystemError("prepare pull resume", "pull journal cannot be created")
	}
	if err := root.sync(); err != nil {
		journal.Close()
		return nil, filesystemError(
			"prepare pull resume",
			"pull staging cannot be synchronized",
		)
	}
	return journal, nil
}

func (c *Client) openExistingPullResume(
	parent *rootedDirectory,
	parentPath string,
	stagingName string,
	expectedStaging rootedFileStat,
	identity pullIdentity,
	expected map[completedPullChunk]struct{},
	maxStateBytes uint64,
) (*pullResume, error) {
	maxIdentityBytes, err := pullIdentitySizeLimit(identity)
	if err != nil {
		return nil, err
	}
	staging, err := parent.openDirectoryRoot(stagingName, nil)
	if err != nil {
		return nil, filesystemError("prepare pull resume", "pull staging cannot be anchored")
	}
	succeeded := false
	var data *rootedDirectory
	var lock, journal *os.File
	defer func() {
		if succeeded {
			return
		}
		if journal != nil {
			_ = journal.Close()
		}
		if lock != nil {
			_ = closePullStateLock(lock)
		}
		if data != nil {
			_ = data.close()
		}
		_ = staging.close()
	}()
	stagingStat, err := staging.currentStat()
	if err != nil ||
		!sameRootedObject(expectedStaging, stagingStat) ||
		validateOwnedPrivateDirectory(stagingStat) != nil {
		return nil, integrityError(
			"prepare pull resume",
			"existing pull staging is not the expected private directory",
		)
	}
	lock, err = openRootedPullLock(staging, false)
	if err != nil {
		return nil, pullLockError("prepare pull resume", err)
	}
	hasIdentity, err := readPullIdentityAt(
		staging,
		identity,
		maxIdentityBytes,
		c.maxFiles,
		c.maxManifestBytes,
		c.effectivePortablePathLimits(),
	)
	if err != nil {
		return nil, err
	}
	checkpoint, completed, err := readPullCheckpointAt(
		staging,
		identity,
		expected,
		c.pullStateLimits(),
	)
	if err != nil {
		return nil, err
	}
	if !hasIdentity {
		body, marshalErr := json.Marshal(identity)
		if marshalErr != nil {
			return nil, protocolError("prepare pull resume", "pull identity cannot be encoded")
		}
		identityFile, createErr := createRootedPrivateFile(
			staging,
			pullIdentityName,
			os.O_RDWR,
			body,
		)
		if createErr != nil {
			return nil, filesystemError(
				"prepare pull resume",
				"legacy pull identity cannot be persisted",
			)
		}
		if closeErr := identityFile.Close(); closeErr != nil {
			return nil, filesystemError(
				"prepare pull resume",
				"legacy pull identity cannot be closed",
			)
		}
	}
	data, err = staging.openDirectoryRoot(pullDataName, nil)
	if err != nil {
		return nil, integrityError(
			"prepare pull resume",
			"private pull data directory is unavailable",
		)
	}
	dataStat, err := data.currentStat()
	if err != nil || validateOwnedPrivateDirectory(dataStat) != nil {
		return nil, integrityError(
			"prepare pull resume",
			"private pull data directory is not private",
		)
	}
	if err := removeCheckpointTempsAt(staging); err != nil {
		return nil, err
	}
	journal, err = openOrCreatePullJournalAt(staging)
	if err != nil {
		return nil, err
	}
	stagingRoot := filepath.Join(parentPath, stagingName)
	resume := &pullResume{
		stagingRoot:    stagingRoot,
		dataRoot:       filepath.Join(stagingRoot, pullDataName),
		checkpointPath: filepath.Join(stagingRoot, pullCheckpointName),
		journalPath:    filepath.Join(stagingRoot, pullJournalName),
		lockPath:       filepath.Join(stagingRoot, pullLockName),
		lock:           lock,
		journal:        journal,
		parent:         parent,
		staging:        staging,
		data:           data,
		stagingName:    stagingName,
		stagingStat:    stagingStat,
		dataStat:       dataStat,
		checkpoint:     checkpoint,
		completed:      completed,
		maxEntries:     pullCleanupEntryLimit(c.maxFiles, c.maxManifestBytes),
		maxStateBytes:  maxStateBytes,
		hooks:          c.filesystemHooks,
	}
	if err := resume.persistCheckpointLocked(); err != nil {
		return nil, err
	}
	if err := resume.resetJournalLocked(); err != nil {
		return nil, err
	}
	stagingStat, err = staging.currentStat()
	if err != nil {
		return nil, filesystemError("prepare pull resume", "pull staging cannot be inspected")
	}
	resume.stagingStat = stagingStat
	succeeded = true
	return resume, nil
}
