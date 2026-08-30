package transfer

import (
	"context"
	"sync"
	"time"

	"github.com/basetenlabs/baseten-go/internal/volume"
	"github.com/basetenlabs/baseten-go/internal/volume/bdn"
)

// credentialMargin is how long before a lease expires the client asks for a
// new one. A download that starts inside the margin should finish inside the
// lease.
const credentialMargin = time.Minute

// origin holds the credential lease a pull reads objects with, and renews it
// when it is about to run out.
//
// Renewing means resolving the pinned ref again. That is safe to repeat
// because resolving changes nothing: it mints credentials and reports where a
// version lives. Pinning is what makes it safe to repeat usefully — resolving
// the same digest twice always names the same bytes, where re-resolving a tag
// could quietly switch versions in the middle of a download.
type origin struct {
	client    *bdn.Client
	ref       bdn.Ref
	namespace string

	mu    sync.Mutex
	org   string
	lease bdn.Origin

	// renewable goes false once a renewal has come back no better than what it
	// replaced. Leases shorter than the margin are always "about to expire",
	// so retrying would mean a resolve before every object — a storm, and a
	// serialized one, since renewal holds the lock. Giving up leaves the
	// credentials to fail on their own terms, which is a legible error rather
	// than a hang.
	renewable bool
}

func newOrigin(client *bdn.Client, ref bdn.Ref, org string, lease bdn.Origin) *origin {
	return &origin{client: client, ref: ref, namespace: ref.Namespace, org: org, lease: lease, renewable: true}
}

// request builds a read of one object, renewing the lease first if it is close
// to expiring.
func (o *origin) request(ctx context.Context, target volume.Target, size int64) (volume.ObjectDownload, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.renewable && !o.lease.ExpiresAt.IsZero() && time.Until(o.lease.ExpiresAt) < credentialMargin {
		resolved, err := o.client.Resolve(ctx, o.ref.String())
		if err != nil {
			return volume.ObjectDownload{}, err
		}
		o.org, o.lease = resolved.Resolved.OrgID, resolved.Origin

		// A replacement that is itself already inside the margin bought
		// nothing, and asking again on the next object would mean a resolve
		// per object for the rest of the download.
		if o.lease.ExpiresAt.IsZero() || time.Until(o.lease.ExpiresAt) <= credentialMargin {
			o.renewable = false
		}
	}

	return volume.ObjectDownload{
		Endpoint: o.lease.Endpoint,
		Region:   o.lease.Region,
		Bucket:   o.lease.Bucket,
		Key:      volume.ObjectKey(o.org, o.namespace, target),
		Credentials: volume.Credentials{
			AccessKeyID:     o.lease.AccessKeyID,
			SecretAccessKey: o.lease.SecretAccessKey,
			SessionToken:    o.lease.SessionToken,
		},
		ExpectedSize: size,
	}, nil
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
