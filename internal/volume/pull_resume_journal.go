package volume

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// pullCheckpointSizeLimit bounds either a checkpoint snapshot or its journal
// from the already validated identity and expected chunk set. Marshaling the
// widest header keeps selector and destination encoding in the bound.
func pullCheckpointSizeLimit(
	identity pullIdentity,
	expected map[completedPullChunk]struct{},
	maximumBytes ...uint64,
) (uint64, error) {
	if len(maximumBytes) > 1 {
		return 0, preconditionError(
			"read pull checkpoint",
			"pull resume metadata exceeds its byte limit",
		)
	}
	maxAllowedBytes := uint64((^uint(0) >> 1) - 1)
	if len(maximumBytes) == 1 {
		maxAllowedBytes = min(maxAllowedBytes, maximumBytes[0])
	}
	header, err := json.Marshal(pullCheckpoint{
		Version:              pullCheckpointVersion,
		Identity:             identity,
		CreatedAtUnixSeconds: math.MaxUint64,
		UpdatedAtUnixSeconds: math.MaxUint64,
		CompletedChunks:      []completedPullChunk{},
	})
	if err != nil {
		return 0, protocolError(
			"read pull checkpoint",
			"pull checkpoint header cannot be encoded",
		)
	}
	maxBytes := uint64(len(header))
	if maxBytes > maxAllowedBytes {
		return 0, preconditionError(
			"read pull checkpoint",
			"pull resume metadata exceeds its byte limit",
		)
	}
	for chunk := range expected {
		encoded, err := json.Marshal(chunk)
		if err != nil {
			return 0, protocolError(
				"read pull checkpoint",
				"expected chunk cannot be encoded",
			)
		}
		recordBytes, overflow := addUint64(uint64(len(encoded)), 1)
		if overflow {
			return 0, preconditionError(
				"read pull checkpoint",
				"pull resume metadata exceeds its byte limit",
			)
		}
		var exceeded bool
		maxBytes, exceeded = addPullStateBytes(maxBytes, recordBytes, maxAllowedBytes)
		if exceeded {
			return 0, preconditionError(
				"read pull checkpoint",
				"pull resume metadata exceeds its byte limit",
			)
		}
	}
	return maxBytes, nil
}

func addPullStateBytes(current, additional, maximum uint64) (uint64, bool) {
	next, overflow := addUint64(current, additional)
	return next, overflow || next > maximum
}

func pullIdentitySizeLimit(identity pullIdentity) (uint64, error) {
	encoded, err := json.Marshal(identity)
	if err != nil {
		return 0, protocolError(
			"read pull identity",
			"pull identity cannot be encoded",
		)
	}
	size := uint64(len(encoded))
	if size > uint64((^uint(0)>>1)-1) {
		return 0, preconditionError(
			"read pull identity",
			"pull identity exceeds its platform byte limit",
		)
	}
	return size, nil
}

