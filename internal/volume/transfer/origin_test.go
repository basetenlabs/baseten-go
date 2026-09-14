package transfer

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-go/internal/require"
	"github.com/basetenlabs/baseten-go/internal/volume"
	"github.com/basetenlabs/baseten-go/internal/volume/bdn"
)

type resolverFunc func(context.Context, bdn.ResolveRequest) (*bdn.ResolveResult, error)

type originRequestResult struct {
	req volume.ObjectDownload
	err error
}

func (f resolverFunc) Resolve(ctx context.Context, ref bdn.ResolveRequest) (*bdn.ResolveResult, error) {
	return f(ctx, ref)
}

// TestOriginRequestsContinueDuringRenewal guards the transfer's hot path: one
// request may wait for Resolve, but sibling chunk requests keep using the
// current, still-valid lease rather than queueing behind that network call.
func TestOriginRequestsContinueDuringRenewal(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int64
	o := &origin{
		client: resolverFunc(func(context.Context, bdn.ResolveRequest) (*bdn.ResolveResult, error) {
			calls.Add(1)
			close(started)
			<-release
			return resolvedLease("new-org", "new", time.Now().Add(2*credentialMargin)), nil
		}),
		ref:       bdn.ResolveRequest{Namespace: "ns", Volume: "vol", Digest: "digest"},
		namespace: "ns",
		org:       "old-org",
		lease:     testLease("old", time.Now().Add(credentialMargin/2)),
	}

	first := make(chan originRequestResult, 1)
	go func() {
		req, err := o.request(context.Background(), volume.Target{RelativeKey: "first"}, 1)
		first <- originRequestResult{req: req, err: err}
	}()
	<-started

	second := make(chan originRequestResult, 1)
	go func() {
		req, err := o.request(context.Background(), volume.Target{RelativeKey: "second"}, 2)
		second <- originRequestResult{req: req, err: err}
	}()
	select {
	case result := <-second:
		require.NoError(t, result.err)
		require.Equal(t, "old", result.req.Credentials.AccessKeyID)
		require.Equal(t, "bdn/old-org/ns/second", result.req.Key)
	case <-time.After(time.Second):
		t.Fatal("a chunk request blocked behind credential renewal")
	}
	require.Equal(t, int64(1), calls.Load())

	close(release)
	select {
	case result := <-first:
		require.NoError(t, result.err)
		require.Equal(t, "new", result.req.Credentials.AccessKeyID)
		require.Equal(t, "bdn/new-org/ns/first", result.req.Key)
	case <-time.After(time.Second):
		t.Fatal("credential renewal did not finish")
	}
}

func TestOriginInstallsNonExpiringRenewal(t *testing.T) {
	var calls atomic.Int64
	o := &origin{
		client: resolverFunc(func(context.Context, bdn.ResolveRequest) (*bdn.ResolveResult, error) {
			calls.Add(1)
			return resolvedLease("static-org", "static", time.Time{}), nil
		}),
		ref:       bdn.ResolveRequest{Namespace: "ns", Volume: "vol"},
		namespace: "ns",
		org:       "old-org",
		lease:     testLease("expiring", time.Now().Add(30*time.Second)),
	}

	first, err := o.request(context.Background(), volume.Target{RelativeKey: "first"}, 1)
	require.NoError(t, err)
	require.Equal(t, "static", first.Credentials.AccessKeyID)
	require.Equal(t, "bdn/static-org/ns/first", first.Key)

	second, err := o.request(context.Background(), volume.Target{RelativeKey: "second"}, 1)
	require.NoError(t, err)
	require.Equal(t, "static", second.Credentials.AccessKeyID)
	require.Equal(t, int64(1), calls.Load())
}

// TestOriginRetriesAfterShortRenewal covers the old permanent latch: a short
// replacement is accepted when it is an improvement, then retried after the
// cooldown so a later healthy lease can recover the transfer.
func TestOriginRetriesAfterShortRenewal(t *testing.T) {
	leases := []*bdn.ResolveResult{
		resolvedLease("org", "short", time.Now().Add(40*time.Second)),
		resolvedLease("org", "long", time.Now().Add(2*credentialMargin)),
	}
	o := &origin{
		client: resolverFunc(func(context.Context, bdn.ResolveRequest) (*bdn.ResolveResult, error) {
			result := leases[0]
			leases = leases[1:]
			return result, nil
		}),
		ref:       bdn.ResolveRequest{Namespace: "ns", Volume: "vol"},
		namespace: "ns",
		org:       "org",
		lease:     testLease("old", time.Now().Add(30*time.Second)),
	}

	first, err := o.request(context.Background(), volume.Target{RelativeKey: "object"}, 1)
	require.NoError(t, err)
	require.Equal(t, "short", first.Credentials.AccessKeyID)

	// The next object does not immediately resolve the same short lease again.
	second, err := o.request(context.Background(), volume.Target{RelativeKey: "object"}, 1)
	require.NoError(t, err)
	require.Equal(t, "short", second.Credentials.AccessKeyID)
	require.Equal(t, 1, len(leases))

	// Once the rate limit passes, renewal is allowed to recover.
	o.mu.Lock()
	o.renewAfter = time.Now().Add(-time.Second)
	o.mu.Unlock()
	third, err := o.request(context.Background(), volume.Target{RelativeKey: "object"}, 1)
	require.NoError(t, err)
	require.Equal(t, "long", third.Credentials.AccessKeyID)
	require.Equal(t, 0, len(leases))
}

func TestOriginDoesNotReplaceLeaseWithEarlierExpiry(t *testing.T) {
	expires := time.Now().Add(30 * time.Second)
	o := &origin{
		client: resolverFunc(func(context.Context, bdn.ResolveRequest) (*bdn.ResolveResult, error) {
			return resolvedLease("wrong-org", "older", expires.Add(-time.Second)), nil
		}),
		ref:       bdn.ResolveRequest{Namespace: "ns", Volume: "vol"},
		namespace: "ns",
		org:       "org",
		lease:     testLease("current", expires),
	}

	req, err := o.request(context.Background(), volume.Target{RelativeKey: "object"}, 1)
	require.NoError(t, err)
	require.Equal(t, "current", req.Credentials.AccessKeyID)
	require.Equal(t, "bdn/org/ns/object", req.Key)
}

func testLease(accessKey string, expires time.Time) bdn.Origin {
	return bdn.Origin{AccessKeyID: accessKey, ExpiresAt: expires}
}

func resolvedLease(org, accessKey string, expires time.Time) *bdn.ResolveResult {
	return &bdn.ResolveResult{
		Resolved: bdn.Resolved{OrgID: org},
		Origin:   testLease(accessKey, expires),
	}
}
