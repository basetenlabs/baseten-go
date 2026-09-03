package volume

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

// stubDownload serves one object with the given media type and stored size.
func stubDownload(body []byte, contentType string, size int64) ObjectDownloader {
	return func(context.Context, ObjectDownload) (*ObjectResult, error) {
		return &ObjectResult{
			Body:        io.NopCloser(bytes.NewReader(body)),
			ContentType: contentType,
			Size:        size,
		}, nil
	}
}

// TestFetchObjectReadsAKnownSizeIntoOneBuffer pins the sized read: with the
// content length known up front, the whole body comes back in a buffer
// allocated once at that size. The capacity assertion is what fails under a
// growing read, whose doubling leaves the final buffer larger than the
// content it holds.
func TestFetchObjectReadsAKnownSizeIntoOneBuffer(t *testing.T) {
	content := bytes.Repeat([]byte{0xA5}, 8000)
	got, err := FetchObject(context.Background(), stubDownload(content, "application/test", 8000), nil,
		ObjectDownload{Key: "k", ExpectedSize: 8000}, 8000)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("read %d bytes, want the %d byte body", len(got), len(content))
	}
	if cap(got) > 8001 {
		t.Errorf("buffer capacity is %d for an 8000 byte object; the size was known, so one allocation of 8001 suffices", cap(got))
	}
}

// TestFetchObjectSizedReadCoversCompressedObjects: the expected size is the
// content's length, not the stored object's, so the sized read applies behind
// a decompressor too — where the stored size says nothing useful.
func TestFetchObjectSizedReadCoversCompressedObjects(t *testing.T) {
	content := bytes.Repeat([]byte{0x5A}, 4000)
	// The stub decompressor ignores the stored bytes and yields the content,
	// standing in for a real one without pulling a compression library into
	// this module.
	decompress := func(io.Reader) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(content)), nil
	}
	got, err := FetchObject(context.Background(), stubDownload([]byte("stored"), "application/test+zstd", 6), decompress,
		ObjectDownload{Key: "k", ExpectedSize: 4000}, 4000)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("read %d bytes, want the %d byte content", len(got), len(content))
	}
	if cap(got) > 4001 {
		t.Errorf("buffer capacity is %d for a 4000 byte object; the size was known, so one allocation of 4001 suffices", cap(got))
	}
}

// TestFetchObjectStillRefusesAnOverrunWithASizeHint: a body longer than the
// expected size must still be read far enough for the bound check to name the
// overrun — the sized buffer must not turn "too large" into a short read or a
// new failure shape. The stored size is left at zero so the read is what
// detects it, not the earlier length comparison.
func TestFetchObjectStillRefusesAnOverrunWithASizeHint(t *testing.T) {
	content := bytes.Repeat([]byte{0x11}, 200)
	_, err := FetchObject(context.Background(), stubDownload(content, "application/test", 0), nil,
		ObjectDownload{Key: "k", ExpectedSize: 100}, 100)
	if err == nil {
		t.Fatal("a 200 byte object passed a 100 byte bound")
	}
	if !strings.Contains(err.Error(), "larger than the 100 byte limit") {
		t.Errorf("expected the size bound to name the refusal, got %v", err)
	}
}

// TestSizedReadMatchesGrowingRead is the differential: whatever the body and
// whatever the caller expected, the sized path must return byte-identical
// content to the growing read it replaces — a body shorter than expected, one
// past the expectation, one far past it, and the empty body included. The
// size is an allocation hint, never a truncation.
func TestSizedReadMatchesGrowingRead(t *testing.T) {
	pattern := func(n int) []byte {
		data := make([]byte, n)
		for i := range data {
			data[i] = byte(i%251) ^ byte(i>>7)
		}
		return data
	}
	for _, c := range []struct {
		body int
		size int64
	}{
		{0, 5},   // empty body, size expected
		{5, 5},   // exact
		{3, 5},   // shorter than expected
		{6, 5},   // one past the expectation, the spare-byte edge
		{800, 5}, // far past it, through the fallback read
		{5, 0},   // no expectation: the growing path itself
		{800, 0}, // no expectation, larger body
		{0, 0},   // nothing at all
	} {
		body := pattern(c.body)
		want, wantErr := io.ReadAll(bytes.NewReader(body))
		got, gotErr := readAllSized(bytes.NewReader(body), c.size)
		if (gotErr == nil) != (wantErr == nil) {
			t.Errorf("body %d size %d: sized read error %v, growing read error %v", c.body, c.size, gotErr, wantErr)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("body %d size %d: sized read returned %d bytes, growing read %d, contents differ", c.body, c.size, len(got), len(want))
		}
	}
}
