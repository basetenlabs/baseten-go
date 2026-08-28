package volume

import (
	"context"
	"testing"
)

type classifiedErrorBody struct {
	err error
}

func (body *classifiedErrorBody) Read([]byte) (int, error) {
	return 0, body.err
}

func (*classifiedErrorBody) Close() error {
	return nil
}

func TestObjectReaderBoundaryRetainsOnlyClassification(t *testing.T) {
	fixture := newClassifiedBoundaryError()
	reader := ObjectReaderFunc(func(context.Context, ObjectRequest) (Object, error) {
		return Object{}, fixture.boundaryErr
	})
	_, err := newTestVolumeClient(t).readVerifiedObject(
		t.Context(),
		reader,
		testDigest(0x31),
		expectedObject{kind: ObjectKindChunk, maxDecodedBytes: 1},
		newByteGate(2<<20),
	)
	assertClassifiedBoundaryError(
		t,
		err,
		fixture.discarded,
		fixture.classification,
	)
}

func TestObjectReaderBodyBoundaryRetainsOnlyClassification(t *testing.T) {
	fixture := newClassifiedBoundaryError()
	reader := ObjectReaderFunc(func(context.Context, ObjectRequest) (Object, error) {
		return Object{
			Body:     &classifiedErrorBody{err: fixture.boundaryErr},
			Size:     -1,
			Kind:     ObjectKindChunk,
			Encoding: ObjectEncodingIdentity,
		}, nil
	})
	_, err := newTestVolumeClient(t).readVerifiedObject(
		t.Context(),
		reader,
		testDigest(0x34),
		expectedObject{kind: ObjectKindChunk, maxDecodedBytes: 1},
		newByteGate(2<<20),
	)
	assertClassifiedBoundaryError(
		t,
		err,
		fixture.discarded,
		fixture.classification,
	)
}
