package volume

import (
	"context"
	"errors"
	"io"
	"time"
)

type boundedBuffer struct {
	data     []byte
	max      int
	exceeded bool
}

func (buffer *boundedBuffer) Write(value []byte) (int, error) {
	if len(value) > buffer.max-len(buffer.data) {
		buffer.exceeded = true
		return 0, errors.New("decoded object exceeds limit")
	}
	buffer.data = append(buffer.data, value...)
	return len(value), nil
}

type verifiedObjectBody struct {
	data            []byte
	permit          *bytePermit
	observation     TransferObservation
	transferLatency time.Duration
}

func (body *verifiedObjectBody) release() {
	if body == nil {
		return
	}
	clear(body.data)
	body.data = nil
	body.permit.release()
	body.permit = nil
}

type expectedObject struct {
	kind              ObjectKind
	maxDecodedBytes   uint64
	exactDecodedBytes *uint64
}

type objectReadAttempt struct {
	observation TransferObservation
	latency     time.Duration
}

func (c *Client) readVerifiedObject(
	ctx context.Context,
	reader ObjectReader,
	digest Digest,
	expected expectedObject,
	budget *byteGate,
) (*verifiedObjectBody, error) {
	body, _, err := c.readVerifiedObjectObserved(ctx, reader, digest, expected, budget)
	return body, err
}

