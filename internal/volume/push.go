package volume

import (
	"bytes"
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

func missingObjects(
	ctx context.Context,
	session UploadSession,
	objects map[Digest]*pushObject,
) (map[Digest]bool, error) {
	digests := make([]Digest, 0, len(objects))
	for digest := range objects {
		digests = append(digests, digest)
	}
	sort.Slice(digests, func(i, j int) bool {
		return digests[i].Hex() < digests[j].Hex()
	})

	missing := make(map[Digest]bool)
	for start := 0; start < len(digests); start += MaxMissingDigests {
		if err := ctx.Err(); err != nil {
			return nil, canceledError("query missing objects", err)
		}
		end := min(start+MaxMissingDigests, len(digests))
		requested := append([]Digest(nil), digests[start:end]...)
		returned, err := session.MissingObjects(ctx, requested)
		if err != nil {
			return nil, transferError("query missing objects", err)
		}
		requestedSet := make(map[Digest]bool, len(requested))
		for _, digest := range requested {
			requestedSet[digest] = true
		}
		for _, digest := range returned {
			if !requestedSet[digest] || missing[digest] {
				return nil, protocolError(
					"query missing objects",
					"upload session returned an unrequested or duplicate digest",
				)
			}
			missing[digest] = true
		}
	}
	return missing, nil
}

type uploadSummary struct {
	uploadedBytes   uint64
	manifestCreated bool
}

func (c *Client) uploadMissing(
	ctx context.Context,
	uploader ObjectUploader,
	plan pushPlan,
	missing map[Digest]bool,
	progress *progressReporter,
) (uploadSummary, error) {
	objects := make([]*pushObject, 0, len(missing))
	for digest := range missing {
		object := plan.objects[digest]
		if object == nil {
			return uploadSummary{}, protocolError(
				"upload objects",
				"missing-object inventory references unknown content",
			)
		}
		objects = append(objects, object)
	}
	sort.Slice(objects, func(i, j int) bool {
		return objects[i].digest.Hex() < objects[j].digest.Hex()
	})
	var totalUploadBytes uint64
	for _, object := range objects {
		if object.source == nil {
			continue
		}
		next, overflow := addUint64(totalUploadBytes, object.source.length)
		if overflow {
			return uploadSummary{}, preconditionError(
				"upload objects",
				"upload byte count overflows",
			)
		}
		totalUploadBytes = next
	}
	progress.emit(ProgressEvent{
		Phase:      ProgressUpload,
		TotalItems: totalPointer(uint64(len(objects))),
		TotalBytes: totalPointer(totalUploadBytes),
	})
	if len(objects) == 0 {
		return uploadSummary{}, nil
	}

	workerContext, cancel := context.WithCancel(ctx)
	defer cancel()
	var next atomic.Int64
	var uploaded atomic.Uint64
	var completed atomic.Uint64
	var manifestCreated atomic.Bool
	var firstError error
	var errorMu sync.Mutex
	requests := newDataPathRequestGate(c.maxConcurrency)
	memory := newByteGate(c.maxBytesInFlight)
	workers := min(c.maxConcurrency, len(objects))
	var wait sync.WaitGroup
	wait.Add(workers)
	for range workers {
		go func() {
			defer wait.Done()
			for {
				index := int(next.Add(1) - 1)
				if index >= len(objects) || workerContext.Err() != nil {
					return
				}
				object := objects[index]
				created, logicalBytes, err := c.uploadOne(
					workerContext,
					uploader,
					object,
					requests,
					memory,
				)
				if err != nil {
					errorMu.Lock()
					if firstError == nil {
						firstError = err
						cancel()
					}
					errorMu.Unlock()
					return
				}
				if object.source != nil {
					uploaded.Add(logicalBytes)
				}
				if object.digest == plan.manifestDigest && created {
					manifestCreated.Store(true)
				}
				done := completed.Add(1)
				progress.emit(ProgressEvent{
					Phase:          ProgressUpload,
					CompletedItems: done,
					TotalItems:     totalPointer(uint64(len(objects))),
					CompletedBytes: uploaded.Load(),
					TotalBytes:     totalPointer(totalUploadBytes),
				})
			}
		}()
	}
	wait.Wait()
	if firstError != nil {
		return uploadSummary{}, firstError
	}
	if err := ctx.Err(); err != nil {
		return uploadSummary{}, canceledError("upload objects", err)
	}
	return uploadSummary{
		uploadedBytes:   uploaded.Load(),
		manifestCreated: manifestCreated.Load(),
	}, nil
}

func (c *Client) uploadOne(
	ctx context.Context,
	uploader ObjectUploader,
	object *pushObject,
	requests *adaptiveRequestGate,
	memory *byteGate,
) (bool, uint64, error) {
	requestPermit, err := requests.acquire(ctx)
	if err != nil {
		return false, 0, canceledError("upload object", err)
	}
	requestOutcome := transferNeutral
	var requestLatency *time.Duration
	defer func() {
		if requestLatency == nil {
			requestPermit.complete(requestOutcome)
		} else {
			requestPermit.completeWithLatency(requestOutcome, *requestLatency)
		}
	}()

	bodyBytes := uint64(len(object.data))
	if object.source != nil {
		bodyBytes = object.source.length
	}
	memoryPermit, err := memory.acquire(ctx, bodyBytes)
	if err != nil {
		return false, 0, byteGateError("upload object", err)
	}
	defer memoryPermit.release()

	body := object.data
	var logicalBytes uint64
	if object.source != nil {
		body, err = c.readChunkSource(ctx, *object.source)
		if err != nil {
			return false, 0, err
		}
		defer clear(body)
		logicalBytes = object.source.length
	}
	requestStarted := time.Now()
	result, err := uploader.UploadObject(ctx, UploadObject{
		Digest: object.digest,
		Kind:   object.kind,
		Size:   uint64(len(body)),
		Body:   bytes.NewReader(body),
	})
	if err != nil {
		normalized := transferError("upload object", err)
		requestOutcome = classifyTransferOutcome(
			normalized,
			snapshotTransferObservation(result.Observation),
			false,
		)
		return false, 0, normalized
	}
	if err := ctx.Err(); err != nil {
		normalized := canceledError("upload object", err)
		requestOutcome = classifyTransferOutcome(
			normalized,
			snapshotTransferObservation(result.Observation),
			false,
		)
		return false, 0, normalized
	}
	transferred := result.Created && len(body) > 0
	if transferred {
		elapsed := time.Since(requestStarted)
		requestLatency = &elapsed
	}
	requestOutcome = classifyTransferOutcome(
		nil,
		snapshotTransferObservation(result.Observation),
		transferred,
	)
	if !result.Created {
		logicalBytes = 0
	}
	return result.Created, logicalBytes, nil
}

// Push scans and hashes a directory, transfers missing content through the
// supplied uploader, and asks the session to publish the manifest.
func (c *Client) Push(ctx context.Context, options PushOptions) (*PushResult, error) {
	if ctx == nil {
		return nil, invalidError("push volume", "context is required")
	}
	if !platformSupportsPush() {
		return nil, unsupportedError(
			"push volume",
			"stable source mutation detection is supported only on Linux and macOS",
		)
	}
	if options.Path == "" {
		return nil, invalidError("push volume", "Path is required")
	}
	if options.Session == nil {
		return nil, invalidError("push volume", "Session is required")
	}
	if options.Uploader == nil {
		return nil, invalidError("push volume", "Uploader is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, canceledError("push volume", err)
	}

	progress := newProgressReporter(OperationPush, options.Progress, c.progress)
	inputs, err := c.scanPush(ctx, options.Path, progress)
	if err != nil {
		return nil, err
	}
	defer inputs.close()
	plan, err := c.planPush(ctx, inputs, progress)
	if err != nil {
		return nil, err
	}
	missing, err := missingObjects(ctx, options.Session, plan.objects)
	if err != nil {
		return nil, err
	}
	summary, err := c.uploadMissing(ctx, options.Uploader, plan, missing, progress)
	if err != nil {
		return nil, err
	}
	if err := verifyPushInputs(ctx, plan.source); err != nil {
		return nil, err
	}
	if err := validatePushInputStructure(
		plan.source,
		c.maxFiles,
		c.effectivePortablePathLimits(),
	); err != nil {
		return nil, err
	}
	result := &PushResult{
		ManifestDigest: plan.manifestDigest,
		LogicalBytes:   plan.totalBytes,
		UploadedBytes:  summary.uploadedBytes,
		ReusedBytes:    plan.totalBytes - summary.uploadedBytes,
		FileCount:      plan.fileCount,
		DirectoryCount: plan.directoryCount,
		ContentCreated: summary.manifestCreated,
	}
	publication, publicationErr := publishPush(
		ctx,
		options.Session,
		plan.manifestDigest,
		progress,
	)
	result.Publication = publication
	if publicationErr != nil {
		if publicationErr.PublicationMayHaveHappened() {
			publicationErr.Result = result
			return result, publicationErr
		}
		return nil, publicationErr
	}
	return result, nil
}

func (c *Client) scanPush(
	ctx context.Context,
	path string,
	progress *progressReporter,
) (pushInputs, error) {
	progress.emit(ProgressEvent{Phase: ProgressScan})
	var afterLstat func(string)
	if c.filesystemHooks != nil {
		afterLstat = c.filesystemHooks.afterPushLstat
	}
	inputs, err := collectPushInputsWithHook(
		ctx,
		path,
		c.maxFiles,
		c.effectivePortablePathLimits(),
		afterLstat,
	)
	if err != nil {
		return pushInputs{}, err
	}
	progress.emit(ProgressEvent{
		Phase:          ProgressScan,
		CompletedItems: uint64(len(inputs.files)),
		TotalItems:     totalPointer(uint64(len(inputs.files))),
		CompletedBytes: inputs.totalBytes,
		TotalBytes:     totalPointer(inputs.totalBytes),
	})
	return inputs, nil
}

func (c *Client) planPush(
	ctx context.Context,
	inputs pushInputs,
	progress *progressReporter,
) (pushPlan, error) {
	progress.emit(ProgressEvent{
		Phase:      ProgressHash,
		TotalItems: totalPointer(uint64(len(inputs.files))),
		TotalBytes: totalPointer(inputs.totalBytes),
	})
	return c.buildPushPlan(ctx, inputs, progress)
}

func publishPush(
	ctx context.Context,
	session UploadSession,
	manifestDigest Digest,
	progress *progressReporter,
) (PublishResult, *PushPublicationError) {
	progress.emit(ProgressEvent{
		Phase:      ProgressPublish,
		TotalItems: totalPointer(1),
	})
	publication, publishErr := session.Publish(ctx, manifestDigest)
	publication, invalid := normalizePublishResult(publication, publishErr)
	if publication.Outcome == PublishOutcomePublished {
		progress.emit(ProgressEvent{
			Phase:          ProgressPublish,
			CompletedItems: 1,
			TotalItems:     totalPointer(1),
		})
	}
	if publishErr != nil || invalid || publication.Outcome != PublishOutcomePublished {
		return publication, newPushPublicationError(publication, publishErr)
	}
	return publication, nil
}
