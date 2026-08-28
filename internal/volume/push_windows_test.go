//go:build windows

package volume

import "testing"

func TestWindowsPushFailsClosedBeforeUsingAdapters(t *testing.T) {
	_, err := (&Client{}).Push(t.Context(), PushOptions{})
	if err == nil || !IsCode(err, ErrorUnsupported) {
		t.Fatalf("push error = %v, want %s", err, ErrorUnsupported)
	}
}
