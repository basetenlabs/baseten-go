package volume

import (
	"context"
	"path/filepath"
	"testing"
)

func TestPullRequiresDecoderBeforeObjectRead(t *testing.T) {
	client, err := New(Options{NewHasher: newTestHasher})
	if err != nil {
		t.Fatal(err)
	}
	readerCalled := false
	_, err = client.Pull(t.Context(), PullOptions{
		Objects: ObjectReaderFunc(func(context.Context, ObjectRequest) (Object, error) {
			readerCalled = true
			return Object{}, nil
		}),
		Destination: filepath.Join(t.TempDir(), "output"),
	})
	if err == nil || !IsCode(err, ErrorInvalidArgument) {
		t.Fatalf("pull without decoder error = %v, want %s", err, ErrorInvalidArgument)
	}
	if readerCalled {
		t.Fatal("pull without decoder reached its object reader")
	}
}
