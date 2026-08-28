package volume

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"sort"
	"strings"
	"time"
)

func (c *Client) removeMatchingPullStateAt(
	parent *rootedDirectory,
	stagingName string,
	expectedStat rootedFileStat,
	identity pullIdentity,
) error {
	maxIdentityBytes, err := pullIdentitySizeLimit(identity)
	if err != nil {
		return err
	}
	staging, err := parent.openDirectoryRoot(stagingName, nil)
	if err != nil {
		return filesystemError("restart pull", "matching pull staging cannot be anchored")
	}
	stagingStat, err := staging.currentStat()
	if err != nil ||
		!sameRootedObject(expectedStat, stagingStat) ||
		validateOwnedPrivateDirectory(stagingStat) != nil {
		staging.close()
		return integrityError(
			"restart pull",
			"matching pull staging is not the expected private directory",
		)
	}
	lock, err := openRootedPullLock(staging, false)
	if err != nil {
		staging.close()
		return pullLockError("restart pull", err)
	}
	proven, err := readPullIdentityAt(
		staging,
		identity,
		maxIdentityBytes,
		c.maxFiles,
		c.maxManifestBytes,
		c.effectivePortablePathLimits(),
	)
	if err != nil {
		closePullStateLock(lock)
		staging.close()
		return err
	}
	if !proven {
		checkpoint, decodeErr := decodePullCheckpointAt(
			staging,
			c.maxManifestBytes+pullStateOverheadBytes,
			c.pullStateLimits(),
		)
		if decodeErr != nil || !pullIdentityEqual(checkpoint.Identity, identity) {
			closePullStateLock(lock)
			staging.close()
			return integrityError(
				"restart pull",
				"matching pull state ownership and path identity cannot be proven",
			)
		}
	}
	if err := staging.close(); err != nil {
		closePullStateLock(lock)
		return filesystemError("restart pull", "matching pull staging cannot be closed")
	}
	if !parent.entryMatches(stagingName, stagingStat) {
		closePullStateLock(lock)
		return integrityError("restart pull", "matching pull state changed before removal")
	}
	if err := parent.removeTree(
		stagingName,
		stagingStat,
		pullCleanupEntryLimit(c.maxFiles, c.maxManifestBytes),
	); err != nil {
		closePullStateLock(lock)
		return filesystemError("restart pull", "matching resumable state cannot be removed safely")
	}
	if err := closePullStateLock(lock); err != nil {
		return filesystemError("restart pull", "matching pull state cannot be unlocked")
	}
	if err := parent.sync(); err != nil {
		return filesystemError("restart pull", "destination parent cannot be synchronized")
	}
	return nil
}

func validPullInitName(name string) bool {
	if !strings.HasPrefix(name, pullInitPrefix) || !strings.HasSuffix(name, pullInitSuffix) {
		return false
	}
	value := strings.TrimSuffix(strings.TrimPrefix(name, pullInitPrefix), pullInitSuffix)
	parts := strings.Split(value, "-")
	if len(parts) != 2 || len(parts[0]) != 64 || len(parts[1]) != 32 {
		return false
	}
	for _, part := range parts {
		if _, err := hex.DecodeString(part); err != nil || strings.ToLower(part) != part {
			return false
		}
	}
	return true
}

func pullInitStorageKey(name string) (string, bool) {
	if !validPullInitName(name) {
		return "", false
	}
	value := strings.TrimSuffix(strings.TrimPrefix(name, pullInitPrefix), pullInitSuffix)
	return value[:64], true
}

