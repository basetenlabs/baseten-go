package transfer

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/basetenlabs/baseten-go/internal/volume"
	"github.com/basetenlabs/baseten-go/internal/volume/bdn"
)

// credentialMargin is how long before a lease expires the client asks for a
// new one. A download that starts inside the margin should finish inside the
// lease.
const credentialMargin = time.Minute

// credentialRenewalRetry bounds resolve traffic when the service returns a
// lease that is still inside credentialMargin. It is short enough to recover
// from a transient short lease without turning every object request into a
// resolve.
const credentialRenewalRetry = time.Second

type originResolver interface {
	Resolve(context.Context, bdn.ResolveRequest) (*bdn.ResolveResult, error)
}

// origin holds the credential lease a pull reads objects with, and renews it
// when it is about to run out.
//
// Renewing means resolving the pinned ref again. That is safe to repeat
// because resolving changes nothing: it mints credentials and reports where a
// version lives. Pinning is what makes it safe to repeat usefully — resolving
// the same digest twice always names the same bytes, where re-resolving a tag
// could quietly switch versions in the middle of a download.
type origin struct {
	client    originResolver
	ref       bdn.ResolveRequest
	namespace string

	mu    sync.Mutex
	org   string
	lease bdn.Origin
	// renewing indicates whether there is an in-flight renew.
	renewing bool
	// renewAfter rate-limits attempts when a renewal fails or returns another
	// short lease, without permanently disabling recovery.
	renewAfter time.Time
}

func newOrigin(client *bdn.Client, ref bdn.ResolveRequest, org string, lease bdn.Origin) *origin {
	return &origin{client: client, ref: ref, namespace: ref.Namespace, org: org, lease: lease}
}

// request builds a read of one object, renewing the lease first if it is close
// to expiring.
func (o *origin) request(ctx context.Context, target volume.Target, size int64) (volume.ObjectDownload, error) {
	now := time.Now()
	o.mu.Lock()
	org, lease := o.org, o.lease
	renew := !o.renewing && !lease.ExpiresAt.IsZero() &&
		lease.ExpiresAt.Sub(now) < credentialMargin && !now.Before(o.renewAfter)
	if renew {
		o.renewing = true
	}
	o.mu.Unlock()

	if renew {
		resolved, err := o.client.Resolve(ctx, o.ref)

		o.mu.Lock()
		o.renewing = false
		o.renewAfter = time.Now().Add(credentialRenewalRetry)
		if err != nil {
			o.mu.Unlock()
			return volume.ObjectDownload{}, err
		}
		// A zero expiry is a non-expiring lease. Otherwise, a stale resolve
		// must not shorten the usable lifetime of current credentials.
		if resolved.Origin.ExpiresAt.IsZero() ||
			resolved.Origin.ExpiresAt.After(o.lease.ExpiresAt) {
			o.org, o.lease = resolved.Resolved.OrgID, resolved.Origin
		}
		org, lease = o.org, o.lease
		o.mu.Unlock()
	}

	return volume.ObjectDownload{
		Endpoint: lease.Endpoint,
		Region:   lease.Region,
		Bucket:   lease.Bucket,
		Key:      volume.ObjectKey(org, o.namespace, target),
		Credentials: volume.Credentials{
			AccessKeyID:     lease.AccessKeyID,
			SecretAccessKey: lease.SecretAccessKey,
			SessionToken:    lease.SessionToken,
		},
		ExpectedSize: size,
	}, nil
}

// stream opens one object for reading rather than holding it in memory,
// decoded according to the media type the store reports. The caller closes
// what comes back.
func (o *origin) stream(
	ctx context.Context,
	download volume.ObjectDownloader,
	decompress volume.Decompressor,
	target volume.Target,
	size int64,
) (io.ReadCloser, error) {
	req, err := o.request(ctx, target, size)
	if err != nil {
		return nil, err
	}
	return volume.OpenObject(ctx, download, decompress, req)
}

// fetch reads one object whole, decoding it according to the media type the
// store reports.
func (o *origin) fetch(
	ctx context.Context,
	download volume.ObjectDownloader,
	decompress volume.Decompressor,
	target volume.Target,
	size int64,
	maxSize int64,
) ([]byte, error) {
	req, err := o.request(ctx, target, size)
	if err != nil {
		return nil, err
	}
	return volume.FetchObject(ctx, download, decompress, req, maxSize)
}