func (c *Client) readVerifiedObjectObserved(
	ctx context.Context,
	reader ObjectReader,
	digest Digest,
	expected expectedObject,
	budget *byteGate,
) (*verifiedObjectBody, objectReadAttempt, error) {
	var attempt objectReadAttempt
	if err := ctx.Err(); err != nil {
		return nil, attempt, canceledError("read volume object", err)
	}

	limits := decodeLimits(expected.maxDecodedBytes)
	if limits.MaxEncodedBytes < expected.maxDecodedBytes {
		return nil, attempt, preconditionError("read volume object", "object size limit overflows")
	}
	memoryBytes, overflow := addUint64(expected.maxDecodedBytes, limits.MaxMemoryBytes)
	if overflow {
		return nil, attempt, preconditionError("read volume object", "object memory limit overflows")
	}
	if expected.maxDecodedBytes > uint64(^uint(0)>>1) ||
		limits.MaxEncodedBytes >= uint64(^uint64(0)>>1) {
		return nil, attempt, preconditionError(
			"read volume object",
			"object limit is too large for this platform",
		)
	}
	if budget == nil {
		return nil, attempt, protocolError(
			"read volume object",
			"operation byte budget is not initialized",
		)
	}
	permit, err := budget.acquire(ctx, memoryBytes)
	if err != nil {
		return nil, attempt, byteGateError("read volume object", err)
	}
	succeeded := false
	defer func() {
		if !succeeded {
			permit.release()
		}
	}()

	requestStarted := time.Now()
	object, readErr := reader.ReadObject(ctx, ObjectRequest{
		Digest: digest,
		Kind:   expected.kind,
	})
	timer := &objectTransferTimer{elapsed: time.Since(requestStarted)}
	if object.Body != nil {
		object.Body = &timedObjectBody{body: object.Body, timer: timer}
	}
	closeObject := func() error {
		var closeErr error
		if object.Body != nil {
			closeErr = object.Body.Close()
		}
		attempt.latency = timer.elapsed
		attempt.observation = snapshotTransferObservation(object.Observation)
		return closeErr
	}
	if readErr != nil {
		closeErr := closeObject()
		return nil, attempt, transferError(
			"read volume object",
			errors.Join(readErr, closeErr),
		)
	}
	if object.Body == nil {
		attempt.latency = timer.elapsed
		attempt.observation = snapshotTransferObservation(object.Observation)
		return nil, attempt, protocolError("read volume object", "object response omitted its body")
	}
	if object.Kind != expected.kind {
		_ = closeObject()
		return nil, attempt, protocolError("read volume object", "object response returned the wrong kind")
	}

	maxEncodedBytes := expected.maxDecodedBytes
	switch object.Encoding {
	case ObjectEncodingIdentity:
	case ObjectEncodingZstd:
		maxEncodedBytes = limits.MaxEncodedBytes
	default:
		_ = closeObject()
		return nil, attempt, protocolError(
			"read volume object",
			"object response has an unsupported encoding",
		)
	}
	if object.Size < -1 || (object.Size >= 0 && uint64(object.Size) > maxEncodedBytes) {
		_ = closeObject()
		return nil, attempt, preconditionError(
			"read volume object",
			"stored object exceeds its size limit",
		)
	}

	decoded := &boundedBuffer{
		data: make([]byte, 0, int(expected.maxDecodedBytes)),
		max:  int(expected.maxDecodedBytes),
	}
	encodedSource := &contextCheckedReader{ctx: ctx, reader: object.Body}
	limited := &io.LimitedReader{R: encodedSource, N: int64(maxEncodedBytes)}
	var decodeErr error
	switch object.Encoding {
	case ObjectEncodingIdentity:
		_, decodeErr = io.Copy(decoded, limited)
	case ObjectEncodingZstd:
		decodeErr = c.decoder.Decode(ctx, decoded, limited, limits)
	}
	remainingAtDecodeEnd := limited.N
	encodedBytes := maxEncodedBytes - uint64(limited.N)
	encodedExceeded := false
	if decodeErr == nil {
		var trailing [1]byte
		if count, readErr := encodedSource.Read(trailing[:]); count != 0 {
			if remainingAtDecodeEnd == 0 {
				encodedExceeded = true
			} else {
				decodeErr = errors.New("decoder left trailing encoded data")
			}
		} else if readErr != nil && !errors.Is(readErr, io.EOF) {
			decodeErr = readErr
		}
	}
	closeErr := closeObject()
	if contextErr := contextError("read volume object", ctx.Err()); contextErr != nil {
		return nil, attempt, contextErr
	}
	if encodedExceeded {
		return nil, attempt, preconditionError(
			"read volume object",
			"stored object exceeds its size limit",
		)
	}
	if decoded.exceeded {
		return nil, attempt, preconditionError(
			"read volume object",
			"decoded object exceeds its size limit",
		)
	}
	if decodeErr != nil || closeErr != nil {
		boundaryErr := errors.Join(decodeErr, closeErr)
		if canonicalContextCause(boundaryErr) != nil ||
			adapterErrorFrom(boundaryErr) != nil {
			return nil, attempt, transferError("read volume object", boundaryErr)
		}
		if object.Encoding == ObjectEncodingZstd && decodeErr != nil {
			return nil, attempt, integrityError(
				"decode volume object",
				"compressed object could not be decoded",
			)
		}
		return nil, attempt, transferError(
			"read volume object",
			boundaryErr,
		)
	}
	if object.Size >= 0 && encodedBytes != uint64(object.Size) {
		return nil, attempt, protocolError(
			"read volume object",
			"object response size does not match its metadata",
		)
	}
	if expected.exactDecodedBytes != nil &&
		uint64(len(decoded.data)) != *expected.exactDecodedBytes {
		return nil, attempt, integrityError(
			"verify volume object",
			"decoded chunk length does not match metadata",
		)
	}
	if err := ctx.Err(); err != nil {
		return nil, attempt, canceledError("verify volume object", err)
	}
	actual, err := c.digest(decoded.data)
	if err != nil {
		return nil, attempt, err
	}
	if err := ctx.Err(); err != nil {
		return nil, attempt, canceledError("verify volume object", err)
	}
	if actual != digest {
		return nil, attempt, integrityError(
			"verify volume object",
			"volume object digest does not match metadata",
		)
	}
	succeeded = true
	return &verifiedObjectBody{
		data:            decoded.data,
		permit:          permit,
		observation:     attempt.observation,
		transferLatency: attempt.latency,
	}, attempt, nil
}

type objectTransferTimer struct {
	elapsed time.Duration
}

type timedObjectBody struct {
	body  io.ReadCloser
	timer *objectTransferTimer
}

func (body *timedObjectBody) Read(value []byte) (int, error) {
	started := time.Now()
	read, err := body.body.Read(value)
	body.timer.elapsed += time.Since(started)
	return read, err
}

func (body *timedObjectBody) Close() error {
	started := time.Now()
	err := body.body.Close()
	body.timer.elapsed += time.Since(started)
	return err
}

type contextCheckedReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader *contextCheckedReader) Read(value []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(value)
}