func decodePullCheckpointBody(
	body []byte,
	configuredLimits ...pullStateLimits,
) (pullCheckpoint, error) {
	var checkpoint pullCheckpoint
	if err := decodeStrictJSON(body, &checkpoint); err != nil {
		return pullCheckpoint{}, integrityError(
			"read pull checkpoint",
			"pull checkpoint contains malformed JSON",
		)
	}
	limits := selectedPullStateLimits(configuredLimits)
	if err := validatePullIdentity(
		checkpoint.Identity,
		limits.maxSelectors,
		limits.maxSelectorBytes,
		limits.pathLimits,
	); err != nil {
		return pullCheckpoint{}, err
	}
	for _, chunk := range checkpoint.CompletedChunks {
		if err := validatePortablePath(chunk.Path, limits.pathLimits); err != nil {
			return pullCheckpoint{}, integrityError(
				"read pull checkpoint",
				"pull checkpoint contains an invalid chunk path",
			)
		}
	}
	canonical := checkpoint
	canonical.Identity.Selectors = make([]string, len(checkpoint.Identity.Selectors))
	copy(canonical.Identity.Selectors, checkpoint.Identity.Selectors)
	sort.Strings(canonical.Identity.Selectors)
	canonical.Identity.Selectors = compactSortedStrings(canonical.Identity.Selectors)
	canonical.CompletedChunks = make(
		[]completedPullChunk,
		len(checkpoint.CompletedChunks),
	)
	copy(canonical.CompletedChunks, checkpoint.CompletedChunks)
	sortCompletedPullChunks(canonical.CompletedChunks)
	canonicalBody, err := json.Marshal(canonical)
	if err != nil {
		return pullCheckpoint{}, protocolError(
			"read pull checkpoint",
			"pull checkpoint cannot be encoded",
		)
	}
	if !bytes.Equal(body, canonicalBody) {
		return pullCheckpoint{}, integrityError(
			"read pull checkpoint",
			"pull checkpoint is not canonically encoded",
		)
	}
	return checkpoint, nil
}

func decodePullCheckpointAt(
	root *rootedDirectory,
	maxBytes uint64,
	configuredLimits ...pullStateLimits,
) (pullCheckpoint, error) {
	body, err := readRootedPrivateFile(root, pullCheckpointName, maxBytes)
	if err != nil {
		if isNotExist(err) {
			return pullCheckpoint{}, filesystemError(
				"read pull checkpoint",
				"private pull checkpoint is unavailable",
			)
		}
		return pullCheckpoint{}, integrityError(
			"read pull checkpoint",
			"private pull checkpoint cannot be read safely",
		)
	}
	return decodePullCheckpointBody(body, configuredLimits...)
}

func readPullJournalAt(
	root *rootedDirectory,
	maxBytes uint64,
	expected map[completedPullChunk]struct{},
	completed map[completedPullChunk]struct{},
	configuredLimits ...pullStateLimits,
) error {
	return readPullJournalNamedAt(
		root,
		pullJournalName,
		maxBytes,
		expected,
		completed,
		configuredLimits...,
	)
}

func readPullJournalNamedAt(
	root *rootedDirectory,
	name string,
	maxBytes uint64,
	expected map[completedPullChunk]struct{},
	completed map[completedPullChunk]struct{},
	configuredLimits ...pullStateLimits,
) error {
	if maxBytes >= math.MaxInt64 {
		return preconditionError(
			"read pull journal",
			"pull journal size limit is too large for this platform",
		)
	}
	file, stat, err := openRootedPrivateFile(root, name, os.O_RDONLY)
	if isNotExist(err) {
		return nil
	}
	if err != nil {
		return integrityError("read pull journal", "private pull journal cannot be opened safely")
	}
	defer file.Close()
	if stat.size < 0 || uint64(stat.size) > maxBytes {
		return integrityError("read pull journal", "pull journal exceeds its size limit")
	}
	scanner, err := newMetadataStreamScanner(file, maxBytes, uint64(len(expected))+1)
	if err != nil {
		return integrityError("read pull journal", "pull journal limits are invalid")
	}
	journalSeen := make(map[completedPullChunk]struct{})
	locations := make(map[completedPullChunkLocation]completedPullChunk, len(completed))
	for chunk := range completed {
		location := chunk.location()
		if existing, duplicate := locations[location]; duplicate && existing != chunk {
			return integrityError(
				"read pull journal",
				"pull checkpoint contains conflicting chunks",
			)
		}
		locations[location] = chunk
	}
	var pathBytes uint64
	limits := selectedPullStateLimits(configuredLimits)
	for {
		line, done, scanErr := scanner.next()
		if scanErr != nil {
			if errors.Is(scanErr, errMetadataMissingNewline) {
				break
			}
			return integrityError("read pull journal", scanErr.Error())
		}
		if done {
			break
		}
		var chunk completedPullChunk
		if err := decodeStrictJSON(line, &chunk); err != nil {
			return integrityError("read pull journal", "pull journal contains malformed JSON")
		}
		canonical, err := json.Marshal(chunk)
		if err != nil {
			return protocolError("read pull journal", "pull journal record cannot be encoded")
		}
		if !bytes.Equal(line, canonical) {
			return integrityError(
				"read pull journal",
				"pull journal record is not canonically encoded",
			)
		}
		if err := validatePortablePath(chunk.Path, limits.pathLimits); err != nil {
			return integrityError(
				"read pull journal",
				"pull journal contains an invalid chunk path",
			)
		}
		nextPathBytes, overflow := addUint64(pathBytes, uint64(len(chunk.Path)))
		if overflow || nextPathBytes > maxBytes {
			return integrityError(
				"read pull journal",
				"pull journal path bytes exceed their limit",
			)
		}
		pathBytes = nextPathBytes
		if _, ok := expected[chunk]; !ok {
			return integrityError("read pull journal", "pull journal contains an unexpected chunk")
		}
		if _, duplicate := journalSeen[chunk]; duplicate {
			return integrityError("read pull journal", "pull journal contains a duplicate chunk")
		}
		journalSeen[chunk] = struct{}{}
		if existing, duplicate := locations[chunk.location()]; duplicate && existing != chunk {
			return integrityError("read pull journal", "pull journal contains a conflicting chunk")
		}
		locations[chunk.location()] = chunk
		if _, checkpointed := completed[chunk]; checkpointed {
			continue
		}
		completed[chunk] = struct{}{}
	}
	return nil
}

