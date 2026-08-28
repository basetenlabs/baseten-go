package volume

import (
	"bytes"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type fileKind string

const (
	fileKindChunk    fileKind = "chunk"
	fileKindChunkmap fileKind = "chunkmap"
	fileKindSlabmap  fileKind = "slabmap"
)

type chunkEntry struct {
	Digest Digest       `json:"digest"`
	Length uint64       `json:"length"`
	Offset uint64       `json:"offset"`
	Target objectTarget `json:"target"`
}

type directoryEntry struct {
	Mode uint16
	Path string
}

type symlinkEntry struct {
	Mode   uint16
	Path   string
	Target string
}

type manifestFile struct {
	Kind   fileKind
	Chunk  chunkEntry
	Digest Digest
	Mode   uint16
	Path   string
	Size   uint64
	Target objectTarget
}

func (f manifestFile) digest() Digest {
	if f.Kind == fileKindChunk {
		return f.Chunk.Digest
	}
	return f.Digest
}

type validatedManifest struct {
	Directories []directoryEntry
	Files       []manifestFile
	Symlinks    []symlinkEntry
	TotalSize   uint64
}

type chunkmap struct {
	FileSize uint64
	Chunks   []chunkEntry
}

func encodeChunkmap(fileSize uint64, chunks []chunkEntry) []byte {
	var output []byte
	output = appendChunkmapHeaderRecord(output, chunkmapHeaderWire{
		ChunkCount: uint32(len(chunks)),
		FileSize:   fileSize,
	})
	output = append(output, '\n')
	for _, chunk := range chunks {
		output = appendChunkRecord(output, chunk)
		output = append(output, '\n')
	}
	return output
}

func encodeManifest(
	totalSize uint64,
	directories []directoryEntry,
	files []manifestFile,
	symlinks []symlinkEntry,
	sourceURI string,
) []byte {
	sort.Slice(directories, func(i, j int) bool {
		return directories[i].Path < directories[j].Path
	})
	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
	sort.Slice(symlinks, func(i, j int) bool {
		return symlinks[i].Path < symlinks[j].Path
	})

	entryCount := len(directories) + len(files) + len(symlinks)
	var output []byte
	output = appendManifestHeaderRecord(output, manifestHeaderWire{
		EntryCount:     uint32(entryCount),
		ManifestSchema: defaultManifestSchema,
		TotalSize:      totalSize,
	})
	output = append(output, '\n')
	output = appendProvenanceRecord(output, "provenance", provenanceWire{
		SourceFingerprint:     defaultSourceFingerprint,
		SourceFingerprintType: defaultFingerprintType,
		SourceURI:             sourceURI,
	})
	output = append(output, '\n')

	for _, directory := range directories {
		output = appendDirectoryRecord(output, directory)
		output = append(output, '\n')
	}
	for _, file := range files {
		output = appendManifestFileRecord(output, file)
		output = append(output, '\n')
	}
	for _, symlink := range symlinks {
		output = appendSymlinkRecord(output, symlink)
		output = append(output, '\n')
	}
	return output
}

func appendManifestHeaderRecord(output []byte, header manifestHeaderWire) []byte {
	output = append(output, `{"_type":"manifest_header","entry_count":`...)
	output = strconv.AppendUint(output, uint64(header.EntryCount), 10)
	output = append(output, `,"manifest_schema":`...)
	output = appendJSONString(output, header.ManifestSchema)
	output = append(output, `,"total_size":`...)
	output = strconv.AppendUint(output, header.TotalSize, 10)
	return append(output, '}')
}

func appendChunkmapHeaderRecord(output []byte, header chunkmapHeaderWire) []byte {
	output = append(output, `{"_type":"chunkmap_header","chunk_count":`...)
	output = strconv.AppendUint(output, uint64(header.ChunkCount), 10)
	output = append(output, `,"file_size":`...)
	output = strconv.AppendUint(output, header.FileSize, 10)
	return append(output, '}')
}

// Nested chunk entries omit the record discriminator while top-level chunkmap records include it,
// so appendChunkJSON and appendChunkRecord intentionally remain separate.
func appendChunkJSON(output []byte, chunk chunkEntry) []byte {
	output = append(output, `{"digest":`...)
	output = appendJSONString(output, chunk.Digest.String())
	output = append(output, `,"length":`...)
	output = strconv.AppendUint(output, chunk.Length, 10)
	output = append(output, `,"offset":`...)
	output = strconv.AppendUint(output, chunk.Offset, 10)
	output = append(output, `,"target":{"relative_key":`...)
	output = appendJSONString(output, chunk.Target.RelativeKey)
	return append(output, "}}"...)
}

func appendChunkRecord(output []byte, chunk chunkEntry) []byte {
	output = append(output, `{"_type":"chunk","digest":`...)
	output = appendJSONString(output, chunk.Digest.String())
	output = append(output, `,"length":`...)
	output = strconv.AppendUint(output, chunk.Length, 10)
	output = append(output, `,"offset":`...)
	output = strconv.AppendUint(output, chunk.Offset, 10)
	output = append(output, `,"target":{"relative_key":`...)
	output = appendJSONString(output, chunk.Target.RelativeKey)
	return append(output, "}}"...)
}

func appendDirectoryRecord(output []byte, directory directoryEntry) []byte {
	output = append(output, `{"_type":"directory","mode":`...)
	output = appendMode(output, directory.Mode)
	output = append(output, `,"path":`...)
	output = appendJSONString(output, directory.Path)
	return append(output, '}')
}

func appendManifestFileRecord(output []byte, file manifestFile) []byte {
	switch file.Kind {
	case fileKindChunk:
		output = append(output, `{"_type":"file","_kind":"chunk","chunk":`...)
		output = appendChunkJSON(output, file.Chunk)
		output = append(output, `,"mode":`...)
		output = appendMode(output, file.Mode)
		output = append(output, `,"path":`...)
		output = appendJSONString(output, file.Path)
		return append(output, '}')
	case fileKindChunkmap:
		output = append(output, `{"_type":"file","_kind":"chunkmap","digest":`...)
		output = appendJSONString(output, file.Digest.String())
		output = append(output, `,"mode":`...)
		output = appendMode(output, file.Mode)
		output = append(output, `,"path":`...)
		output = appendJSONString(output, file.Path)
		output = append(output, `,"size":`...)
		output = strconv.AppendUint(output, file.Size, 10)
		output = append(output, `,"target":{"relative_key":`...)
		output = appendJSONString(output, file.Target.RelativeKey)
		return append(output, "}}"...)
	default:
		panic("unsupported manifest file kind")
	}
}

func appendSymlinkRecord(output []byte, symlink symlinkEntry) []byte {
	output = append(output, `{"_type":"symlink","mode":`...)
	output = appendMode(output, symlink.Mode)
	output = append(output, `,"path":`...)
	output = appendJSONString(output, symlink.Path)
	output = append(output, `,"target":`...)
	output = appendJSONString(output, symlink.Target)
	return append(output, '}')
}

func appendProvenanceRecord(
	output []byte,
	recordType string,
	provenance provenanceWire,
) []byte {
	output = append(output, `{"_type":`...)
	output = appendJSONString(output, recordType)
	switch recordType {
	case "prefix_provenance":
		output = append(output, `,"prefix":`...)
		output = appendJSONString(output, provenance.Prefix)
	case "path_provenance":
		output = append(output, `,"path":`...)
		output = appendJSONString(output, provenance.Path)
	case "provenance":
	default:
		panic("unsupported provenance record type")
	}
	if provenance.ResolvedAt != nil {
		output = append(output, `,"resolved_at":`...)
		output = appendJSONString(output, *provenance.ResolvedAt)
	}
	output = append(output, `,"source_fingerprint":`...)
	output = appendJSONString(output, provenance.SourceFingerprint)
	output = append(output, `,"source_fingerprint_type":`...)
	output = appendJSONString(output, provenance.SourceFingerprintType)
	output = append(output, `,"source_uri":`...)
	output = appendJSONString(output, provenance.SourceURI)
	return append(output, '}')
}

func appendMode(output []byte, mode uint16) []byte {
	output = append(output, '"')
	value := strconv.FormatUint(uint64(mode), 8)
	for range max(4-len(value), 0) {
		output = append(output, '0')
	}
	output = append(output, value...)
	return append(output, '"')
}

// appendJSONString matches serde_json's compact string escaping. In
// particular it does not apply Go's HTML escaping.
func appendJSONString(output []byte, value string) []byte {
	const hexadecimal = "0123456789abcdef"
	output = append(output, '"')
	for _, character := range value {
		switch character {
		case '"', '\\':
			output = append(output, '\\', byte(character))
		case '\b':
			output = append(output, `\b`...)
		case '\t':
			output = append(output, `\t`...)
		case '\n':
			output = append(output, `\n`...)
		case '\f':
			output = append(output, `\f`...)
		case '\r':
			output = append(output, `\r`...)
		default:
			if character < 0x20 {
				output = append(output, `\u00`...)
				output = append(
					output,
					hexadecimal[(character>>4)&0xf],
					hexadecimal[character&0xf],
				)
			} else {
				output = utf8.AppendRune(output, character)
			}
		}
	}
	return append(output, '"')
}

type manifestRecordStage uint8

// The canonical producer emits path-sorted provenance and entry groups in this
// order; sorting all entry kinds together would produce different digests.
const (
	manifestStageHeader manifestRecordStage = iota
	manifestStagePrefixProvenance
	manifestStagePathProvenance
	manifestStageDirectory
	manifestStageFile
	manifestStageSymlink
)

func advanceManifestRecordStage(
	current *manifestRecordStage,
	next manifestRecordStage,
) error {
	if next < *current {
		return protocolError(
			"validate manifest",
			"manifest records are not in canonical order",
		)
	}
	*current = next
	return nil
}

func incrementManifestEntryCount(count *uint64, limit uint64) error {
	if *count == limit {
		return preconditionError(
			"validate manifest",
			"manifest exceeds the configured entry limit",
		)
	}
	next, overflow := addUint64(*count, 1)
	if overflow {
		return preconditionError("validate manifest", "manifest entry count overflows")
	}
	*count = next
	return nil
}

func requireAscendingPath(previous, current string, hasPrevious bool) error {
	if hasPrevious && current <= previous {
		return protocolError(
			"validate manifest",
			"manifest records are not in canonical path order",
		)
	}
	return nil
}

func addMetadataPathBytes(total *uint64, count, limit uint64) error {
	next, overflow := addUint64(*total, count)
	if overflow {
		return preconditionError("validate manifest", "manifest path bytes overflow")
	}
	if next > limit {
		return preconditionError(
			"validate manifest",
			"manifest exceeds the aggregate path byte limit",
		)
	}
	*total = next
	return nil
}

func decodeProvenanceRecord(
	line []byte,
	recordType string,
) (provenanceWire, error) {
	switch recordType {
	case "prefix_provenance", "path_provenance", "provenance":
	default:
		return provenanceWire{}, protocolError(
			"decode manifest",
			"manifest contains an unknown provenance type",
		)
	}
	var value provenanceWire
	if err := decodeStrictJSON(line, &value); err != nil {
		return provenanceWire{}, protocolError(
			"decode manifest",
			recordType+" record is malformed",
		)
	}
	if value.ResolvedAt != nil {
		parsed, err := time.Parse(time.RFC3339Nano, *value.ResolvedAt)
		if err != nil {
			return provenanceWire{}, protocolError(
				"decode manifest",
				"provenance timestamp is malformed",
			)
		}
		canonical := parsed.Format(time.RFC3339Nano)
		value.ResolvedAt = &canonical
	}
	return value, nil
}

func decodeManifest(
	data []byte,
	maxBytes uint64,
	maxFiles int,
	configuredPathLimits ...portablePathLimits,
) (validatedManifest, error) {
	if maxFiles < 1 {
		return validatedManifest{}, preconditionError(
			"validate manifest",
			"manifest entry limit must be positive",
		)
	}
	pathLimits, err := selectPortablePathLimits(configuredPathLimits)
	if err != nil {
		return validatedManifest{}, err
	}
	maxRecords, overflow := multiplyUint64(uint64(maxFiles), 3)
	if overflow {
		return validatedManifest{}, preconditionError(
			"validate manifest",
			"manifest record limit overflows",
		)
	}
	maxRecords, overflow = addUint64(maxRecords, 2)
	if overflow {
		return validatedManifest{}, preconditionError(
			"validate manifest",
			"manifest record limit overflows",
		)
	}
	scanner := newMetadataSliceScanner(data, maxBytes, maxRecords)

	var manifest validatedManifest
	var header *manifestHeaderWire
	var provenanceSeen bool
	var stage manifestRecordStage
	var prefixPath, provenancePath, directoryPath, filePath, symlinkPath string
	var prefixSeen, provenancePathSeen, directorySeen, fileSeen, symlinkSeen bool
	var entryCount uint64
	var pathBytes uint64
	paths, err := newManifestPathIndex(maxFiles, pathLimits)
	if err != nil {
		return validatedManifest{}, err
	}

	for {
		line, done, err := scanner.next()
		if err != nil {
			return validatedManifest{}, protocolError("decode manifest", err.Error())
		}
		if done {
			break
		}
		recordType, kind, err := decodeRecordEnvelope(line)
		if err != nil {
			return validatedManifest{}, err
		}
		if scanner.records == 1 && recordType != "manifest_header" {
			return validatedManifest{}, protocolError(
				"validate manifest",
				"manifest header must be the first record",
			)
		}
		if scanner.records == 2 && recordType != "provenance" {
			return validatedManifest{}, protocolError(
				"validate manifest",
				"manifest provenance must follow its header",
			)
		}

		switch recordType {
		case "manifest_header":
			if scanner.records != 1 || header != nil {
				return validatedManifest{}, protocolError(
					"validate manifest",
					"manifest contains a misplaced or duplicate header",
				)
			}
			var value manifestHeaderWire
			if err := decodeStrictJSON(line, &value); err != nil {
				return validatedManifest{}, protocolError("decode manifest", "manifest header is malformed")
			}
			if value.ManifestSchema != defaultManifestSchema {
				return validatedManifest{}, protocolError("validate manifest", "manifest schema is unsupported")
			}
			if uint64(value.EntryCount) > uint64(maxFiles) {
				return validatedManifest{}, preconditionError(
					"validate manifest",
					"manifest exceeds the configured entry limit",
				)
			}
			if !bytes.Equal(line, appendManifestHeaderRecord(nil, value)) {
				return validatedManifest{}, protocolError(
					"decode manifest",
					"manifest header is not canonically encoded",
				)
			}
			header = &value
		case "provenance":
			if provenanceSeen || scanner.records != 2 {
				return validatedManifest{}, protocolError(
					"validate manifest",
					"manifest contains misplaced or duplicate provenance",
				)
			}
			value, err := decodeProvenanceRecord(line, recordType)
			if err != nil {
				return validatedManifest{}, err
			}
			if !bytes.Equal(line, appendProvenanceRecord(nil, recordType, value)) {
				return validatedManifest{}, protocolError(
					"decode manifest",
					"provenance record is not canonically encoded",
				)
			}
			provenanceSeen = true
		case "prefix_provenance":
			if err := advanceManifestRecordStage(&stage, manifestStagePrefixProvenance); err != nil {
				return validatedManifest{}, err
			}
			value, err := decodeProvenanceRecord(line, recordType)
			if err != nil {
				return validatedManifest{}, err
			}
			if !bytes.Equal(line, appendProvenanceRecord(nil, recordType, value)) {
				return validatedManifest{}, protocolError(
					"decode manifest",
					"prefix provenance is not canonically encoded",
				)
			}
			if err := requireAscendingPath(prefixPath, value.Prefix, prefixSeen); err != nil {
				return validatedManifest{}, err
			}
			if err := validatePortablePath(
				strings.TrimRight(value.Prefix, "/"),
				pathLimits,
			); err != nil {
				return validatedManifest{}, err
			}
			if err := addMetadataPathBytes(&pathBytes, uint64(len(value.Prefix)), maxBytes); err != nil {
				return validatedManifest{}, err
			}
			prefixPath, prefixSeen = value.Prefix, true
		case "path_provenance":
			if err := advanceManifestRecordStage(&stage, manifestStagePathProvenance); err != nil {
				return validatedManifest{}, err
			}
			value, err := decodeProvenanceRecord(line, recordType)
			if err != nil {
				return validatedManifest{}, err
			}
			if !bytes.Equal(line, appendProvenanceRecord(nil, recordType, value)) {
				return validatedManifest{}, protocolError(
					"decode manifest",
					"path provenance is not canonically encoded",
				)
			}
			if err := requireAscendingPath(provenancePath, value.Path, provenancePathSeen); err != nil {
				return validatedManifest{}, err
			}
			if err := validatePortablePath(value.Path, pathLimits); err != nil {
				return validatedManifest{}, err
			}
			if err := addMetadataPathBytes(&pathBytes, uint64(len(value.Path)), maxBytes); err != nil {
				return validatedManifest{}, err
			}
			provenancePath, provenancePathSeen = value.Path, true
		case "directory":
			if err := advanceManifestRecordStage(&stage, manifestStageDirectory); err != nil {
				return validatedManifest{}, err
			}
			if err := incrementManifestEntryCount(&entryCount, uint64(maxFiles)); err != nil {
				return validatedManifest{}, err
			}
			var value pathModeWire
			if err := decodeStrictJSON(line, &value); err != nil {
				return validatedManifest{}, protocolError("decode manifest", "directory record is malformed")
			}
			mode, err := parseMode(value.Mode)
			if err != nil {
				return validatedManifest{}, err
			}
			directory := directoryEntry{Mode: mode, Path: value.Path}
			if !bytes.Equal(line, appendDirectoryRecord(nil, directory)) {
				return validatedManifest{}, protocolError(
					"decode manifest",
					"directory record is not canonically encoded",
				)
			}
			if err := requireAscendingPath(directoryPath, value.Path, directorySeen); err != nil {
				return validatedManifest{}, err
			}
			if err := paths.insert(value.Path, manifestPathDirectory, ""); err != nil {
				return validatedManifest{}, err
			}
			if err := addMetadataPathBytes(&pathBytes, uint64(len(value.Path)), maxBytes); err != nil {
				return validatedManifest{}, err
			}
			manifest.Directories = append(manifest.Directories, directory)
			directoryPath, directorySeen = value.Path, true
		case "file":
			if err := advanceManifestRecordStage(&stage, manifestStageFile); err != nil {
				return validatedManifest{}, err
			}
			if err := incrementManifestEntryCount(&entryCount, uint64(maxFiles)); err != nil {
				return validatedManifest{}, err
			}
			file, err := decodeManifestFile(line, kind)
			if err != nil {
				return validatedManifest{}, err
			}
			if err := requireAscendingPath(filePath, file.Path, fileSeen); err != nil {
				return validatedManifest{}, err
			}
			if err := paths.insert(file.Path, manifestPathFile, ""); err != nil {
				return validatedManifest{}, err
			}
			if err := addMetadataPathBytes(&pathBytes, uint64(len(file.Path)), maxBytes); err != nil {
				return validatedManifest{}, err
			}
			nextTotal, overflow := addUint64(manifest.TotalSize, file.Size)
			if overflow {
				return validatedManifest{}, protocolError(
					"validate manifest",
					"manifest total size overflows",
				)
			}
			manifest.TotalSize = nextTotal
			manifest.Files = append(manifest.Files, file)
			filePath, fileSeen = file.Path, true
		case "symlink":
			if err := advanceManifestRecordStage(&stage, manifestStageSymlink); err != nil {
				return validatedManifest{}, err
			}
			if err := incrementManifestEntryCount(&entryCount, uint64(maxFiles)); err != nil {
				return validatedManifest{}, err
			}
			var value symlinkWire
			if err := decodeStrictJSON(line, &value); err != nil {
				return validatedManifest{}, protocolError("decode manifest", "symlink record is malformed")
			}
			mode, err := parseMode(value.Mode)
			if err != nil {
				return validatedManifest{}, err
			}
			symlink := symlinkEntry{Mode: mode, Path: value.Path, Target: value.Target}
			if !bytes.Equal(line, appendSymlinkRecord(nil, symlink)) {
				return validatedManifest{}, protocolError(
					"decode manifest",
					"symlink record is not canonically encoded",
				)
			}
			if err := requireAscendingPath(symlinkPath, value.Path, symlinkSeen); err != nil {
				return validatedManifest{}, err
			}
			if err := paths.insert(
				value.Path,
				manifestPathSymlink,
				value.Target,
			); err != nil {
				return validatedManifest{}, err
			}
			pathAndTargetBytes, overflow := addUint64(
				uint64(len(value.Path)),
				uint64(len(value.Target)),
			)
			if overflow {
				return validatedManifest{}, preconditionError(
					"validate manifest",
					"manifest path bytes overflow",
				)
			}
			if err := addMetadataPathBytes(&pathBytes, pathAndTargetBytes, maxBytes); err != nil {
				return validatedManifest{}, err
			}
			manifest.Symlinks = append(manifest.Symlinks, symlink)
			symlinkPath, symlinkSeen = value.Path, true
		default:
			return validatedManifest{}, protocolError(
				"decode manifest",
				"manifest contains an unknown record type",
			)
		}
	}
	if header == nil {
		return validatedManifest{}, protocolError("validate manifest", "manifest has no header")
	}
	if !provenanceSeen {
		return validatedManifest{}, protocolError("validate manifest", "manifest has no provenance")
	}
	if entryCount != uint64(header.EntryCount) {
		return validatedManifest{}, protocolError(
			"validate manifest",
			"manifest entry count does not match its header",
		)
	}
	if err := paths.validateSymlinks(); err != nil {
		return validatedManifest{}, err
	}
	if manifest.TotalSize != header.TotalSize {
		return validatedManifest{}, protocolError(
			"validate manifest",
			"manifest total size does not match its header",
		)
	}
	return manifest, nil
}

func decodeManifestFile(
	line []byte,
	kind string,
) (manifestFile, error) {
	switch fileKind(kind) {
	case fileKindChunk:
		var value fileChunkWire
		if err := decodeStrictJSON(line, &value); err != nil {
			return manifestFile{}, protocolError("decode manifest", "chunk file record is malformed")
		}
		mode, err := parseMode(value.Mode)
		if err != nil {
			return manifestFile{}, err
		}
		if value.Chunk.Offset != 0 {
			return manifestFile{}, protocolError("validate manifest", "single-chunk file has a nonzero offset")
		}
		if value.Chunk.Length > ChunkSize {
			return manifestFile{}, protocolError("validate manifest", "single-chunk file exceeds the chunk size")
		}
		if value.Chunk.Length == 0 && value.Chunk.Digest != blake3EmptyDigest {
			return manifestFile{}, integrityError(
				"validate manifest",
				"empty file does not use the BLAKE3 empty digest",
			)
		}
		if err := validateTarget(value.Chunk.Target, value.Chunk.Digest); err != nil {
			return manifestFile{}, err
		}
		file := manifestFile{
			Kind: fileKindChunk, Chunk: value.Chunk, Mode: mode, Path: value.Path, Size: value.Chunk.Length,
		}
		if !bytes.Equal(line, appendManifestFileRecord(nil, file)) {
			return manifestFile{}, protocolError(
				"decode manifest",
				"chunk file record is not canonically encoded",
			)
		}
		return file, nil
	case fileKindChunkmap:
		var value fileChunkmapWire
		if err := decodeStrictJSON(line, &value); err != nil {
			return manifestFile{}, protocolError("decode manifest", "chunkmap file record is malformed")
		}
		mode, err := parseMode(value.Mode)
		if err != nil {
			return manifestFile{}, err
		}
		if err := validateTarget(value.Target, value.Digest); err != nil {
			return manifestFile{}, err
		}
		file := manifestFile{
			Kind: fileKindChunkmap, Digest: value.Digest, Mode: mode,
			Path: value.Path, Size: value.Size, Target: value.Target,
		}
		if !bytes.Equal(line, appendManifestFileRecord(nil, file)) {
			return manifestFile{}, protocolError(
				"decode manifest",
				"chunkmap file record is not canonically encoded",
			)
		}
		return file, nil
	case fileKindSlabmap:
		return manifestFile{}, unsupportedError("validate manifest", "slabmap files are not supported")
	default:
		return manifestFile{}, protocolError("decode manifest", "file record has an unknown kind")
	}
}

func decodeChunkmap(
	data []byte,
	maxBytes uint64,
	expectedSize uint64,
	maxChunks int,
) (chunkmap, error) {
	if maxChunks < 0 {
		return chunkmap{}, preconditionError(
			"validate chunkmap",
			"chunk limit must not be negative",
		)
	}
	maxRecords, overflow := addUint64(uint64(maxChunks), 1)
	if overflow {
		return chunkmap{}, preconditionError(
			"validate chunkmap",
			"chunkmap record limit overflows",
		)
	}
	scanner := newMetadataSliceScanner(data, maxBytes, maxRecords)
	var result chunkmap
	var header *chunkmapHeaderWire
	var offset uint64
	for {
		line, done, err := scanner.next()
		if err != nil {
			return chunkmap{}, protocolError("decode chunkmap", err.Error())
		}
		if done {
			break
		}
		recordType, _, err := decodeRecordEnvelope(line)
		if err != nil {
			return chunkmap{}, err
		}
		if scanner.records == 1 && recordType != "chunkmap_header" {
			return chunkmap{}, protocolError(
				"validate chunkmap",
				"chunkmap header must be the first record",
			)
		}
		switch recordType {
		case "chunkmap_header":
			if scanner.records != 1 || header != nil {
				return chunkmap{}, protocolError(
					"validate chunkmap",
					"chunkmap contains a misplaced or duplicate header",
				)
			}
			var value chunkmapHeaderWire
			if err := decodeStrictJSON(line, &value); err != nil {
				return chunkmap{}, protocolError("decode chunkmap", "chunkmap header is malformed")
			}
			if uint64(value.ChunkCount) > uint64(maxChunks) {
				return chunkmap{}, preconditionError(
					"validate chunkmap",
					"chunkmap exceeds the configured chunk limit",
				)
			}
			if value.FileSize != expectedSize {
				return chunkmap{}, protocolError(
					"validate chunkmap",
					"chunkmap size does not match the manifest",
				)
			}
			if !bytes.Equal(line, appendChunkmapHeaderRecord(nil, value)) {
				return chunkmap{}, protocolError(
					"decode chunkmap",
					"chunkmap header is not canonically encoded",
				)
			}
			header = &value
		case "chunk":
			if header == nil {
				return chunkmap{}, protocolError(
					"validate chunkmap",
					"chunkmap chunk precedes its header",
				)
			}
			if len(result.Chunks) == maxChunks {
				return chunkmap{}, preconditionError(
					"validate chunkmap",
					"chunkmap exceeds the configured chunk limit",
				)
			}
			var value chunkRecordWire
			if err := decodeStrictJSON(line, &value); err != nil {
				return chunkmap{}, protocolError("decode chunkmap", "chunk record is malformed")
			}
			chunk := chunkEntry{
				Digest: value.Digest,
				Length: value.Length,
				Offset: value.Offset,
				Target: value.Target,
			}
			if !bytes.Equal(line, appendChunkRecord(nil, chunk)) {
				return chunkmap{}, protocolError(
					"decode chunkmap",
					"chunk record is not canonically encoded",
				)
			}
			if err := validateTarget(value.Target, value.Digest); err != nil {
				return chunkmap{}, err
			}
			if chunk.Length == 0 || chunk.Length > ChunkSize {
				return chunkmap{}, protocolError(
					"validate chunkmap",
					"chunk length is outside the supported range",
				)
			}
			if chunk.Offset != offset {
				return chunkmap{}, protocolError(
					"validate chunkmap",
					"chunkmap is not ordered and contiguous",
				)
			}
			nextOffset, overflow := addUint64(offset, chunk.Length)
			if overflow {
				return chunkmap{}, protocolError("validate chunkmap", "chunk offset overflows")
			}
			offset = nextOffset
			result.Chunks = append(result.Chunks, chunk)
		default:
			return chunkmap{}, protocolError(
				"decode chunkmap",
				"chunkmap contains an unknown record type",
			)
		}
	}
	if header == nil {
		return chunkmap{}, protocolError("validate chunkmap", "chunkmap has no header")
	}
	if uint64(header.ChunkCount) != uint64(len(result.Chunks)) {
		return chunkmap{}, protocolError(
			"validate chunkmap",
			"chunkmap count does not match its header",
		)
	}
	if offset != expectedSize {
		return chunkmap{}, protocolError("validate chunkmap", "chunkmap does not cover the file")
	}
	result.FileSize = header.FileSize
	return result, nil
}

func decodeRecordEnvelope(
	line []byte,
) (string, string, error) {
	const typePrefix = `{"_type":"`
	if !bytes.HasPrefix(line, []byte(typePrefix)) {
		return "", "", protocolError(
			"decode metadata",
			"JSONL record has no canonical type discriminator",
		)
	}
	remainder := line[len(typePrefix):]
	typeEnd := bytes.IndexByte(remainder, '"')
	if typeEnd < 1 || typeEnd > 32 {
		return "", "", protocolError(
			"decode metadata",
			"JSONL record has no valid type",
		)
	}
	recordType := string(remainder[:typeEnd])
	if recordType != "file" {
		return recordType, "", nil
	}

	const kindPrefix = `,"_kind":"`
	remainder = remainder[typeEnd+1:]
	if !bytes.HasPrefix(remainder, []byte(kindPrefix)) {
		return "", "", protocolError(
			"decode metadata",
			"file record has no canonical kind discriminator",
		)
	}
	remainder = remainder[len(kindPrefix):]
	kindEnd := bytes.IndexByte(remainder, '"')
	if kindEnd < 1 || kindEnd > 16 {
		return "", "", protocolError(
			"decode metadata",
			"file record has no valid kind",
		)
	}
	return recordType, string(remainder[:kindEnd]), nil
}

func parseMode(value string) (uint16, error) {
	if value == "" {
		return 0, protocolError("decode manifest", "entry mode is empty")
	}
	parsed, err := strconv.ParseUint(value, 8, 16)
	if err != nil {
		return 0, protocolError("decode manifest", "entry mode is not valid octal")
	}
	return uint16(parsed), nil
}

func addUint64(left, right uint64) (uint64, bool) {
	value := left + right
	return value, value < left
}

func multiplyUint64(left, right uint64) (uint64, bool) {
	if left != 0 && right > ^uint64(0)/left {
		return 0, true
	}
	return left * right, false
}

type manifestHeaderWire struct {
	Type           string `json:"_type"`
	EntryCount     uint32 `json:"entry_count"`
	ManifestSchema string `json:"manifest_schema"`
	TotalSize      uint64 `json:"total_size"`
}

type provenanceWire struct {
	Type                  string  `json:"_type"`
	Path                  string  `json:"path,omitempty"`
	Prefix                string  `json:"prefix,omitempty"`
	ResolvedAt            *string `json:"resolved_at,omitempty"`
	SourceFingerprint     string  `json:"source_fingerprint"`
	SourceFingerprintType string  `json:"source_fingerprint_type"`
	SourceURI             string  `json:"source_uri"`
}

type pathModeWire struct {
	Type string `json:"_type"`
	Mode string `json:"mode"`
	Path string `json:"path"`
}

type symlinkWire struct {
	Type   string `json:"_type"`
	Mode   string `json:"mode"`
	Path   string `json:"path"`
	Target string `json:"target"`
}

type fileChunkWire struct {
	Type  string     `json:"_type"`
	Kind  string     `json:"_kind"`
	Chunk chunkEntry `json:"chunk"`
	Mode  string     `json:"mode"`
	Path  string     `json:"path"`
}

type fileChunkmapWire struct {
	Type   string       `json:"_type"`
	Kind   string       `json:"_kind"`
	Digest Digest       `json:"digest"`
	Mode   string       `json:"mode"`
	Path   string       `json:"path"`
	Size   uint64       `json:"size"`
	Target objectTarget `json:"target"`
}

type chunkmapHeaderWire struct {
	Type       string `json:"_type"`
	ChunkCount uint32 `json:"chunk_count"`
	FileSize   uint64 `json:"file_size"`
}

type chunkRecordWire struct {
	Type   string       `json:"_type"`
	Digest Digest       `json:"digest"`
	Length uint64       `json:"length"`
	Offset uint64       `json:"offset"`
	Target objectTarget `json:"target"`
}
