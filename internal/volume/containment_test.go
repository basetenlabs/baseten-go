package volume

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
)

// containTree builds a manifest from a compact description: "d path" for a
// directory, "f path" for a file, "l path -> target" for a symlink.
func containTree(t *testing.T, entries ...string) *Manifest {
	t.Helper()
	m := &Manifest{}
	for _, e := range entries {
		kind, rest, ok := strings.Cut(e, " ")
		if !ok {
			t.Fatalf("bad entry %q", e)
		}
		switch kind {
		case "d":
			m.Directories = append(m.Directories, DirectoryEntry{Path: rest, Mode: 0o755})
		case "f":
			m.Files = append(m.Files, FileEntry{Path: rest, Mode: 0o644, Kind: FileKindChunk})
		case "l":
			path, target, ok := strings.Cut(rest, " -> ")
			if !ok {
				t.Fatalf("bad link entry %q", e)
			}
			m.Symlinks = append(m.Symlinks, SymlinkEntry{Path: path, Target: target, Mode: SymlinkMode})
		default:
			t.Fatalf("bad entry kind %q", e)
		}
	}
	return m
}

// TestContainmentVerdicts pins the resolution against the format's rule, one
// named case per row. The rows where a per-link lexical check and the real
// resolution disagree are the reason the resolution exists.
func TestContainmentVerdicts(t *testing.T) {
	cases := []struct {
		name    string
		entries []string
		fatal   string // substring of the hard error, empty for none
		warns   int    // lenient-profile warnings expected when not fatal
		// strictOK: the strict profile accepts despite lenient warnings —
		// only implicit parents qualify; every link finding hardens.
		strictOK bool
	}{
		{
			name:    "plain contained link",
			entries: []string{"d models", "d weights", "f weights/w.bin", "l models/a -> ../weights/w.bin"},
		},
		{
			name:    "direct escape",
			entries: []string{"l a -> ../../etc/passwd"},
			fatal:   "steps outside the volume root",
		},
		{
			name: "chain escape a lexical check misses",
			// sub/up -> .. passes a lexical depth count, and so does
			// esc -> sub/up/..; only chasing sub/up to the root and taking
			// the real depth from there sees esc step above the root.
			entries: []string{"d sub", "l sub/up -> ..", "l esc -> sub/up/.."},
			fatal:   "steps outside the volume root",
		},
		{
			name:    "absolute target resolving in the volume",
			entries: []string{"d usr", "d usr/lib", "f usr/lib/x", "l libfoo.so -> /usr/lib/x"},
		},
		{
			name:    "absolute target escaping through dot-dot",
			entries: []string{"d x", "l a -> /x/../../y"},
			fatal:   "steps outside the volume root",
		},
		{
			name:    "absolute target with no entry dangles",
			entries: []string{"l libfoo.so -> /usr/lib/x"},
			warns:   1,
		},
		{
			name:    "relative dangling link",
			entries: []string{"d models", "l models/current -> v2"},
			warns:   1,
		},
		{
			name: "dangling with an escaping remainder is still an escape",
			// The walk stops at "missing", but the components after the stop
			// are judged from its real depth: missing/../../x pops past the
			// root even though nothing at "missing" exists to resolve.
			entries: []string{"l a -> missing/../../x"},
			fatal:   "steps outside the volume root",
		},
		{
			name:    "empty target",
			entries: []string{"l e -> "},
			fatal:   "has no target",
		},
		{
			name:    "two-link loop",
			entries: []string{"l a -> b", "l b -> a"},
			fatal:   "chases more than",
		},
		{
			name:    "link through a file",
			entries: []string{"f f", "l l -> f/x"},
			warns:   1,
		},
		{
			name:    "link through a file with an escaping remainder",
			entries: []string{"f f", "l l -> f/../../x"},
			fatal:   "steps outside the volume root",
		},
		{
			name:    "parent recorded as a file",
			entries: []string{"f a", "f a/b"},
			fatal:   "something other than a directory",
		},
		{
			name: "parent with no record is an implicit directory",
			// Producers may omit directory entries; materialization creates
			// them. The lenient profile still says so, the strict one
			// accepts outright — the format allows it everywhere.
			entries:  []string{"f x/y"},
			warns:    1,
			strictOK: true,
		},
		{
			name:    "NUL byte in a target",
			entries: []string{"l n -> a\x00b"},
			fatal:   "contains a NUL byte",
		},
		{
			name: "probe: chained dangling with escaping remainder stays dangling",
			// Pinned with inverted expectations on purpose: the chase into b
			// stops dangling and x's remaining components are deliberately
			// not judged, matching the format (any reader fails at b first).
			// A "fix" that judges them would refuse manifests the format
			// accepts — this row is here so such a fix fails loudly against
			// the row that documents why the behavior matches. The
			// composition caveat is flagged upstream.
			entries: []string{"l b -> missing", "l x -> b/../.."},
			warns:   2,
		},
		{
			name: "probe: chained file-landing with escaping remainder stays a warning",
			// Same deliberate inversion for the file-landing variant: the
			// chase resolves b to a file, x reports resolves-through-a-file,
			// and x's remainder past that point is not judged.
			entries: []string{"f t", "l b -> t", "l x -> b/../../q"},
			warns:   1,
		},
		{
			name: "chased stop leaves the chasing remainder unjudged",
			// The chase into d stops dangling; any reader fails there first
			// and never applies l's remaining components, so l is dangling
			// too rather than an escape — the format's own rule, matched
			// deliberately. Judging l's remainder would refuse manifests the
			// format accepts.
			entries: []string{"l d -> missing", "l l -> d/../../x"},
			warns:   2,
		},
		{
			name: "parent chain through a symlink, however deep, is fatal",
			// b has no record, but the nearest recorded ancestor of a/b/c is
			// the SYMLINK a — no real filesystem holds entries below a link,
			// so this is the parent-not-a-directory kind, not the missing-
			// record warning.
			entries: []string{"f t", "l a -> t", "f a/b/c"},
			fatal:   "something other than a directory",
		},
		{
			name: "chain landing on a real entry",
			// current -> v2 -> the directory; a file inside resolves through
			// both links.
			entries: []string{"d releases", "d releases/v2", "f releases/v2/w.bin",
				"l releases/current -> v2", "l latest -> releases/current/w.bin"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := containTree(t, c.entries...)
			warnings, err := CheckManifestContainment(m)
			if c.fatal != "" {
				if err == nil {
					t.Fatalf("lenient profile accepted it, want an error naming %q", c.fatal)
				}
				if !errors.Is(err, ErrNotContained) || !strings.Contains(err.Error(), c.fatal) {
					t.Fatalf("lenient profile said %v, want ErrNotContained naming %q", err, c.fatal)
				}
			} else {
				if err != nil {
					t.Fatalf("lenient profile rejected it: %v", err)
				}
				if len(warnings) != c.warns {
					t.Fatalf("lenient profile warned %d times (%v), want %d", len(warnings), warnings, c.warns)
				}
			}

			// The strict profile agrees on every hard verdict and hardens
			// every warning: a push never records what a pull would have to
			// warn about.
			src := &Source{Directories: m.Directories, Symlinks: m.Symlinks}
			for _, f := range m.Files {
				src.Files = append(src.Files, SourceFile{Path: f.Path, Mode: f.Mode})
			}
			strictErr := CheckSourceContainment(src)
			switch {
			case c.fatal != "":
				if strictErr == nil || !strings.Contains(strictErr.Error(), c.fatal) {
					t.Fatalf("strict profile said %v, want %q", strictErr, c.fatal)
				}
			case c.warns > 0 && !c.strictOK:
				if !errors.Is(strictErr, ErrNotContained) {
					t.Fatalf("strict profile said %v, want ErrNotContained for what the lenient one warns about", strictErr)
				}
			default:
				if strictErr != nil {
					t.Fatalf("strict profile rejected an acceptable tree: %v", strictErr)
				}
			}
		})
	}
}