func readPullCheckpointAt(
	root *rootedDirectory,
	identity pullIdentity,
	expected map[completedPullChunk]struct{},
	configuredLimits ...pullStateLimits,
) (pullCheckpoint, map[completedPullChunk]struct{}, error) {
	maxBytes, err := pullCheckpointSizeLimit(identity, expected)
	if err != nil {
		return pullCheckpoint{}, nil, err
	}
	checkpoint, err := decodePullCheckpointAt(root, maxBytes, configuredLimits...)
	if err != nil {
		return pullCheckpoint{}, nil, err
	}
	if checkpoint.Version != pullCheckpointVersion {
		return pullCheckpoint{}, nil, integrityError(
			"read pull checkpoint",
			"pull checkpoint version is unsupported",
		)
	}
	if !pullIdentityEqual(checkpoint.Identity, identity) {
		return pullCheckpoint{}, nil, integrityError(
			"read pull checkpoint",
			"pull checkpoint identity does not match its staging path",
		)
	}
	if checkpoint.CreatedAtUnixSeconds == 0 ||
		checkpoint.UpdatedAtUnixSeconds < checkpoint.CreatedAtUnixSeconds {
		return pullCheckpoint{}, nil, integrityError(
			"read pull checkpoint",
			"pull checkpoint timestamps are invalid",
		)
	}
	completed := make(map[completedPullChunk]struct{}, len(checkpoint.CompletedChunks))
	locations := make(map[completedPullChunkLocation]completedPullChunk, len(checkpoint.CompletedChunks))
	for _, chunk := range checkpoint.CompletedChunks {
		if _, ok := expected[chunk]; !ok {
			return pullCheckpoint{}, nil, integrityError(
				"read pull checkpoint",
				"pull checkpoint contains an unexpected chunk",
			)
		}
		if _, duplicate := completed[chunk]; duplicate {
			return pullCheckpoint{}, nil, integrityError(
				"read pull checkpoint",
				"pull checkpoint contains a duplicate chunk",
			)
		}
		if existing, duplicate := locations[chunk.location()]; duplicate && existing != chunk {
			return pullCheckpoint{}, nil, integrityError(
				"read pull checkpoint",
				"pull checkpoint contains conflicting chunks",
			)
		}
		completed[chunk] = struct{}{}
		locations[chunk.location()] = chunk
	}
	if err := readPullJournalAt(
		root,
		maxBytes,
		expected,
		completed,
		configuredLimits...,
	); err != nil {
		return pullCheckpoint{}, nil, err
	}
	checkpoint.CompletedChunks = checkpoint.CompletedChunks[:0]
	for chunk := range completed {
		checkpoint.CompletedChunks = append(checkpoint.CompletedChunks, chunk)
	}
	sortCompletedPullChunks(checkpoint.CompletedChunks)
	return checkpoint, completed, nil
}