func validPullInitializationScaffold(names []string) bool {
	present := make(map[string]bool, len(names))
	for _, name := range names {
		switch name {
		case pullIdentityName,
			pullCheckpointName,
			pullJournalName,
			pullLockName,
			pullDataName:
			present[name] = true
		default:
			return false
		}
	}
	requiredCount := 0
	switch {
	case present[pullDataName]:
		requiredCount = 5
	case present[pullLockName]:
		requiredCount = 4
	case present[pullJournalName]:
		requiredCount = 3
	case present[pullCheckpointName]:
		requiredCount = 2
	case present[pullIdentityName]:
		requiredCount = 1
	}
	if len(names) != requiredCount {
		return false
	}
	required := []string{
		pullIdentityName,
		pullCheckpointName,
		pullJournalName,
		pullLockName,
		pullDataName,
	}
	for _, name := range required[:requiredCount] {
		if !present[name] {
			return false
		}
	}
	return true
}

func readStalePullInitializationFile(
	root *rootedDirectory,
	name string,
	maxBytes uint64,
	cutoff time.Time,
) ([]byte, rootedFileStat, bool) {
	if maxBytes >= math.MaxInt64 {
		return nil, rootedFileStat{}, false
	}
	file, stat, err := openRootedPrivateFile(root, name, os.O_RDONLY)
	if err != nil ||
		stat.size < 0 ||
		uint64(stat.size) > maxBytes ||
		time.Unix(0, stat.modified).After(cutoff) {
		if file != nil {
			file.Close()
		}
		return nil, rootedFileStat{}, false
	}
	body, readErr := readBounded(file, int64(maxBytes))
	after, statErr := rootedFileStatFromFile(file)
	closeErr := file.Close()
	path, pathErr := root.lstat(name, nil)
	if readErr != nil ||
		statErr != nil ||
		closeErr != nil ||
		pathErr != nil ||
		!sameRootedSnapshot(stat, after) ||
		!sameRootedSnapshot(after, path) {
		return nil, rootedFileStat{}, false
	}
	return body, after, true
}

