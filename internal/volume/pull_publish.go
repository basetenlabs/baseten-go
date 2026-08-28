package volume

import (
	"context"
	"errors"
	"io"
	"os"
)

func revalidateStagingForPublication(resume *pullResume) error {
	if resume.parent == nil || resume.staging == nil || resume.data == nil {
		return filesystemError("publish pull", "anchored staging state is unavailable")
	}
	staging, err := resume.staging.currentStat()
	if err != nil || !sameRootedSnapshot(resume.stagingStat, staging) {
		return preconditionError("publish pull", "pull staging changed during transfer")
	}
	parentEntry, err := resume.parent.lstat(resume.stagingName, nil)
	if err != nil || !sameRootedObject(resume.stagingStat, parentEntry) {
		return preconditionError("publish pull", "pull staging path changed during transfer")
	}
	data, err := resume.data.currentStat()
	if err != nil || !sameRootedSnapshot(resume.dataStat, data) {
		return preconditionError("publish pull", "staged data root changed during transfer")
	}
	dataEntry, err := resume.staging.lstat(pullDataName, nil)
	if err != nil || !sameRootedSnapshot(resume.dataStat, dataEntry) {
		return preconditionError("publish pull", "staged data path changed during transfer")
	}
	return nil
}

func (c *Client) publishStaging(
	ctx context.Context,
	resume *pullResume,
	destination destinationPreflight,
	plan pullPlan,
) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, canceledError("publish pull", err)
	}
	if err := validatePullPlanStructure(
		plan,
		c.maxFiles,
		c.effectivePortablePathLimits(),
	); err != nil {
		return false, err
	}
	if resume.parent == nil || resume.staging == nil || resume.data == nil {
		return false, filesystemError("publish pull", "anchored staging state is unavailable")
	}
	if resume.hooks != nil && resume.hooks.beforePullPublish != nil {
		resume.hooks.beforePullPublish(resume, destination)
	}
	if err := revalidateDestination(destination); err != nil {
		return false, err
	}
	if err := revalidateStagingForPublication(resume); err != nil {
		return false, err
	}
	if resume.verifiedTree == nil {
		return false, integrityError("publish pull", "verified staging snapshot is unavailable")
	}
	publishedTree, err := c.verifyStagingForPublication(
		ctx,
		resume.data,
		resume.directories,
		plan,
		*resume.verifiedTree,
	)
	if err != nil {
		return false, err
	}
	resume.verifiedTree = &publishedTree
	resume.dataStat = publishedTree.root
	if resume.hooks != nil && resume.hooks.afterPullPublishVerify != nil {
		if err := resume.hooks.afterPullPublishVerify(resume); err != nil {
			return false, filesystemError(
				"publish pull",
				"pull publication was interrupted after verification",
			)
		}
	}
	if err := revalidateStagingForPublication(resume); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, canceledError("publish pull", err)
	}
	published, err := atomicPublishDirectory(
		resume.staging,
		pullDataName,
		publishedTree.root,
		destination.parent,
		destination.name,
		destination.existed,
		destination.info,
	)
	if err != nil {
		if errors.Is(err, errStagingPublishIdentityChanged) {
			return published, preconditionError(
				"publish pull",
				"destination changed at the publication boundary",
			)
		}
		return published, filesystemError(
			"publish pull",
			"staging directory cannot be atomically published",
		)
	}
	if err := revalidateDestinationParent(destination); err != nil {
		return true, err
	}
	if err := finishPublishedPull(resume, destination); err != nil {
		return true, err
	}
	return true, nil
}

func finishPublishedPull(resume *pullResume, destination destinationPreflight) error {
	if resume.parent == nil {
		return filesystemError("publish pull", "anchored staging state is unavailable")
	}
	if err := destination.parent.sync(); err != nil {
		return filesystemError("publish pull", "destination parent cannot be synchronized")
	}
	if resume.data != nil {
		if err := resume.data.close(); err != nil {
			return filesystemError("publish pull", "published data descriptor cannot be closed")
		}
		resume.data = nil
	}
	if resume.publishedDataCleaned {
		return resume.removePublishedState()
	}
	if resume.staging == nil {
		return filesystemError("publish pull", "anchored staging state is unavailable")
	}
	if destination.existed {
		replaced, err := resume.staging.lstat(pullDataName, nil)
		if err != nil || !sameRootedObject(destination.info, replaced) || !replaced.mode.IsDir() {
			return integrityError(
				"publish pull",
				"replaced destination is not the preflighted empty directory",
			)
		}
		directory, opened, err := resume.staging.openDirectory(pullDataName, nil)
		if err != nil || !sameRootedObject(destination.info, opened) {
			if directory != nil {
				directory.Close()
			}
			return integrityError(
				"publish pull",
				"replaced destination changed before cleanup",
			)
		}
		entries, readErr := directory.ReadDir(1)
		closeErr := directory.Close()
		if (readErr != nil && !errors.Is(readErr, io.EOF)) ||
			closeErr != nil ||
			len(entries) != 0 {
			return integrityError(
				"publish pull",
				"replaced destination is no longer empty",
			)
		}
		if err := resume.staging.remove(pullDataName, true, nil); err != nil {
			return filesystemError(
				"publish pull",
				"replaced empty destination cannot be removed",
			)
		}
	} else if _, err := resume.staging.lstat(pullDataName, nil); !isNotExist(err) {
		if err == nil {
			return integrityError("publish pull", "published staging data unexpectedly remains")
		}
		return filesystemError("publish pull", "published staging data cannot be inspected")
	}
	resume.publishedDataCleaned = true
	return resume.removePublishedState()
}