// TestContainmentHopCap pins the boundary: a chain of exactly the budget
// resolves, one more is treated as a loop.
func TestContainmentHopCap(t *testing.T) {
	chain := func(links int) *Manifest {
		m := &Manifest{Files: []FileEntry{{Path: fmt.Sprintf("link%d", links), Mode: 0o644, Kind: FileKindChunk}}}
		for i := 0; i < links; i++ {
			m.Symlinks = append(m.Symlinks, SymlinkEntry{
				Path: fmt.Sprintf("link%d", i), Target: fmt.Sprintf("link%d", i+1), Mode: SymlinkMode,
			})
		}
		return m
	}
	// A chain of n links costs n-1 hops from the first link's resolution:
	// link0's own target is free, each further link chased counts one.
	if _, err := CheckManifestContainment(chain(maxSymlinkHops + 1)); err != nil {
		t.Fatalf("a chain spending exactly %d hops should resolve: %v", maxSymlinkHops, err)
	}
	_, err := CheckManifestContainment(chain(maxSymlinkHops + 2))
	if err == nil || !strings.Contains(err.Error(), "chases more than") {
		t.Fatalf("a chain spending %d hops should be treated as a loop, got %v", maxSymlinkHops+1, err)
	}

	// A link chasing INTO a maximal chain is one hop over the budget and
	// must be a loop, exactly as the format's gate refuses it. Pinned
	// separately from the plain chain above because any future caching of
	// per-link results has to replay the hop cost to keep this true —
	// accepting here would be a push-side false-accept.
	over := chain(maxSymlinkHops + 1)
	over.Symlinks = append(over.Symlinks, SymlinkEntry{Path: "z", Target: "link0", Mode: SymlinkMode})
	_, err = CheckManifestContainment(over)
	if err == nil || !strings.Contains(err.Error(), "chases more than") {
		t.Fatalf("a link one hop past the budget through a remembered chain should be a loop, got %v", err)
	}
}