func sameSortedNames(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func (c *Client) removeStalePullInitializationAt(
	parent *rootedDirectory,
	name string,
	parentStat rootedFileStat,
	staging *rootedDirectory,
	now time.Time,
) bool {
	cutoff := now.Add(-pullStaleAfter)
	if time.Unix(0, parentStat.modified).After(cutoff) {
		staging.close()
		return false
	}
	key, validName := pullInitStorageKey(name)
	if !validName {
		staging.close()
		return false
	}
	names, err := staging.readDirNames("", nil, 5)
	if err != nil || !validPullInitializationScaffold(names) {
		staging.close()
		return false
	}

	maxStateBytes := c.maxManifestBytes + pullStateOverheadBytes
	fileStats := make(map[string]rootedFileStat, 3)
	fileBodies := make(map[string][]byte, 3)
	for _, stateName := range []string{
		pullIdentityName,
		pullCheckpointName,
		pullJournalName,
	} {
		if !slicesContain(names, stateName) {
			continue
		}
		body, stat, ok := readStalePullInitializationFile(
			staging,
			stateName,
			maxStateBytes,
			cutoff,
		)
		if !ok {
			staging.close()
			return false
		}
		fileStats[stateName] = stat
		fileBodies[stateName] = body
	}

	var identity pullIdentity
	if body, ok := fileBodies[pullIdentityName]; ok {
		if decodeStrictJSON(body, &identity) != nil ||
			identity.FormatVersion != pullCheckpointVersion ||
			validatePullIdentity(
				identity,
				c.maxFiles,
				c.maxManifestBytes,
				c.effectivePortablePathLimits(),
			) != nil {
			staging.close()
			return false
		}
		canonical, marshalErr := json.Marshal(identity)
		normalized := append([]string(nil), identity.Selectors...)
		sort.Strings(normalized)
		normalized = compactSortedStrings(normalized)
		normalizedIdentity := identity
		normalizedIdentity.Selectors = normalized
		derivedKey, keyErr := c.pullStorageKey(identity)
		if marshalErr != nil ||
			!bytes.Equal(body, canonical) ||
			!pullIdentityEqual(identity, normalizedIdentity) ||
			keyErr != nil ||
			derivedKey != key {
			staging.close()
			return false
		}
	}
	if body, ok := fileBodies[pullCheckpointName]; ok {
		checkpoint, decodeErr := decodePullCheckpointBody(body, c.pullStateLimits())
		cutoffUnixSeconds := cutoff.Unix()
		if decodeErr != nil ||
			cutoffUnixSeconds < 0 ||
			checkpoint.Version != pullCheckpointVersion ||
			!pullIdentityEqual(checkpoint.Identity, identity) ||
			checkpoint.CreatedAtUnixSeconds == 0 ||
			checkpoint.UpdatedAtUnixSeconds != checkpoint.CreatedAtUnixSeconds ||
			checkpoint.UpdatedAtUnixSeconds > uint64(cutoffUnixSeconds) ||
			len(checkpoint.CompletedChunks) != 0 {
			staging.close()
			return false
		}
	}
	if body, ok := fileBodies[pullJournalName]; ok && len(body) != 0 {
		staging.close()
		return false
	}

	var dataStat rootedFileStat
	hasData := slicesContain(names, pullDataName)
	if hasData {
		data, openErr := staging.openDirectoryRoot(pullDataName, nil)
		if openErr != nil {
			staging.close()
			return false
		}
		dataStat, err = data.currentStat()
		dataNames, readErr := data.readDirNames("", nil, 0)
		closeErr := data.close()
		if err != nil ||
			validateOwnedPrivateDirectory(dataStat) != nil ||
			time.Unix(0, dataStat.modified).After(cutoff) ||
			readErr != nil ||
			len(dataNames) != 0 ||
			closeErr != nil {
			staging.close()
			return false
		}
	}

	var lock *os.File
	if slicesContain(names, pullLockName) {
		lock, err = openRootedPullLock(staging, false)
		if err != nil {
			staging.close()
			return false
		}
		lockStat, statErr := rootedFileStatFromFile(lock)
		lockPath, pathErr := staging.lstat(pullLockName, nil)
		if statErr != nil ||
			pathErr != nil ||
			!sameRootedSnapshot(lockStat, lockPath) ||
			time.Unix(0, lockStat.modified).After(cutoff) {
			closePullStateLock(lock)
			staging.close()
			return false
		}
		fileStats[pullLockName] = lockStat
	}

	currentNames, err := staging.readDirNames("", nil, 5)
	if err != nil || !sameSortedNames(names, currentNames) {
		closePullStateLock(lock)
		staging.close()
		return false
	}
	for stateName, expected := range fileStats {
		current, statErr := staging.lstat(stateName, nil)
		if statErr != nil || !sameRootedSnapshot(expected, current) {
			closePullStateLock(lock)
			staging.close()
			return false
		}
	}
	if hasData {
		data, openErr := staging.openDirectoryRoot(pullDataName, nil)
		if openErr != nil {
			closePullStateLock(lock)
			staging.close()
			return false
		}
		current, statErr := data.currentStat()
		dataNames, readErr := data.readDirNames("", nil, 0)
		closeErr := data.close()
		if statErr != nil ||
			!sameRootedSnapshot(dataStat, current) ||
			readErr != nil ||
			len(dataNames) != 0 ||
			closeErr != nil {
			closePullStateLock(lock)
			staging.close()
			return false
		}
	}
	currentRoot, err := staging.currentStat()
	if err != nil || !sameRootedSnapshot(parentStat, currentRoot) {
		closePullStateLock(lock)
		staging.close()
		return false
	}
	if err := staging.close(); err != nil {
		closePullStateLock(lock)
		return false
	}
	parentEntry, err := parent.lstat(name, nil)
	if err != nil || !sameRootedSnapshot(parentStat, parentEntry) {
		closePullStateLock(lock)
		return false
	}
	if err := parent.removeTree(name, parentStat, 5); err != nil {
		closePullStateLock(lock)
		return false
	}
	if err := closePullStateLock(lock); err != nil {
		return false
	}
	return true
}

func slicesContain(values []string, value string) bool {
	index := sort.SearchStrings(values, value)
	return index < len(values) && values[index] == value
}

func (c *Client) removeStalePullStatesAt(
	parent *rootedDirectory,
	_ string,
	current string,
) {
	names, err := parent.readDirNamesAtMost("", nil, pullMaxStaleScan)
	if err != nil {
		return
	}
	now := time.Now()
	removed := 0
	for _, name := range names {
		if removed >= pullMaxStaleRemovals {
			break
		}
		if name == current {
			continue
		}
		deterministic := validPullStagingName(name)
		temporary := validPullInitName(name)
		if !deterministic && !temporary {
			continue
		}
		stat, err := parent.lstat(name, nil)
		if err != nil || !stat.mode.IsDir() || validateOwnedPrivateDirectory(stat) != nil {
			continue
		}
		staging, err := parent.openDirectoryRoot(name, nil)
		if err != nil {
			continue
		}
		opened, err := staging.currentStat()
		if err != nil || !sameRootedObject(stat, opened) {
			staging.close()
			continue
		}
		if temporary {
			if c.removeStalePullInitializationAt(parent, name, stat, staging, now) {
				removed++
			}
			continue
		}
		lastModified := time.Unix(0, stat.modified)
		for _, stateName := range []string{
			pullIdentityName,
			pullCheckpointName,
			pullJournalName,
		} {
			state, stateErr := staging.lstat(stateName, nil)
			if stateErr == nil && state.mode.IsRegular() {
				modified := time.Unix(0, state.modified)
				if modified.After(lastModified) {
					lastModified = modified
				}
			}
		}
		if now.Sub(lastModified) < pullStaleAfter {
			staging.close()
			continue
		}
		body, readErr := readRootedPrivateFile(
			staging,
			pullIdentityName,
			c.maxManifestBytes+pullStateOverheadBytes,
		)
		var identity pullIdentity
		switch {
		case readErr != nil && deterministic && isNotExist(readErr):
			checkpoint, checkpointErr := decodePullCheckpointAt(
				staging,
				c.maxManifestBytes+pullStateOverheadBytes,
				c.pullStateLimits(),
			)
			if checkpointErr != nil || checkpoint.Version != pullCheckpointVersion {
				staging.close()
				continue
			}
			identity = checkpoint.Identity
		case readErr != nil:
			staging.close()
			continue
		default:
			decodeErr := decodeStrictJSON(body, &identity)
			identityErr := validatePullIdentity(
				identity,
				c.maxFiles,
				c.maxManifestBytes,
				c.effectivePortablePathLimits(),
			)
			canonical, canonicalErr := json.Marshal(identity)
			if decodeErr != nil ||
				identityErr != nil ||
				canonicalErr != nil ||
				!bytes.Equal(body, canonical) {
				staging.close()
				continue
			}
		}
		key, err := c.pullStorageKey(identity)
		if err != nil ||
			deterministic && name != pullStagingPrefix+key+pullStagingSuffix {
			staging.close()
			continue
		}
		lock, err := openRootedPullLock(staging, false)
		if err != nil {
			staging.close()
			continue
		}
		if err := staging.close(); err != nil {
			closePullStateLock(lock)
			continue
		}
		if !parent.entryMatches(name, stat) {
			closePullStateLock(lock)
			continue
		}
		if parent.removeTree(
			name,
			stat,
			pullCleanupEntryLimit(c.maxFiles, c.maxManifestBytes),
		) == nil {
			removed++
		}
		_ = closePullStateLock(lock)
	}
	if removed > 0 {
		_ = parent.sync()
	}
}

func validPullStagingName(name string) bool {
	if !strings.HasPrefix(name, pullStagingPrefix) ||
		!strings.HasSuffix(name, pullStagingSuffix) {
		return false
	}
	key := strings.TrimSuffix(strings.TrimPrefix(name, pullStagingPrefix), pullStagingSuffix)
	if len(key) != 64 {
		return false
	}
	_, err := hex.DecodeString(key)
	return err == nil && strings.ToLower(key) == key
}
