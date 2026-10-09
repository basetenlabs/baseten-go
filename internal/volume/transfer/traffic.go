package transfer

import (
	"context"
	"io"

	"github.com/basetenlabs/baseten-go/internal/volume"
)

func (p *pusher) addTransferred(n int64) {
	p.transferred.Add(n)
	p.progress.AddTransferred(n)
}

func (p *puller) addTransferred(n int64) {
	p.transferred.Add(n)
	p.progress.AddTransferred(n)
}

// Only chunk reads use this wrapper. Count stored bytes before decompression,
// leaving manifest and chunkmap traffic outside the transfer payload metric.
func (p *puller) chunkDownloader() volume.ObjectDownloader {
	return func(ctx context.Context, req volume.ObjectDownload) (*volume.ObjectResult, error) {
		result, err := p.opts.DownloadObject(ctx, req)
		if err != nil {
			return nil, err
		}
		counted := *result
		counted.Body = &trafficReader{ReadCloser: result.Body, onRead: p.addTransferred}
		return &counted, nil
	}
}

type trafficReader struct {
	io.ReadCloser
	onRead func(int64)
}

func (r *trafficReader) Read(buf []byte) (int, error) {
	n, err := r.ReadCloser.Read(buf)
	if n > 0 {
		r.onRead(int64(n))
	}
	return n, err
}