// TestRelativeLinkTarget pins the on-disk rendering of a volume-root-absolute
// target: climb from the link's directory to the root, then descend the
// target's own components.
func TestRelativeLinkTarget(t *testing.T) {
	for _, c := range []struct {
		link, target, want string
	}{
		{"libfoo.so", "/usr/lib/x", "usr/lib/x"},
		{"a/b", "/usr/lib/x", "../usr/lib/x"},
		{"a/b/c", "/x/./y//z", "../../x/y/z"},
		{"a/b", "/", ".."},
		{"top", "/", "."},
	} {
		if got := RelativeLinkTarget(c.link, c.target); got != c.want {
			t.Errorf("RelativeLinkTarget(%q, %q) = %q, want %q", c.link, c.target, got, c.want)
		}
	}
}

// TestResolutionAgainstReferenceWalk holds the walker to an independent
// second implementation, the way the encoder's comparator is held to its
// reference: random small manifests full of links and ".." components,
// resolved by a straightforward string-and-map walk that shares no code
// with the interned namespace, asserting the same verdict for every link —
// and the same terminal for the contained ones.
func TestResolutionAgainstReferenceWalk(t *testing.T) {
	rng := rand.New(rand.NewPCG(11, 47))
	dirPool := []string{"a", "b", "a/b", "b/c", "d"}
	filePool := []string{"a/f", "b/c/g", "h", "d/i"}
	linkPool := []string{"l1", "a/l2", "b/l3", "b/c/l4"}
	compPool := []string{"a", "b", "c", "d", "f", "g", "h", "x", "l1", "l2", "l3", ".."}

	for round := 0; round < 1500; round++ {
		entries := map[string]nsKind{}
		targets := map[string]string{}
		var list []linkEntry
		add := func(path string, kind nsKind, target string) {
			if _, dup := entries[path]; dup {
				return
			}
			entries[path] = kind
			targets[path] = target
			list = append(list, linkEntry{path: path, kind: kind, target: target})
		}
		for _, d := range dirPool[:2+rng.IntN(len(dirPool)-1)] {
			add(d, nsDirectory, "")
		}
		for _, f := range filePool[:rng.IntN(len(filePool)+1)] {
			add(f, nsFile, "")
		}
		for _, l := range linkPool[:1+rng.IntN(len(linkPool))] {
			n := 1 + rng.IntN(4)
			parts := make([]string, 0, n)
			for range n {
				parts = append(parts, compPool[rng.IntN(len(compPool))])
			}
			target := strings.Join(parts, "/")
			if rng.IntN(4) == 0 {
				target = "/" + target
			}
			add(l, nsSymlink, target)
		}

		ns, err := buildLinkNamespace(list)
		if err != nil {
			t.Fatalf("round %d: namespace: %v", round, err)
		}
		implicit := map[string]bool{}
		for path := range entries {
			for _, parent := range parentPaths(path) {
				if _, recorded := entries[parent]; !recorded {
					implicit[parent] = true
				}
			}
		}
		for path, kind := range entries {
			if kind != nsSymlink {
				continue
			}
			var id int32
			for _, candidate := range ns.links() {
				if ns.pathOf(candidate) == path {
					id = candidate
				}
			}
			hops := 0
			res, err := ns.resolveLink(id, &hops)
			got, gotTerminal := classifyResolution(res, err), ""
			if got == "contained" {
				// The namespace names the root "."; the reference uses the
				// empty component list. Same place, two spellings.
				if gotTerminal = ns.pathOf(res.node); gotTerminal == "." {
					gotTerminal = ""
				}
			}
			refHops := 0
			want, wantTerminal := referenceWalk(entries, targets, implicit, path, &refHops)
			if got != want || (got == "contained" && gotTerminal != wantTerminal) {
				t.Fatalf("round %d, link %s -> %q: walker says %s (terminal %q), reference says %s (terminal %q)\nmanifest: %v",
					round, path, targets[path], got, gotTerminal, want, wantTerminal, entries)
			}
		}
	}
}