func removeCheckpointTempsAt(root *rootedDirectory) error {
	names, err := root.readDirNames("", nil, 64)
	if err != nil {
		return filesystemError("prepare pull resume", "pull staging cannot be read")
	}
	prefix := "." + pullCheckpointName + ".tmp-"
	for _, name := range names {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		file, _, err := openRootedPrivateFile(root, name, os.O_RDONLY)
		if err != nil {
			return integrityError(
				"prepare pull resume",
				"temporary pull checkpoint is not a private regular file",
			)
		}
		if err := file.Close(); err != nil {
			return filesystemError(
				"prepare pull resume",
				"temporary pull checkpoint cannot be closed",
			)
		}
		if err := root.remove(name, false, nil); err != nil {
			return filesystemError(
				"prepare pull resume",
				"temporary pull checkpoint cannot be removed",
			)
		}
	}
	return nil
}

func readPullJournal(
	path string,
	maxBytes uint64,
	expected map[completedPullChunk]struct{},
	completed map[completedPullChunk]struct{},
) error {
	root, err := openRootedDirectory(filepath.Dir(path))
	if err != nil {
		return filesystemError("read pull journal", "pull journal parent cannot be opened")
	}
	defer root.close()
	return readPullJournalNamedAt(
		root,
		filepath.ToSlash(filepath.Base(path)),
		maxBytes,
		expected,
		completed,
	)
}

func (resume *pullResume) contains(chunk completedPullChunk) bool {
	resume.mu.Lock()
	defer resume.mu.Unlock()
	_, ok := resume.completed[chunk]
	return ok
}

func (resume *pullResume) markCompleted(chunk completedPullChunk) error {
	resume.mu.Lock()
	defer resume.mu.Unlock()
	if _, exists := resume.completed[chunk]; exists {
		return nil
	}
	body, err := json.Marshal(chunk)
	if err != nil {
		return protocolError("write pull journal", "completed chunk cannot be encoded")
	}
	body = append(body, '\n')
	maxStateBytes := resume.maxStateBytes
	if maxStateBytes == 0 {
		maxStateBytes = defaultMaxManifestBytes + pullStateOverheadBytes
	}
	nextJournalBytes, overflow := addUint64(resume.journalBytes, uint64(len(body)))
	if overflow || nextJournalBytes > maxStateBytes {
		return preconditionError(
			"write pull journal",
			"pull journal exceeds its byte limit",
		)
	}
	written, err := resume.journal.Write(body)
	if err != nil || written != len(body) {
		return filesystemError("write pull journal", "completed chunk cannot be appended")
	}
	if err := resume.journal.Sync(); err != nil {
		return filesystemError("write pull journal", "pull journal cannot be synchronized")
	}
	resume.journalBytes = nextJournalBytes
	resume.completed[chunk] = struct{}{}
	now, err := currentUnixSeconds()
	if err != nil {
		return err
	}
	resume.checkpoint.UpdatedAtUnixSeconds = now
	return nil
}

func sortCompletedPullChunks(chunks []completedPullChunk) {
	sort.Slice(chunks, func(left, right int) bool {
		switch {
		case chunks[left].Path != chunks[right].Path:
			return chunks[left].Path < chunks[right].Path
		case chunks[left].Offset != chunks[right].Offset:
			return chunks[left].Offset < chunks[right].Offset
		case chunks[left].Length != chunks[right].Length:
			return chunks[left].Length < chunks[right].Length
		default:
			return chunks[left].Digest.Hex() < chunks[right].Digest.Hex()
		}
	})
}

