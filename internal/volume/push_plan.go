package volume

import "context"

type pushObject struct {
	digest Digest
	kind   ObjectKind
	data   []byte
	source *chunkSource
}

type pushPlan struct {
	objects        map[Digest]*pushObject
	manifestDigest Digest
	totalBytes     uint64
	fileCount      uint64
	directoryCount uint64
	source         pushInputs
}

func (c *Client) buildPushPlan(
	ctx context.Context,
	inputs pushInputs,
	progress *progressReporter,
) (pushPlan, error) {
	if err := validatePushInputStructure(
		inputs,
		c.maxFiles,
		c.effectivePortablePathLimits(),
	); err != nil {
		return pushPlan{}, err
	}
	objects := make(map[Digest]*pushObject)
	kinds := make(map[Digest]ObjectKind)
	files := make([]manifestFile, 0, len(inputs.files))
	var hashedBytes uint64
	var graphChunks uint64
	var chunkmapMetadataBytes uint64
	maxChunkmapFanout := max(c.maxManifestBytes/chunkmapFanoutBudgetBytes, uint64(1))
	maxGraphChunks := max(c.maxManifestBytes/contentGraphChunkBudgetBytes, uint64(1))
	for index, source := range inputs.files {
		if err := ctx.Err(); err != nil {
			return pushPlan{}, canceledError("hash source", err)
		}
		sourceChunks := sourceChunkCount(source.snapshot.size)
		if sourceChunks > 1 && sourceChunks > maxChunkmapFanout {
			return pushPlan{}, preconditionError(
				"hash source",
				"source file exceeds the configured chunkmap fanout limit",
			)
		}
		nextGraphChunks, overflow := addUint64(graphChunks, sourceChunks)
		if overflow || nextGraphChunks > maxGraphChunks {
			return pushPlan{}, preconditionError(
				"hash source",
				"source exceeds the aggregate chunk limit",
			)
		}
		graphChunks = nextGraphChunks
		chunks, err := c.hashSourceFile(ctx, source)
		if err != nil {
			return pushPlan{}, err
		}
		for _, chunk := range chunks {
			var object *pushObject
			if chunk.length != 0 {
				chunkCopy := chunk
				object = &pushObject{
					digest: chunk.digest,
					kind:   ObjectKindChunk,
					source: &chunkCopy,
				}
			}
			if err := addPushObject(objects, kinds, chunk.digest, ObjectKindChunk, object); err != nil {
				return pushPlan{}, err
			}
		}
		if len(chunks) == 1 {
			chunk := chunks[0]
			files = append(files, manifestFile{
				Kind: fileKindChunk,
				Chunk: chunkEntry{
					Digest: chunk.digest,
					Length: chunk.length,
					Target: targetForDigest(chunk.digest),
				},
				Mode: source.snapshot.mode,
				Path: source.relativePath,
				Size: chunk.length,
			})
		} else {
			entries := make([]chunkEntry, 0, len(chunks))
			for _, chunk := range chunks {
				entries = append(entries, chunkEntry{
					Digest: chunk.digest,
					Length: chunk.length,
					Offset: chunk.offset,
					Target: targetForDigest(chunk.digest),
				})
			}
			chunkmapBody := encodeChunkmap(source.snapshot.size, entries)
			nextMetadataBytes, overflow := addUint64(
				chunkmapMetadataBytes,
				uint64(len(chunkmapBody)),
			)
			if overflow || nextMetadataBytes > c.maxManifestBytes {
				return pushPlan{}, preconditionError(
					"build chunkmaps",
					"encoded chunkmap metadata exceeds the aggregate limit",
				)
			}
			chunkmapMetadataBytes = nextMetadataBytes
			chunkmapDigest, err := c.digest(chunkmapBody)
			if err != nil {
				return pushPlan{}, err
			}
			object := &pushObject{
				digest: chunkmapDigest,
				kind:   ObjectKindChunkmap,
				data:   chunkmapBody,
			}
			if err := addPushObject(
				objects,
				kinds,
				chunkmapDigest,
				ObjectKindChunkmap,
				object,
			); err != nil {
				return pushPlan{}, err
			}
			files = append(files, manifestFile{
				Kind:   fileKindChunkmap,
				Digest: chunkmapDigest,
				Mode:   source.snapshot.mode,
				Path:   source.relativePath,
				Size:   source.snapshot.size,
				Target: targetForDigest(chunkmapDigest),
			})
		}
		nextHashedBytes, overflow := addUint64(hashedBytes, source.snapshot.size)
		if overflow {
			return pushPlan{}, preconditionError("hash source", "hashed byte count overflows")
		}
		hashedBytes = nextHashedBytes
		progress.emit(ProgressEvent{
			Phase:          ProgressHash,
			CompletedItems: uint64(index + 1),
			TotalItems:     totalPointer(uint64(len(inputs.files))),
			CompletedBytes: hashedBytes,
			TotalBytes:     totalPointer(inputs.totalBytes),
		})
	}

	manifestBody := encodeManifest(
		inputs.totalBytes,
		inputs.directories,
		files,
		inputs.symlinks,
		defaultLocalSourceURI,
	)
	if uint64(len(manifestBody)) > c.maxManifestBytes {
		return pushPlan{}, preconditionError(
			"build manifest",
			"manifest exceeds the configured size limit",
		)
	}
	manifestDigest, err := c.digest(manifestBody)
	if err != nil {
		return pushPlan{}, err
	}
	manifestObject := &pushObject{
		digest: manifestDigest,
		kind:   ObjectKindManifest,
		data:   manifestBody,
	}
	if err := addPushObject(
		objects,
		kinds,
		manifestDigest,
		ObjectKindManifest,
		manifestObject,
	); err != nil {
		return pushPlan{}, err
	}
	return pushPlan{
		objects:        objects,
		manifestDigest: manifestDigest,
		totalBytes:     inputs.totalBytes,
		fileCount:      uint64(len(inputs.files)),
		directoryCount: uint64(len(inputs.directories)),
		source:         inputs,
	}, nil
}

func addPushObject(
	objects map[Digest]*pushObject,
	kinds map[Digest]ObjectKind,
	digest Digest,
	kind ObjectKind,
	object *pushObject,
) error {
	if err := addSemanticObjectKind(
		kinds,
		digest,
		kind,
		"build content graph",
	); err != nil {
		return err
	}
	if object == nil {
		return nil
	}
	if _, exists := objects[digest]; !exists {
		objects[digest] = object
	}
	return nil
}

func addSemanticObjectKind(
	kinds map[Digest]ObjectKind,
	digest Digest,
	kind ObjectKind,
	operation string,
) error {
	if previous, exists := kinds[digest]; exists && previous != kind {
		return preconditionError(
			operation,
			"one digest is used by multiple semantic object kinds",
		)
	}
	kinds[digest] = kind
	return nil
}
