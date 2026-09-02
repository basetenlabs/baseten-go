package client

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"testing"

	internal "github.com/basetenlabs/baseten-go/internal/volume"
	"github.com/basetenlabs/baseten-go/internal/volume/transfer"
)

// Translation layers rot by twin drift: a field added to an internal struct
// but not to its public twin is a silent omission — this repository has one
// on its own ledger, where the download result gained containment warnings
// internally and the public result went without them until the parity tests
// were built. These tests make a drifted twin a red test instead: every
// pair's field-name sets must match after applying the documented renames,
// and every deliberate one-sided field must be named in an exception list
// with its reason living here, next to the assertion that enforces it.
//
// Field TYPES may deliberately differ (the public string manifest digest vs
// the internal 32-byte digest; the Store interface vs the internal function
// pair); the boundary translation and its tests own those conversions. Names
// are what drift silently.
//
// The table itself is guarded by TestVolumeVocabularyIsClassified below:
// every exported type declared in the vocabulary files must appear either
// here or in that test's exception list, so a pair silently dropped from
// this table is a red test too — matching by name means a rename wave is
// exactly when a pair could otherwise vanish without a compile error.

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

// volumeTwinPairs is the pair table, shared by the drift test and the
// classification test.
func volumeTwinPairs() []twinPair {
	seamOnly := map[string]string{
		"Store": "the public seam is one VolumeObjectStore interface; internally it is the DownloadObject and Decompress function pair",
	}
	seamInternal := map[string]string{
		"DownloadObject": "half of the public Store interface",
		"Decompress":     "half of the public Store interface",
		"Limiter": "internal mechanics, never public: the Limiter/Permit/Outcome seam changed semantics twice in one review cycle " +
			"(the untimed zero-routing, then the coarse-clock meaning of a zero duration); public, both would have been API breaks",
	}
	return []twinPair{
		{name: "VolumeConcurrencyOptions", public: VolumeConcurrencyOptions{}, internal: internal.Concurrency{}},
		{name: "VolumeProgress", public: VolumeProgress{}, internal: internal.Progress{}},
		{name: "VolumeObjectCredentials", public: VolumeObjectCredentials{}, internal: internal.Credentials{}},
		{name: "VolumeObjectDownload", public: VolumeObjectDownload{}, internal: internal.ObjectDownload{}},
		{name: "VolumeObjectResult", public: VolumeObjectResult{}, internal: internal.ObjectResult{}},
		{name: "VolumeWarning", public: VolumeWarning{}, internal: internal.ContainmentWarning{}},
		{name: "PushVolumeResult", public: PushVolumeResult{}, internal: transfer.PushResult{}},
		{name: "DownloadVolumeResult", public: DownloadVolumeResult{}, internal: transfer.PullResult{}},
		{
			name: "PushVolumeOptions", public: PushVolumeOptions{}, internal: transfer.PushOptions{},
			renamed:      map[string]string{"Hasher": "NewHasher"},
			publicOnly:   seamOnly,
			internalOnly: seamInternal,
		},
		{
			name: "DownloadVolumeOptions", public: DownloadVolumeOptions{}, internal: transfer.PullOptions{},
			renamed:      map[string]string{"Hasher": "NewHasher"},
			publicOnly:   seamOnly,
			internalOnly: seamInternal,
		},
	}
}

func TestPublicTwinsDoNotDrift(t *testing.T) {
	for _, pair := range volumeTwinPairs() {
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

// TestVolumeVocabularyIsClassified is the exhaustiveness leg: every exported
// type declared in the volume vocabulary files must be a pair in the twin
// table or carry a reasoned entry here. Without it, deleting a row from the
// table — both types intact — would silently stop checking that pair; the
// pairs are matched by name, so a rename wave is the exact moment that could
// happen.
func TestVolumeVocabularyIsClassified(t *testing.T) {
	// Types with no struct twin to compare fields against, each with the
	// reason it is not in the table.
	exceptions := map[string]string{
		"VolumePhase":       "defined string type; its values mirror the internal phase strings and cross the boundary as a cast — VolumeProgress's parity covers the field that carries it",
		"VolumeWarningKind": "defined uint8 type; its values mirror the internal containment kinds one to one and cross the boundary as a cast — VolumeWarning's parity covers the field that carries it",
		"VolumeErrorReason": "defined string type mirroring the service's wire reason strings, so a reason the service adds flows through without new API",
		"VolumeError":       "deliberately narrower than the internal error: Reason and Message are the caller's API, Err wraps the whole original chain, and the internal Code and Domain stay internal — volumeOpError owns the translation",
		"VolumeObjectStore": "the seam interface; its two halves are the internal DownloadObject and Decompress function pair, excepted per pair in the table",
	}

	inTable := map[string]bool{}
	for _, pair := range volumeTwinPairs() {
		inTable[pair.name] = true
	}

	declared := exportedTypeDecls(t, "volume_types.go", "volume_errors.go")
	if len(declared) == 0 {
		t.Fatal("no exported types found in the vocabulary files — the enumeration is broken, not the vocabulary empty")
	}
	for _, name := range declared {
		if inTable[name] {
			continue
		}
		if _, ok := exceptions[name]; ok {
			continue
		}
		t.Errorf("exported type %s is neither a pair in the twin table nor in the exception list — classify it", name)
	}
	for name := range exceptions {
		if inTable[name] {
			t.Errorf("%s is both a pair and an exception — one entry must go", name)
		}
	}
}

// exportedTypeDecls parses the named files of this package and returns the
// exported type names they declare.
func exportedTypeDecls(t *testing.T, files ...string) []string {
	t.Helper()
	var names []string
	fset := token.NewFileSet()
	for _, file := range files {
		parsed, err := parser.ParseFile(fset, file, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range parsed.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts := spec.(*ast.TypeSpec)
				if ts.Name.IsExported() {
					names = append(names, ts.Name.Name)
				}
			}
		}
	}
	return names
}

func fieldNames(t reflect.Type) map[string]bool {
	names := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		names[t.Field(i).Name] = true
	}
	return names
}