func (resume *pullResume) removePublishedStateAt() error {
	if resume.staging == nil {
		if err := resume.parent.sync(); err != nil {
			return filesystemError(
				"publish pull",
				"destination parent cannot be synchronized after cleanup",
			)
		}
		return nil
	}
	if resume.journal != nil {
		journal := resume.journal
		resume.journal = nil
		if err := journal.Close(); err != nil {
			return filesystemError("publish pull", "completed pull journal cannot be closed")
		}
	}
	for _, name := range []string{
		pullIdentityName,
		pullCheckpointName,
		pullJournalName,
		pullLockName,
	} {
		if name == pullLockName && resume.lock != nil {
			held, heldErr := rootedFileStatFromFile(resume.lock)
			path, pathErr := resume.staging.lstat(name, nil)
			if heldErr != nil ||
				pathErr != nil ||
				!sameRootedObject(held, path) ||
				validateOwnedRegular(held, 0o600) != nil {
				return integrityError(
					"publish pull",
					"completed pull lock changed before cleanup",
				)
			}
			if err := resume.staging.remove(name, false, nil); err != nil {
				return filesystemError("publish pull", "completed pull lock cannot be removed")
			}
			continue
		}
		file, _, err := openRootedPrivateFile(resume.staging, name, os.O_RDONLY)
		if isNotExist(err) {
			continue
		}
		if err != nil {
			return integrityError(
				"publish pull",
				"completed pull state is not a private regular file",
			)
		}
		if err := file.Close(); err != nil {
			return filesystemError("publish pull", "completed pull state cannot be closed")
		}
		if err := resume.staging.remove(name, false, nil); err != nil && !isNotExist(err) {
			return filesystemError("publish pull", "completed pull state cannot be removed")
		}
	}
	names, err := resume.staging.readDirNames("", nil, 0)
	if err != nil {
		return integrityError("publish pull", "completed pull staging is not empty")
	}
	if len(names) != 0 {
		return integrityError("publish pull", "completed pull staging contains unexpected state")
	}
	if err := resume.staging.sync(); err != nil {
		return filesystemError("publish pull", "completed pull staging cannot be synchronized")
	}
	parentEntry, err := resume.parent.lstat(resume.stagingName, nil)
	switch {
	case err == nil:
		if !sameRootedObject(resume.stagingStat, parentEntry) {
			return integrityError("publish pull", "completed pull staging path changed before cleanup")
		}
		if err := resume.parent.remove(resume.stagingName, true, nil); err != nil {
			return filesystemError("publish pull", "completed pull staging cannot be removed")
		}
	case isNotExist(err):
		// A previous cleanup attempt removed the state before parent fsync.
	default:
		return filesystemError("publish pull", "completed pull staging cannot be inspected")
	}
	if resume.lock != nil {
		lock := resume.lock
		resume.lock = nil
		if err := closePullStateLock(lock); err != nil {
			return filesystemError("publish pull", "completed pull state cannot be unlocked")
		}
	}
	if err := resume.parent.sync(); err != nil {
		return filesystemError(
			"publish pull",
			"destination parent cannot be synchronized after cleanup",
		)
	}
	if err := resume.staging.close(); err != nil {
		return filesystemError("publish pull", "completed pull staging cannot be closed")
	}
	resume.staging = nil
	return nil
}

func newPullPublicationError(
	result *PullResult,
	resume *pullResume,
	destination destinationPreflight,
	cause error,
) *PullPublicationError {
	resume.cleanupRetained = true
	var duplicateErr error
	if resume.parent != nil {
		duplicate, err := resume.parent.duplicate()
		if err != nil {
			duplicateErr = err
			resume.cleanupRetained = false
			_ = resume.close()
		} else {
			resume.parent = duplicate
			resume.ownsParent = true
			destination.parent = duplicate
		}
	}
	return &PullPublicationError{
		Result: result,
		cleanup: func(retryContext context.Context) error {
			if err := retryContext.Err(); err != nil {
				return canceledError("retry pull cleanup", err)
			}
			if duplicateErr != nil {
				return filesystemError(
					"retry pull cleanup",
					"destination parent descriptor cannot be retained",
				)
			}
			if err := revalidateDestinationParent(destination); err != nil {
				return err
			}
			if err := finishPublishedPull(resume, destination); err != nil {
				return err
			}
			resume.cleanupRetained = false
			return resume.close()
		},
		cause: &Error{
			Code:      ErrorPublicationIncomplete,
			Operation: "publish pull",
			Message:   "pull content was published but durability or cleanup is incomplete",
			cause:     cause,
		},
	}
}
