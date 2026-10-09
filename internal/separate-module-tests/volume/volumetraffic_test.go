package volume_test

import (
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/basetenlabs/baseten-go/internal/volume"
	"github.com/basetenlabs/baseten-go/internal/volume/transfer"
)

func TestPushTransferredBytes(t *testing.T) {
	root := t.TempDir()
	body := []byte("the same chunk in two files")
	writeFile(t, root, "a", body, 0o644)
	writeFile(t, root, "b", body, 0o644)
	writeFile(t, root, "empty", nil, 0o644)
	fake := newFakeService(t)
	for _, mode := range []string{"upload", "reuse", "server-existing", "no-callback"} {
		t.Run(mode, func(t *testing.T) {
			opts := pushOptions(root, fake)
			if mode == "server-existing" || mode == "no-callback" {
				opts.DownloadObject = nil
				opts.Decompress = nil
			}
			var last volume.Progress
			if mode != "no-callback" {
				opts.Progress = func(p volume.Progress) {
					if p.Phase != volume.PhaseUpload {
						return
					}
					if p.TransferredBytes < last.TransferredBytes {
						t.Error("traffic decreased")
					}
					last = p
				}
			}
			result, err := transfer.Push(context.Background(), fake.client(t), opts)
			if err != nil {
				t.Fatal(err)
			}
			want := int64(len(body))
			if mode == "reuse" {
				want = 0
			}
			if result.TransferredBytes != want {
				t.Fatalf("result transferred %d, want %d", result.TransferredBytes, want)
			}
			if mode != "no-callback" && last.TransferredBytes != want {
				t.Fatalf("progress transferred %d, want %d", last.TransferredBytes, want)
			}
			if result.Bytes != int64(2*len(body)) {
				t.Fatalf("logical bytes = %d", result.Bytes)
			}
		})
	}
}

func TestPullTransferredBytes(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		name := "identity"
		if compressed {
			name = "compressed"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			body := patternBytes(volume.ChunkSize + 1024)
			writeFile(t, root, "large", body, 0o644)
			writeFile(t, root, "empty", nil, 0o644)
			fake := newFakeService(t)
			fake.compressChunks = compressed
			if _, err := transfer.Push(context.Background(), fake.client(t), pushOptions(root, fake)); err != nil {
				t.Fatal(err)
			}
			var want int64
			for _, span := range volume.ChunkRanges(uint64(len(body))) {
				chunk := body[span.Offset : span.Offset+span.Length]
				if compressed {
					chunk = zstdCompress(t, chunk)
				}
				want += int64(len(chunk))
			}
			dest := filepath.Join(t.TempDir(), "download")
			for _, mode := range []string{"download", "reuse", "stream", "no-callback"} {
				t.Run(mode, func(t *testing.T) {
					opts := pullOptions(dest, fake)
					opts.Overwrite = true
					if mode == "no-callback" {
						opts.DestDir = filepath.Join(t.TempDir(), "fresh")
					}
					if mode == "stream" {
						opts.DestDir = ""
						opts.Overwrite = false
						opts.EntryHandler = func(_ context.Context, _ volume.Entry, r io.Reader) error {
							_, err := io.Copy(io.Discard, r)
							return err
						}
					}
					var last volume.Progress
					if mode != "no-callback" {
						opts.Progress = func(p volume.Progress) {
							if p.Phase != volume.PhaseDownload {
								return
							}
							if p.TransferredBytes < last.TransferredBytes {
								t.Error("traffic decreased")
							}
							last = p
						}
					}
					result, err := transfer.Pull(context.Background(), fake.client(t), opts)
					if err != nil {
						t.Fatal(err)
					}
					expected := want
					if mode == "reuse" {
						expected = 0
					}
					if result.TransferredBytes != expected {
						t.Fatalf("result transferred %d, want %d", result.TransferredBytes, expected)
					}
					if mode != "no-callback" && last.TransferredBytes != expected {
						t.Fatalf("progress transferred %d, want %d", last.TransferredBytes, expected)
					}
					if result.Bytes != int64(len(body)) {
						t.Fatalf("logical bytes = %d", result.Bytes)
					}
				})
			}
		})
	}
}

func TestEmptyTransferHasNoPayload(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "empty", nil, 0o644)
	fake := newFakeService(t)
	pushed, err := transfer.Push(context.Background(), fake.client(t), pushOptions(root, fake))
	if err != nil {
		t.Fatal(err)
	}
	pulled, err := transfer.Pull(context.Background(), fake.client(t), pullOptions(filepath.Join(t.TempDir(), "dest"), fake))
	if err != nil {
		t.Fatal(err)
	}
	if pushed.TransferredBytes != 0 || pulled.TransferredBytes != 0 {
		t.Fatalf("empty transfer counted payload: push=%d pull=%d", pushed.TransferredBytes, pulled.TransferredBytes)
	}
}
