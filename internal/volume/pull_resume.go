package volume

import (
	"errors"
	"os"
	"sync"
	"time"
)

// Pull resume state is a sibling of the destination with this layout:
//
//	.baseten-volume-pull-v1-<identity>.staging/
//	    data/
//	    identity-v1.json
//	    checkpoint-v1.json
//	    checkpoint-v1.journal
//	    pull.lock
//
// The identity binds the format version, manifest digest, canonical
// destination, and normalized include selectors. On Linux and macOS, the
// current effective user must own staging directories with mode 0700 and state
// and staged regular files with mode 0600; verified destination modes are
// applied only during finalization. Inactive matching state is eligible for
// bounded stale cleanup after 30 days. Pull, including resume and atomic
// publication, is unsupported on other platforms. New state is fully synced
// under a private temporary sibling before the deterministic path appears.
const (
	pullCheckpointVersion  = 1
	pullStagingPrefix      = ".baseten-volume-pull-v1-"
	pullStagingSuffix      = ".staging"
	pullCheckpointName     = "checkpoint-v1.json"
	pullIdentityName       = "identity-v1.json"
	pullJournalName        = "checkpoint-v1.journal"
	pullLockName           = "pull.lock"
	pullDataName           = "data"
	pullStaleAfter         = 30 * 24 * time.Hour
	pullMaxStaleScan       = 128
	pullMaxStaleRemovals   = 8
	pullMaxCleanupEntries  = 1_000_000
	pullStateOverheadBytes = 64 << 10
	pullInitPrefix         = ".baseten-volume-pull-v1-init-"
	pullInitSuffix         = ".tmp"
)

func pullCleanupEntryLimit(maxFiles int, maxManifestBytes uint64) int {
	maxInt := int(^uint(0) >> 1)
	maximum := min(maxInt, pullMaxCleanupEntries)
	if maxFiles > maxInt-16 {
		return maximum
	}
	remaining := maxInt - maxFiles - 16
	if maxManifestBytes > uint64(remaining) {
		return maximum
	}
	return min(maxFiles+int(maxManifestBytes)+16, maximum)
}

type pullIdentity struct {
	FormatVersion  uint32   `json:"format_version"`
	ManifestDigest Digest   `json:"manifest_digest"`
	Destination    string   `json:"destination"`
	Selectors      []string `json:"selectors"`
}

type completedPullChunk struct {
	Path   string `json:"path"`
	Offset uint64 `json:"offset"`
	Length uint64 `json:"length"`
	Digest Digest `json:"digest"`
}

type completedPullChunkLocation struct {
	Path   string
	Offset uint64
}

func (chunk completedPullChunk) location() completedPullChunkLocation {
	return completedPullChunkLocation{Path: chunk.Path, Offset: chunk.Offset}
}

type pullCheckpoint struct {
	Version              uint32               `json:"version"`
	Identity             pullIdentity         `json:"identity"`
	CreatedAtUnixSeconds uint64               `json:"created_at_unix_seconds"`
	UpdatedAtUnixSeconds uint64               `json:"updated_at_unix_seconds"`
	CompletedChunks      []completedPullChunk `json:"completed_chunks"`
}

type pullStateLimits struct {
	maxSelectors     int
	maxSelectorBytes uint64
	pathLimits       portablePathLimits
}

func defaultPullStateLimits() pullStateLimits {
	return pullStateLimits{
		maxSelectors:     defaultMaxFiles,
		maxSelectorBytes: defaultMaxManifestBytes,
		pathLimits:       defaultPortablePathLimits(),
	}
}

func (c *Client) pullStateLimits() pullStateLimits {
	return pullStateLimits{
		maxSelectors:     c.maxFiles,
		maxSelectorBytes: c.maxManifestBytes,
		pathLimits:       c.effectivePortablePathLimits(),
	}
}

func selectedPullStateLimits(configured []pullStateLimits) pullStateLimits {
	if len(configured) == 0 {
		return defaultPullStateLimits()
	}
	return configured[0]
}

