package volume

import (
	"reflect"
	"testing"

	internal "github.com/basetenlabs/baseten-go/internal/volume"
	"github.com/basetenlabs/baseten-go/internal/volume/transfer"
)

// Translation layers rot by twin drift: a field added to an internal struct
// but not to its public twin is a silent omission — this repository has one
// on its own ledger, where the download result gained containment warnings
// internally and the public result went without them until this package was
// built. These tests make a drifted twin a red test instead: every pair's
// field-name sets must match after applying the documented renames, and
// every deliberate one-sided field must be named in an exception list with
// its reason living here, next to the assertion that enforces it.
//
// Field TYPES may deliberately differ (public Digest string vs the internal
// 32-byte digest; the Store interface vs the internal function pair); the
// boundary translation and its tests own those conversions. Names are what
// drift silently.

type twinPair struct {
	name     string
	public   any
	internal any
	// renamed maps a public field name to the internal name it translates
	// to or from.
	renamed map[string]string
	// publicOnly names fields that deliberately exist only on the public
	// side, with the reason.
	publicOnly map[string]string
	// internalOnly names fields that deliberately exist only on the
	// internal side, with the reason.
	internalOnly map[string]string
}

func TestPublicTwinsDoNotDrift(t *testing.T) {
	seamOnly := map[string]string{
		"Store": "the public seam is one ObjectStore interface; internally it is the DownloadObject and Decompress function pair",
	}
	seamInternal := map[string]string{
		"DownloadObject": "half of the public Store interface",
		"Decompress":     "half of the public Store interface",
		"Limiter": "internal mechanics, never public: the Limiter/Permit/Outcome seam changed semantics twice in one review cycle " +
			"(the untimed zero-routing, then the coarse-clock meaning of a zero duration); public, both would have been API breaks",
	}
	pairs := []twinPair{
		{name: "Concurrency", public: Concurrency{}, internal: internal.Concurrency{}},
		{name: "Progress", public: Progress{}, internal: internal.Progress{}},
		{name: "Credentials", public: Credentials{}, internal: internal.Credentials{}},
		{name: "ObjectDownload", public: ObjectDownload{}, internal: internal.ObjectDownload{}},
		{name: "ObjectResult", public: ObjectResult{}, internal: internal.ObjectResult{}},
		{name: "Warning", public: Warning{}, internal: internal.ContainmentWarning{}},
		{name: "PushResult", public: PushResult{}, internal: transfer.PushResult{}},
		{name: "DownloadResult", public: DownloadResult{}, internal: transfer.PullResult{}},
		{
			name: "PushOptions", public: PushOptions{}, internal: transfer.PushOptions{},
			renamed:      map[string]string{"Hasher": "NewHasher"},
			publicOnly:   seamOnly,
			internalOnly: seamInternal,
		},
		{
			name: "DownloadOptions", public: DownloadOptions{}, internal: transfer.PullOptions{},
			renamed:      map[string]string{"Hasher": "NewHasher"},
			publicOnly:   seamOnly,
			internalOnly: seamInternal,
		},
	}
	for _, pair := range pairs {
		t.Run(pair.name, func(t *testing.T) {
			pub := fieldNames(reflect.TypeOf(pair.public))
			internalNames := fieldNames(reflect.TypeOf(pair.internal))

			mapped := map[string]bool{}
			for name := range pub {
				if reason, ok := pair.publicOnly[name]; ok {
					_ = reason
					continue
				}
				want := name
				if r, ok := pair.renamed[name]; ok {
					want = r
				}
				if !internalNames[want] {
					t.Errorf("public field %s has no internal twin %q — either the internal side lost it or the exception list is missing an entry", name, want)
				}
				mapped[want] = true
			}
			for name := range internalNames {
				if mapped[name] {
					continue
				}
				if _, ok := pair.internalOnly[name]; ok {
					continue
				}
				t.Errorf("internal field %s has no public twin — a caller-visible capability drifted, or the exception list is missing an entry", name)
			}
		})
	}
}

func fieldNames(t reflect.Type) map[string]bool {
	names := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		names[t.Field(i).Name] = true
	}
	return names
}
