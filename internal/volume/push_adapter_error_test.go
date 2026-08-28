package volume

import (
	"context"
	"testing"
)

type classifiedErrorSession struct {
	err         error
	publication PublishResult
}

func (session *classifiedErrorSession) MissingObjects(
	context.Context,
	[]Digest,
) ([]Digest, error) {
	return nil, session.err
}

func (*classifiedErrorSession) UploadObject(
	context.Context,
	UploadObject,
) (UploadObjectResult, error) {
	panic("upload session must not be used as an object uploader")
}

func (session *classifiedErrorSession) Publish(
	context.Context,
	Digest,
) (PublishResult, error) {
	return session.publication, session.err
}

func TestObjectUploaderBoundaryRetainsOnlyClassification(t *testing.T) {
	fixture := newClassifiedBoundaryError()
	uploader := ObjectUploaderFunc(func(
		context.Context,
		UploadObject,
	) (UploadObjectResult, error) {
		return UploadObjectResult{}, fixture.boundaryErr
	})
	gate := newAdaptiveRequestGate(adaptiveGateConfig{min: 1, max: 8, initial: 4})
	_, _, err := newTestVolumeClient(t).uploadOne(
		t.Context(),
		uploader,
		&pushObject{kind: ObjectKindChunk, data: []byte("payload")},
		gate,
		newByteGate(1<<20),
	)
	assertClassifiedBoundaryError(
		t,
		err,
		fixture.discarded,
		fixture.classification,
	)
	if got := gate.currentLimit(); got != 2 {
		t.Fatalf("classified stall limit = %d, want 2", got)
	}
}

func TestUploadSessionBoundariesRetainOnlyClassification(t *testing.T) {
	t.Run("missing objects", func(t *testing.T) {
		fixture := newClassifiedBoundaryError()
		session := &classifiedErrorSession{err: fixture.boundaryErr}
		digest := testDigest(0x32)
		_, err := missingObjects(t.Context(), session, map[Digest]*pushObject{
			digest: {digest: digest, kind: ObjectKindChunk},
		})
		assertClassifiedBoundaryError(
			t,
			err,
			fixture.discarded,
			fixture.classification,
		)
	})

	t.Run("publish", func(t *testing.T) {
		fixture := newClassifiedBoundaryError()
		session := &classifiedErrorSession{
			err: fixture.boundaryErr,
			publication: PublishResult{
				Outcome:       PublishOutcomeUnknown,
				FailureReason: PublishFailureReasonTransportFailure,
			},
		}
		_, err := publishPush(
			t.Context(),
			session,
			testDigest(0x33),
			newProgressReporter(OperationPush, nil, nil),
		)
		assertClassifiedBoundaryError(
			t,
			err,
			fixture.discarded,
			fixture.classification,
		)
	})
}