func classifyResolution(res linkResolution, err error) string {
	if err != nil {
		msg := err.Error()
		switch {
		case strings.Contains(msg, "has no target"):
			return "empty"
		case strings.Contains(msg, "chases more than"):
			return "loop"
		case strings.Contains(msg, "steps outside"):
			return "escape"
		default:
			return "error:" + msg
		}
	}
	switch res.kind {
	case linkContained:
		return "contained"
	case linkDangling:
		return "dangling"
	default:
		return "traverses"
	}
}

// referenceWalk resolves one link over plain strings and maps: the current
// position is a slice of components, ".." pops it (popping past the root is
// an escape), a name looks the joined path up. It mirrors the rule, not the
// implementation.
func referenceWalk(entries map[string]nsKind, targets map[string]string, implicit map[string]bool, link string, hops *int) (string, string) {
	target := targets[link]
	if target == "" {
		return "empty", ""
	}
	var comps []string
	for _, c := range strings.Split(target, "/") {
		if c != "" && c != "." {
			comps = append(comps, c)
		}
	}
	var cur []string
	if !strings.HasPrefix(target, "/") {
		if parent := parentPaths(link); len(parent) > 0 {
			cur = strings.Split(parent[0], "/")
		}
	}
	for i, c := range comps {
		last := i+1 == len(comps)
		if c == ".." {
			if len(cur) == 0 {
				return "escape", ""
			}
			cur = cur[:len(cur)-1]
			continue
		}
		next := strings.Join(append(slices.Clone(cur), c), "/")
		kind, recorded := entries[next]
		switch {
		case recorded && kind == nsSymlink:
			*hops++
			if *hops > maxSymlinkHops {
				return "loop", ""
			}
			verdict, terminal := referenceWalk(entries, targets, implicit, next, hops)
			if verdict != "contained" {
				return verdict, terminal
			}
			if !last && entries[terminal] == nsFile {
				return "traverses", terminal
			}
			if terminal == "" {
				cur = nil
			} else {
				cur = strings.Split(terminal, "/")
			}
		case recorded && kind == nsFile:
			if !last {
				if refRemainderEscapes(len(cur)+1, comps[i+1:]) {
					return "escape", ""
				}
				return "traverses", next
			}
			cur = append(slices.Clone(cur), c)
		case (recorded && kind == nsDirectory) || implicit[next]:
			cur = append(slices.Clone(cur), c)
		default:
			if refRemainderEscapes(len(cur)+1, comps[i+1:]) {
				return "escape", ""
			}
			return "dangling", next
		}
	}
	return "contained", strings.Join(cur, "/")
}

