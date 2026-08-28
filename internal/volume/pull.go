package volume

import "context"

// Pull verifies a complete content graph in private same-filesystem staging
// and atomically publishes it into an empty destination. The supplied object
// reader must already be scoped to the pinned manifest.
func (c *Client) Pull(ctx context.Context, options PullOptions) (*PullResult, error) {
	if ctx == nil {
		return nil, invalidError("pull volume", "context is required")
	}
	if !platformSupportsAtomicPull() {
		return nil, unsupportedError(
			"pull volume",
			"atomic volume pull is supported only on Linux and macOS",
		)
	}
	if options.Objects == nil {
		return nil, invalidError("pull volume", "Objects is required")
	}
	if c.decoder == nil {
		return nil, invalidError("pull volume", "Decoder is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, canceledError("pull volume", err)
	}
	selectors, err := normalizeIncludePaths(
		options.Include,
		c.maxFiles,
		c.maxManifestBytes,
		c.effectivePortablePathLimits(),
	)
	if err != nil {
		return nil, err
	}
	destination, err := preflightDestination(options.Destination)
	if err != nil {
		return nil, err
	}
	defer destination.close()
	progress := newProgressReporter(OperationPull, options.Progress, c.progress)
	budget := newByteGate(c.maxBytesInFlight)
	plan, totals, err := c.loadPullPlan(
		ctx,
		options.Objects,
		options.ManifestDigest,
		selectors,
		progress,
		budget,
	)
	if err != nil {
		return nil, err
	}
	resume, err := c.preparePullResume(
		ctx,
		destination,
		options.ManifestDigest,
		selectors,
		plan,
		options.Restart,
		budget,
	)
	if err != nil {
		return nil, err
	}
	defer resume.close()

	downloaded, reused, err := c.extractPull(
		ctx,
		options.Objects,
		resume,
		plan,
		progress,
		budget,
	)
	if err != nil {
		return nil, err
	}
	result := &PullResult{
		ManifestDigest:       options.ManifestDigest,
		OutputDirectory:      options.Destination,
		PublicationOutcome:   PullPublicationIncomplete,
		LogicalBytes:         plan.totalSize,
		DownloadedBytes:      downloaded,
		ReusedBytes:          reused,
		FileCount:            uint64(len(plan.files)),
		DirectoryCount:       uint64(len(plan.directories)),
		ContentVerified:      true,
		VolumeLogicalBytes:   totals.logicalBytes,
		VolumeFileCount:      totals.fileCount,
		VolumeDirectoryCount: totals.directoryCount,
	}
	return c.publishPull(ctx, resume, destination, plan, result, progress)
}

func (c *Client) preparePullResume(
	ctx context.Context,
	destination destinationPreflight,
	manifestDigest Digest,
	selectors []string,
	plan pullPlan,
	restart bool,
	budget *byteGate,
) (*pullResume, error) {
	identity, err := newPullIdentityWithLimits(
		manifestDigest,
		destination.destination,
		selectors,
		c.maxFiles,
		c.maxManifestBytes,
		c.effectivePortablePathLimits(),
	)
	if err != nil {
		return nil, err
	}
	expectedChunks, err := expectedPullChunks(ctx, plan)
	if err != nil {
		return nil, err
	}
	resume, err := c.openPullResumeAt(
		destination.parent,
		destination.parentPath,
		identity,
		expectedChunks,
		restart,
	)
	if err != nil {
		return nil, err
	}
	succeeded := false
	defer func() {
		if !succeeded {
			_ = resume.close()
		}
	}()

	aliases := make(hostAliasRegistry)
	directories, err := prepareStagingDirectories(
		ctx,
		resume.data,
		plan,
		aliases,
		c.maxFiles,
		c.effectivePortablePathLimits(),
	)
	if err != nil {
		return nil, err
	}
	if err := prepareStagingFiles(ctx, resume.data, plan, directories, aliases); err != nil {
		return nil, err
	}
	if err := prepareStagingSymlinks(ctx, resume.data, plan, directories, aliases); err != nil {
		return nil, err
	}
	if err := recaptureStagingDirectories(resume.data, directories); err != nil {
		return nil, err
	}
	resume.directories = directories
	resume.dataStat = directories[""]
	if c.filesystemHooks != nil && c.filesystemHooks.afterPullPrepared != nil {
		c.filesystemHooks.afterPullPrepared(resume)
	}
	var reusableBytes uint64
	if !resume.created {
		reusableBytes, err = c.reusableResumeBytes(
			ctx,
			resume.data,
			directories,
			plan,
			resume,
			budget,
		)
		if err != nil {
			return nil, err
		}
	}
	if reusableBytes > plan.totalSize {
		return nil, integrityError(
			"check destination capacity",
			"reusable pull bytes exceed the logical pull size",
		)
	}
	remainingBytes := plan.totalSize - reusableBytes
	if err := c.ensureDestinationCapacity(destination.parentPath, remainingBytes); err != nil {
		if discardErr := resume.discardCreatedState(); discardErr != nil {
			return nil, discardErr
		}
		return nil, err
	}
	succeeded = true
	return resume, nil
}

func (c *Client) extractPull(
	ctx context.Context,
	reader ObjectReader,
	resume *pullResume,
	plan pullPlan,
	progress *progressReporter,
	budget *byteGate,
) (uint64, uint64, error) {
	if err := validatePullPlanStructure(
		plan,
		c.maxFiles,
		c.effectivePortablePathLimits(),
	); err != nil {
		return 0, 0, err
	}
	progress.emit(ProgressEvent{
		Phase:      ProgressDownload,
		TotalItems: totalPointer(uint64(len(plan.files))),
		TotalBytes: totalPointer(plan.totalSize),
	})
	downloaded, reused, err := c.extractFiles(
		ctx,
		reader,
		resume.data,
		resume.directories,
		plan,
		resume,
		progress,
		budget,
	)
	if err != nil {
		return 0, 0, err
	}
	if c.filesystemHooks != nil && c.filesystemHooks.beforePullFinalVerify != nil {
		c.filesystemHooks.beforePullFinalVerify(resume)
	}
	verifiedTree, err := c.verifyStagingAfterExtraction(
		ctx,
		resume.data,
		resume.directories,
		plan,
		progress,
	)
	if err != nil {
		return 0, 0, err
	}
	resume.verifiedTree = &verifiedTree
	return downloaded, reused, nil
}

func (c *Client) publishPull(
	ctx context.Context,
	resume *pullResume,
	destination destinationPreflight,
	plan pullPlan,
	result *PullResult,
	progress *progressReporter,
) (*PullResult, error) {
	if err := validatePullPlanStructure(
		plan,
		c.maxFiles,
		c.effectivePortablePathLimits(),
	); err != nil {
		return nil, err
	}
	progress.emit(ProgressEvent{
		Phase:      ProgressPublish,
		TotalItems: totalPointer(1),
	})
	published, publishErr := c.publishStaging(ctx, resume, destination, plan)
	if !published {
		return nil, publishErr
	}
	progress.emit(ProgressEvent{
		Phase:          ProgressPublish,
		CompletedItems: 1,
		TotalItems:     totalPointer(1),
	})
	if publishErr != nil {
		return result, newPullPublicationError(result, resume, destination, publishErr)
	}
	result.PublicationOutcome = PullPublicationComplete
	return result, nil
}