func (resume *pullResume) persistCheckpointLocked() error {
	body, err := json.Marshal(resume.checkpoint)
	if err != nil {
		return protocolError("write pull checkpoint", "pull checkpoint cannot be encoded")
	}
	if resume.staging == nil {
		return filesystemError("write pull checkpoint", "anchored pull staging is unavailable")
	}
	maxStateBytes := resume.maxStateBytes
	if maxStateBytes == 0 {
		maxStateBytes = defaultMaxManifestBytes + pullStateOverheadBytes
	}
	if uint64(len(body)) > maxStateBytes {
		return preconditionError(
			"write pull checkpoint",
			"pull checkpoint exceeds its byte limit",
		)
	}
	return resume.persistCheckpointAt(body)
}

func (resume *pullResume) journalCompactionHook(stage string) error {
	if resume.hooks == nil || resume.hooks.duringJournalCompaction == nil {
		return nil
	}
	if err := resume.hooks.duringJournalCompaction(stage); err != nil {
		return filesystemError(
			"compact pull journal",
			"pull journal compaction was interrupted",
		)
	}
	return nil
}

func (resume *pullResume) persistCheckpointAt(body []byte) error {
	var tempName string
	var temp *os.File
	for range 10 {
		token, err := newCorrelationID()
		if err != nil {
			return filesystemError(
				"write pull checkpoint",
				"temporary checkpoint identity cannot be created",
			)
		}
		tempName = "." + pullCheckpointName + ".tmp-" + token
		temp, err = createRootedPrivateFile(
			resume.staging,
			tempName,
			os.O_RDWR,
			body,
		)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrExist) {
			return filesystemError(
				"write pull checkpoint",
				"temporary pull checkpoint cannot be created",
			)
		}
	}
	if temp == nil {
		return filesystemError(
			"write pull checkpoint",
			"unique temporary pull checkpoint cannot be created",
		)
	}
	renamed := false
	defer func() {
		_ = temp.Close()
		if !renamed {
			_ = resume.staging.remove(tempName, false, nil)
		}
	}()
	if err := temp.Close(); err != nil {
		return filesystemError(
			"write pull checkpoint",
			"temporary pull checkpoint cannot be closed",
		)
	}
	if err := resume.journalCompactionHook("before-checkpoint-replace"); err != nil {
		return err
	}
	if err := resume.staging.rename(
		tempName,
		resume.staging,
		pullCheckpointName,
		false,
	); err != nil {
		return filesystemError(
			"write pull checkpoint",
			"pull checkpoint cannot be atomically replaced",
		)
	}
	renamed = true
	if err := resume.staging.sync(); err != nil {
		return filesystemError(
			"write pull checkpoint",
			"pull staging cannot be synchronized",
		)
	}
	if err := resume.journalCompactionHook("checkpoint-replaced"); err != nil {
		return err
	}
	return nil
}

func (resume *pullResume) resetJournalLocked() error {
	if err := resume.journalCompactionHook("before-journal-reset"); err != nil {
		return err
	}
	if err := resume.journal.Truncate(0); err != nil {
		return filesystemError("compact pull journal", "pull journal cannot be truncated")
	}
	if err := resume.journalCompactionHook("journal-truncated"); err != nil {
		return err
	}
	if _, err := resume.journal.Seek(0, io.SeekStart); err != nil {
		return filesystemError("compact pull journal", "pull journal position cannot be reset")
	}
	if err := resume.journal.Sync(); err != nil {
		return filesystemError("compact pull journal", "pull journal cannot be synchronized")
	}
	resume.journalBytes = 0
	if err := resume.journalCompactionHook("journal-reset"); err != nil {
		return err
	}
	return nil
}