func refRemainderEscapes(depth int, rest []string) bool {
	for _, c := range rest {
		if c == ".." {
			if depth == 0 {
				return true
			}
			depth--
		} else {
			depth++
		}
	}
	return false
}

// The adversarial shapes behind the scale note on resolveLink, kept
// executable so the note's numbers can be re-derived: a single maximal
// chain, deep entries with dot-dot-heavy targets, and many links re-chasing
// one shared chain. The benchmark runs them small; the quoted measurements
// are these same generators at 130000.
func chainManifest(links int) *Manifest {
	m := &Manifest{Files: []FileEntry{{Path: fmt.Sprintf("link%06d", links), Mode: 0o644, Kind: FileKindChunk}}}
	for i := 0; i < links; i++ {
		m.Symlinks = append(m.Symlinks, SymlinkEntry{
			Path: fmt.Sprintf("link%06d", i), Target: fmt.Sprintf("link%06d", i+1), Mode: SymlinkMode,
		})
	}
	return m
}

func deepDotDotManifest(links int) *Manifest {
	deep := "d" + strings.Repeat("/d", 39)
	m := &Manifest{Directories: []DirectoryEntry{
		{Path: "d", Mode: 0o755},
		{Path: deep, Mode: 0o755},
	}}
	for i := 0; i < links; i++ {
		m.Symlinks = append(m.Symlinks, SymlinkEntry{
			Path:   fmt.Sprintf("%s/l%06d", deep, i),
			Target: strings.Repeat("../", 39) + deep,
			Mode:   SymlinkMode,
		})
	}
	return m
}

func sharedChainManifest(links int) *Manifest {
	m := &Manifest{Files: []FileEntry{{Path: "t", Mode: 0o644, Kind: FileKindChunk}}}
	for i := 0; i < 39; i++ {
		m.Symlinks = append(m.Symlinks, SymlinkEntry{Path: fmt.Sprintf("c%02d", i), Target: fmt.Sprintf("c%02d", i+1), Mode: SymlinkMode})
	}
	m.Symlinks = append(m.Symlinks, SymlinkEntry{Path: "c39", Target: "t", Mode: SymlinkMode})
	for i := 0; i < links; i++ {
		m.Symlinks = append(m.Symlinks, SymlinkEntry{Path: fmt.Sprintf("u%06d", i), Target: "c00", Mode: SymlinkMode})
	}
	return m
}

func BenchmarkContainmentAdversarial(b *testing.B) {
	for name, m := range map[string]*Manifest{
		"chain":        chainManifest(1000),
		"deep-dot-dot": deepDotDotManifest(1000),
		"shared-chain": sharedChainManifest(1000),
	} {
		b.Run(name, func(b *testing.B) {
			for b.Loop() {
				_, _ = CheckManifestContainment(m)
			}
		})
	}
}
