//go:build windows

package volume

import (
	"context"
	"testing"
)

func TestWindowsPullRemainsExplicitlyUnsupported(t *testing.T) {
	reader := ObjectReaderFunc(func(context.Context, ObjectRequest) (Object, error) {
		panic("unsupported pull must not read objects")
	})
	_, err := (&Client{}).Pull(t.Context(), PullOptions{
		Objects:     reader,
		Destination: "unused",
	})
	if err == nil || !IsCode(err, ErrorUnsupported) {
		t.Fatalf("pull error = %v, want %s", err, ErrorUnsupported)
	}
}