type pullResume struct {
	stagingRoot          string
	dataRoot             string
	checkpointPath       string
	journalPath          string
	lockPath             string
	lock                 *os.File
	journal              *os.File
	created              bool
	parent               *rootedDirectory
	staging              *rootedDirectory
	data                 *rootedDirectory
	stagingName          string
	ownsParent           bool
	stagingStat          rootedFileStat
	dataStat             rootedFileStat
	directories          map[string]rootedFileStat
	verifiedTree         *stagedTreeSnapshot
	maxEntries           int
	maxStateBytes        uint64
	hooks                *filesystemTestHooks
	cleanupRetained      bool
	publishedDataCleaned bool

	mu           sync.Mutex
	checkpoint   pullCheckpoint
	completed    map[completedPullChunk]struct{}
	journalBytes uint64
}

func currentUnixSeconds() (uint64, error) {
	now := time.Now().Unix()
	if now < 0 {
		return 0, filesystemError("write pull checkpoint", "system clock precedes the Unix epoch")
	}
	return uint64(now), nil
}

func (resume *pullResume) close() error {
	if resume == nil {
		return nil
	}
	if resume.cleanupRetained {
		return nil
	}
	var journalErr error
	if resume.journal != nil {
		journal := resume.journal
		resume.journal = nil
		journalErr = journal.Close()
	}
	var lockErr error
	if resume.lock != nil {
		lock := resume.lock
		resume.lock = nil
		lockErr = closePullStateLock(lock)
	}
	var dataErr, stagingErr, parentErr error
	if resume.data != nil {
		dataErr = resume.data.close()
		resume.data = nil
	}
	if resume.staging != nil {
		stagingErr = resume.staging.close()
		resume.staging = nil
	}
	if resume.ownsParent && resume.parent != nil {
		parentErr = resume.parent.close()
		resume.parent = nil
	}
	return errors.Join(journalErr, lockErr, dataErr, stagingErr, parentErr)
}

func (resume *pullResume) discardCreatedState() error {
	if resume == nil || !resume.created {
		return nil
	}
	if resume.parent == nil || resume.staging == nil {
		return filesystemError("discard pull resume", "anchored pull staging is unavailable")
	}
	return resume.discardCreatedStateAt()
}

func (resume *pullResume) discardCreatedStateAt() error {
	if resume.journal != nil {
		journal := resume.journal
		resume.journal = nil
		if err := journal.Close(); err != nil {
			return filesystemError("discard pull resume", "new pull journal cannot be closed")
		}
	}
	if resume.data != nil {
		if err := resume.data.close(); err != nil {
			return filesystemError("discard pull resume", "new pull data cannot be closed")
		}
		resume.data = nil
	}
	if err := resume.staging.close(); err != nil {
		return filesystemError("discard pull resume", "new pull staging cannot be closed")
	}
	resume.staging = nil
	if !resume.parent.entryMatches(resume.stagingName, resume.stagingStat) {
		if resume.lock != nil {
			_ = closePullStateLock(resume.lock)
			resume.lock = nil
		}
		return integrityError("discard pull resume", "new pull staging changed before cleanup")
	}
	if err := resume.parent.removeTree(
		resume.stagingName,
		resume.stagingStat,
		resume.maxEntries,
	); err != nil {
		if resume.lock != nil {
			_ = closePullStateLock(resume.lock)
			resume.lock = nil
		}
		return filesystemError("discard pull resume", "new pull staging cannot be removed safely")
	}
	if resume.lock != nil {
		lock := resume.lock
		resume.lock = nil
		if err := closePullStateLock(lock); err != nil {
			return filesystemError("discard pull resume", "new pull state cannot be unlocked")
		}
	}
	if err := resume.parent.sync(); err != nil {
		return filesystemError(
			"discard pull resume",
			"destination parent cannot be synchronized",
		)
	}
	return nil
}

func (resume *pullResume) removePublishedState() error {
	if resume.staging == nil {
		return filesystemError("publish pull", "anchored pull staging is unavailable")
	}
	return resume.removePublishedStateAt()
}
