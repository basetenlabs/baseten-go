package volume

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"unicode/utf8"
)

var (
	errMetadataEmptyRecord    = errors.New("metadata contains an empty record")
	errMetadataInvalidUTF8    = errors.New("metadata is not valid UTF-8")
	errMetadataMissingNewline = errors.New("metadata record is not newline terminated")
	errMetadataRead           = errors.New("metadata cannot be read")
	errMetadataTooLarge       = errors.New("metadata exceeds its byte limit")
	errMetadataTooManyRecords = errors.New("metadata exceeds its record limit")
)

// metadataSliceScanner walks a verified metadata body without first building a
// slice containing every newline-delimited record.
type metadataSliceScanner struct {
	data       []byte
	offset     int
	records    uint64
	maxRecords uint64
	err        error
}

func newMetadataSliceScanner(
	data []byte,
	maxBytes uint64,
	maxRecords uint64,
) *metadataSliceScanner {
	scanner := &metadataSliceScanner{
		data:       data,
		maxRecords: maxRecords,
	}
	if uint64(len(data)) > maxBytes {
		scanner.err = errMetadataTooLarge
	}
	return scanner
}

func (scanner *metadataSliceScanner) next() ([]byte, bool, error) {
	if scanner.err != nil {
		return nil, false, scanner.err
	}
	if scanner.offset == len(scanner.data) {
		return nil, true, nil
	}
	if scanner.records == scanner.maxRecords {
		return nil, false, errMetadataTooManyRecords
	}
	relativeEnd := bytes.IndexByte(scanner.data[scanner.offset:], '\n')
	if relativeEnd < 0 {
		return nil, false, errMetadataMissingNewline
	}
	if relativeEnd == 0 {
		return nil, false, errMetadataEmptyRecord
	}
	line := scanner.data[scanner.offset : scanner.offset+relativeEnd]
	if !utf8.Valid(line) {
		return nil, false, errMetadataInvalidUTF8
	}
	scanner.offset += relativeEnd + 1
	scanner.records++
	return line, false, nil
}

// metadataStreamScanner keeps at most one record in memory. ReadSlice handles
// ordinary records without allocation; oversized buffer fragments are copied
// only after both document and record byte limits have been checked.
type metadataStreamScanner struct {
	reader     *bufio.Reader
	bytesRead  uint64
	records    uint64
	maxBytes   uint64
	maxRecords uint64
}

func newMetadataStreamScanner(
	reader io.Reader,
	maxBytes uint64,
	maxRecords uint64,
) (*metadataStreamScanner, error) {
	if maxBytes >= math.MaxInt64 {
		return nil, errMetadataTooLarge
	}
	bufferBytes := min(maxBytes+1, uint64(32<<10))
	bufferBytes = max(bufferBytes, uint64(1))
	limited := io.LimitReader(reader, int64(maxBytes)+1)
	return &metadataStreamScanner{
		reader:     bufio.NewReaderSize(limited, int(bufferBytes)),
		maxBytes:   maxBytes,
		maxRecords: maxRecords,
	}, nil
}

func (scanner *metadataStreamScanner) next() ([]byte, bool, error) {
	if scanner.records == scanner.maxRecords {
		if _, err := scanner.reader.Peek(1); errors.Is(err, io.EOF) {
			return nil, true, nil
		} else if err != nil {
			return nil, false, errMetadataRead
		}
		return nil, false, errMetadataTooManyRecords
	}
	var record []byte
	var recordBytes uint64
	for {
		fragment, err := scanner.reader.ReadSlice('\n')
		nextBytes, overflow := addUint64(scanner.bytesRead, uint64(len(fragment)))
		if overflow || nextBytes > scanner.maxBytes {
			return nil, false, errMetadataTooLarge
		}
		scanner.bytesRead = nextBytes
		nextRecordBytes, overflow := addUint64(recordBytes, uint64(len(fragment)))
		if overflow || nextRecordBytes > scanner.maxBytes {
			return nil, false, errMetadataTooLarge
		}
		recordBytes = nextRecordBytes

		switch {
		case err == nil:
			fragment = fragment[:len(fragment)-1]
			var line []byte
			if record == nil {
				line = fragment
			} else {
				record = append(record, fragment...)
				line = record
			}
			if len(line) == 0 {
				return nil, false, errMetadataEmptyRecord
			}
			if !utf8.Valid(line) {
				return nil, false, errMetadataInvalidUTF8
			}
			scanner.records++
			return line, false, nil
		case errors.Is(err, bufio.ErrBufferFull):
			record = append(record, fragment...)
		case errors.Is(err, io.EOF):
			if len(fragment) == 0 && len(record) == 0 {
				return nil, true, nil
			}
			return nil, false, errMetadataMissingNewline
		default:
			return nil, false, errMetadataRead
		}
	}
}

func decodeStrictJSON(data []byte, value any) error {
	if !utf8.Valid(data) {
		return errMetadataInvalidUTF8
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	return ensureJSONEOF(decoder)
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); errors.Is(err, io.EOF) {
		return nil
	} else if err == nil {
		return errors.New("unexpected trailing JSON value")
	} else {
		return err
	}
}
