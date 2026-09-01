package volume

import (
	"errors"
	"fmt"
	"strings"
)

// Containment. A volume is consumed as a filesystem, so a manifest has two
// surfaces a hostile or careless path can escape through: entry paths decide
// where a record lands when the tree is written out, and symlink targets
// decide where a link points for every reader afterwards. The format's rule
// is that a committed manifest is self-contained — every entry path and every
// location a symlink can reach lies inside the volume root.
//
// Entry paths are covered by ValidatePath. This file covers the links, and
// a per-link glance at the target string is not enough: "sub/up -> .." and
// "esc -> sub/up/.." each look harmless alone, but resolving esc walks
// through sub/up to the root and then steps above it. Containment therefore
// resolves every target component by component, chasing chains through the
// manifest's own links, tracking how deep the walk really is at each step.
//
// An absolute target is legal and means volume-root-absolute: "/x" is the
// volume's own x, never the reader's /x. Targets are stored exactly as
// written; only the form created on disk is rendered relative, because a
// kernel has no encoding for "absolute, rooted at this subtree" — see
// RelativeLinkTarget.

// maxSymlinkHops caps how many links one resolution may chase. It matches
// the limit kernels enforce before returning ELOOP, so a chain the format
// accepts is also one a real filesystem would follow.
const maxSymlinkHops = 40

// ErrNotContained marks a tree or manifest that violates containment: a
// symlink escapes the volume root, loops, has an empty target, or an entry's
// parent is recorded as something a directory could never be.
var ErrNotContained = errors.New("not contained in the volume")

// ContainmentWarningKind names what the lenient profile found, so a caller
// can branch without parsing prose.
type ContainmentWarningKind uint8

const (
	// WarningDanglingLink: the symlink's target resolves to a path the
	// manifest has nothing at.
	WarningDanglingLink ContainmentWarningKind = iota + 1
	// WarningLinkThroughFile: resolution walks through an entry recorded as
	// a file, which a real filesystem answers with ENOTDIR.
	WarningLinkThroughFile
	// WarningParentUnrecorded: the entry's ancestors up to the nearest
	// recorded one are all implicit — nothing records the parent directory.
	WarningParentUnrecorded
)

// ContainmentWarning is a finding the lenient profile reports rather than
// fails on: a dangling link, a link resolving through a file, or an entry
// whose parent has no record. Harmless to write out, present in manifests
// that predate the rule, and worth telling the caller about.
type ContainmentWarning struct {
	// Path is the entry the finding is about.
	Path string
	// Kind is the finding, typed.
	Kind ContainmentWarningKind
	// Detail says what was found, in prose.
	Detail string
}

// String renders the finding for a human; the typed fields are the API.
func (w ContainmentWarning) String() string {
	return w.Path + ": " + w.Detail
}

// CheckSourceContainment is the strict profile, for a push: every link
// finding is an error, including a dangling link, because a version is
// immutable — the target a link dangles on can never appear later. It runs
// before the first byte uploads, so a tree the format would refuse fails
// here with the entry named instead of at the server with the whole upload
// spent. A parent with no record of its own is NOT a finding: it is an
// implicit directory — producers may omit directory entries, and
// materialization creates them — and the format accepts it everywhere. Only
// a parent chain that reaches a recorded file or symlink contradicts itself.
func CheckSourceContainment(src *Source) error {
	ns, err := buildLinkNamespace(sourceEntries(src))
	if err != nil {
		return err
	}
	for _, issue := range ns.parentIssues() {
		if issue.missing {
			continue
		}
		return fmt.Errorf("%w: the parent of %s is recorded as something other than a directory", ErrNotContained, ns.pathOf(issue.id))
	}
	for _, id := range ns.links() {
		hops := 0
		res, err := ns.resolveLink(id, &hops)
		if err != nil {
			return err
		}
		switch res.kind {
		case linkContained:
		case linkDangling:
			return fmt.Errorf("%w: symlink %s dangles: nothing in the tree is at %s, and a published version can never gain it",
				ErrNotContained, ns.pathOf(id), res.at)
		case linkTraversesFile:
			return fmt.Errorf("%w: symlink %s resolves through %s, which is a file", ErrNotContained, ns.pathOf(id), res.at)
		}
	}
	return nil
}

// CheckManifestContainment is the lenient profile, for writing a manifest
// out: escapes, loops, empty targets, and a parent recorded as a
// non-directory are errors — a tree like that must not be materialized — but
// dangling links, links through files, and parents with no record at all are
// warnings, because manifests predating the rule carry them and they write
// out harmlessly.
func CheckManifestContainment(m *Manifest) ([]ContainmentWarning, error) {
	ns, err := buildLinkNamespace(manifestEntries(m))
	if err != nil {
		return nil, err
	}
	var warnings []ContainmentWarning
	for _, issue := range ns.parentIssues() {
		if issue.missing {
			warnings = append(warnings, ContainmentWarning{
				Path:   ns.pathOf(issue.id),
				Kind:   WarningParentUnrecorded,
				Detail: "no directory record for its parent",
			})
			continue
		}
		return nil, fmt.Errorf("%w: the parent of %s is recorded as something other than a directory", ErrNotContained, ns.pathOf(issue.id))
	}
	for _, id := range ns.links() {
		hops := 0
		res, err := ns.resolveLink(id, &hops)
		if err != nil {
			return nil, err
		}
		switch res.kind {
		case linkContained:
		case linkDangling:
			warnings = append(warnings, ContainmentWarning{
				Path:   ns.pathOf(id),
				Kind:   WarningDanglingLink,
				Detail: fmt.Sprintf("dangles: the manifest has nothing at %s", res.at),
			})
		case linkTraversesFile:
			warnings = append(warnings, ContainmentWarning{
				Path:   ns.pathOf(id),
				Kind:   WarningLinkThroughFile,
				Detail: fmt.Sprintf("resolves through %s, which is a file", res.at),
			})
		}
	}
	return warnings, nil
}

// RelativeLinkTarget renders a volume-root-absolute target to the form a
// symlink at linkPath should carry on disk. The only portable on-disk
// encoding is relative: a contained relative link resolves correctly whether
// the tree is a subtree mount or has become a container's root, while an
// absolute string would resolve against whatever the reader calls "/". The
// rendered form climbs to the volume root and descends the target's own
// components; "." and empty components are dropped, ".." components pass
// through, since containment has already been judged on the stored form.
func RelativeLinkTarget(linkPath, target string) string {
	depth := strings.Count(linkPath, "/")
	parts := make([]string, 0, depth+8)
	for range depth {
		parts = append(parts, "..")
	}
	for _, component := range strings.Split(target, "/") {
		if component == "" || component == "." {
			continue
		}
		parts = append(parts, component)
	}
	if len(parts) == 0 {
		return "."
	}
	return strings.Join(parts, "/")
}

// ---------------------------------------------------------------------------
// The namespace the resolution walks.

type linkEntry struct {
	path   string
	kind   nsKind
	target string
}

func sourceEntries(src *Source) []linkEntry {
	entries := make([]linkEntry, 0, len(src.Directories)+len(src.Files)+len(src.Symlinks))
	for _, d := range src.Directories {
		entries = append(entries, linkEntry{path: d.Path, kind: nsDirectory})
	}
	for _, f := range src.Files {
		entries = append(entries, linkEntry{path: f.Path, kind: nsFile})
	}
	for _, s := range src.Symlinks {
		entries = append(entries, linkEntry{path: s.Path, kind: nsSymlink, target: s.Target})
	}
	return entries
}

func manifestEntries(m *Manifest) []linkEntry {
	entries := make([]linkEntry, 0, len(m.Directories)+len(m.Files)+len(m.Symlinks))
	for _, d := range m.Directories {
		entries = append(entries, linkEntry{path: d.Path, kind: nsDirectory})
	}
	for _, f := range m.Files {
		entries = append(entries, linkEntry{path: f.Path, kind: nsFile})
	}
	for _, s := range m.Symlinks {
		entries = append(entries, linkEntry{path: s.Path, kind: nsSymlink, target: s.Target})
	}
	return entries
}

type nsKind uint8

const (
	// nsPlaceholder is a path that exists only as an ancestor of some entry,
	// with no record of its own. It resolves like a directory.
	nsPlaceholder nsKind = iota
	nsDirectory
	nsFile
	nsSymlink
)

const nsRoot int32 = 0

type nsNode struct {
	parent int32
	name   string
	kind   nsKind
	target string
}

type linkNamespace struct {
	nodes    []nsNode
	children map[int32]map[string]int32
}

// buildLinkNamespace interns entries into one tree. Paths are expected to
// have passed ValidatePath already; they are checked again here because a
// walk over a malformed path would intern garbage, and the second check is
// cheap next to the first being forgotten.
func buildLinkNamespace(entries []linkEntry) (*linkNamespace, error) {
	ns := &linkNamespace{
		nodes:    []nsNode{{parent: nsRoot, name: "", kind: nsDirectory}},
		children: make(map[int32]map[string]int32),
	}
	for _, entry := range entries {
		if err := ValidatePath(entry.path); err != nil {
			return nil, err
		}
		cur := nsRoot
		rest := entry.path
		for {
			name, tail, more := strings.Cut(rest, "/")
			cur = ns.childOrPlaceholder(cur, name)
			if !more {
				node := &ns.nodes[cur]
				if node.kind != nsPlaceholder {
					return nil, fmt.Errorf("path %q appears twice", entry.path)
				}
				node.kind = entry.kind
				node.target = entry.target
				break
			}
			rest = tail
		}
	}
	return ns, nil
}

func (ns *linkNamespace) childOrPlaceholder(dir int32, name string) int32 {
	if id, ok := ns.children[dir][name]; ok {
		return id
	}
	id := int32(len(ns.nodes))
	ns.nodes = append(ns.nodes, nsNode{parent: dir, name: name, kind: nsPlaceholder})
	if ns.children[dir] == nil {
		ns.children[dir] = make(map[string]int32)
	}
	ns.children[dir][name] = id
	return id
}

// parentIssue is an entry whose parent chain is not made of directory
// records: missing when the chain reaches the nearest recorded ancestor
// without finding a record for the parent, otherwise that nearest recorded
// ancestor is a file or symlink and nothing below it can exist at all.
type parentIssue struct {
	id      int32
	missing bool
}

// parentIssues lists them in the order the entries were interned, so the
// error a caller reports is decided by the manifest and not by map order.
//
// The classification walks past implicit ancestors to the NEAREST RECORDED
// one, rather than looking only at the immediate parent: a file at a/b/c
// with a SYMLINK recorded at a is not "missing a parent record" — no real
// filesystem can hold entries below a link, however many implicit levels
// sit in between — so it is the fatal kind, not the warning kind.
func (ns *linkNamespace) parentIssues() []parentIssue {
	var issues []parentIssue
	for i := 1; i < len(ns.nodes); i++ {
		node := ns.nodes[i]
		if node.kind == nsPlaceholder {
			continue
		}
		ancestor := node.parent
		for ns.nodes[ancestor].kind == nsPlaceholder {
			ancestor = ns.nodes[ancestor].parent
		}
		switch ns.nodes[ancestor].kind {
		case nsDirectory:
			if ns.nodes[node.parent].kind == nsPlaceholder {
				issues = append(issues, parentIssue{id: int32(i), missing: true})
			}
		case nsFile, nsSymlink:
			issues = append(issues, parentIssue{id: int32(i), missing: false})
		}
	}
	return issues
}

func (ns *linkNamespace) links() []int32 {
	var ids []int32
	for i := 1; i < len(ns.nodes); i++ {
		if ns.nodes[i].kind == nsSymlink {
			ids = append(ids, int32(i))
		}
	}
	return ids
}

// pathOf and depthOf walk parent pointers to the root. The walks need no
// bound: nodes are appended as paths intern, every node's parent id is
// strictly smaller than its own, and the root is node zero — so a parent
// chain strictly decreases and terminates by construction. A namespace
// built any other way would need the bound the serving side of the format
// uses against corrupt trees; this one cannot express a cycle.
func (ns *linkNamespace) pathOf(id int32) string {
	if id == nsRoot {
		return "."
	}
	var parts []string
	for cur := id; cur != nsRoot; cur = ns.nodes[cur].parent {
		parts = append(parts, ns.nodes[cur].name)
	}
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return strings.Join(parts, "/")
}

func (ns *linkNamespace) depthOf(id int32) int {
	depth := 0
	for cur := id; cur != nsRoot; cur = ns.nodes[cur].parent {
		depth++
	}
	return depth
}

type resolutionKind uint8

const (
	linkContained resolutionKind = iota
	// linkDangling stops at a path the manifest has nothing at. At a DIRECT
	// stop — a missing child, or a mid-path file — the unwalked remainder is
	// judged lexically for escape from the stop's real depth. A CHASED stop
	// propagates without judging the chasing link's remainder: any reader
	// fails at the chased link first and never reaches that remainder, and
	// the chased link was judged as an entry of its own — the format's rule,
	// matched deliberately; judging further would refuse manifests the
	// format accepts. The caveat that buys: if the missing path is later
	// filled out-of-band, the chain comes alive and the remainder applies —
	// the pull's dangling warning is the visibility surface for that.
	linkDangling
	// linkTraversesFile walks through an entry recorded as a file, which a
	// real filesystem would answer with ENOTDIR.
	linkTraversesFile
)

type linkResolution struct {
	kind resolutionKind
	// node is where a contained resolution lands; the root means the volume
	// root itself.
	node int32
	// at is the missing path for a dangling link, or the file's path for one
	// that resolves through a file.
	at string
}

// resolveLink walks one symlink's target to wherever it really leads.
//
// The walk tracks the real depth of the current node below the root rather
// than counting the target's components, because a chased link can land
// shallower than its spelling suggests ("up -> ..") and a later ".." then
// escapes from that shallower point. Escapes, loops past the hop cap, and
// empty targets are errors; where the walk stops short of a verdict, the
// caller's profile decides what a dangling or file-crossing link means.
// Scale: the check is linear in the entry count — the shared hop budget
// caps the chases any one link's resolution can spend, so no manifest shape
// makes the walk superlinear. The ceiling is therefore set by how many
// entries a manifest can carry, which MaxManifestBytes bounds; raising that
// constant raises this ceiling with it, and is the trigger to revisit.
// Measured at 130k adversarial links the worst shape ran in about half a
// second; the shapes stay executable in BenchmarkContainmentAdversarial.
// If this pass ever runs hot, the pre-designed remedy is a per-link memo of
// verdict plus chain-hop cost, the cost replayed before the budget check so
// the loop boundary lands exactly where the unmemoized walk puts it, with
// budget-truncated results never cached — the one budget-dependent case.
func (ns *linkNamespace) resolveLink(link int32, hops *int) (linkResolution, error) {
	node := ns.nodes[link]
	if node.kind != nsSymlink {
		return linkResolution{kind: linkContained, node: link}, nil
	}
	target := node.target
	if target == "" {
		return linkResolution{}, fmt.Errorf("%w: symlink %s has no target", ErrNotContained, ns.pathOf(link))
	}
	if strings.ContainsRune(target, 0) {
		// A NUL would pass the resolution walk, but no such link can be
		// created — the system call takes a C string — so the format rejects
		// it rather than letting a consumer trip over it.
		return linkResolution{}, fmt.Errorf("%w: symlink %s target contains a NUL byte", ErrNotContained, ns.pathOf(link))
	}

	var comps []string
	for _, c := range strings.Split(target, "/") {
		if c != "" && c != "." {
			comps = append(comps, c)
		}
	}
	// An absolute target anchors at the volume root; a relative one at the
	// link's own directory.
	cur := ns.nodes[link].parent
	if strings.HasPrefix(target, "/") {
		cur = nsRoot
	}
	depth := ns.depthOf(cur)

	for i, comp := range comps {
		last := i+1 == len(comps)
		if comp == ".." {
			if depth == 0 {
				return linkResolution{}, ns.escape(link, target)
			}
			depth--
			cur = ns.nodes[cur].parent
			continue
		}
		next, ok := ns.children[cur][comp]
		if !ok {
			// The manifest ends here. Whatever fills this path at read time
			// is a plain directory, so the remainder is judged lexically
			// from the missing child's depth.
			if remainderEscapes(depth+1, comps[i+1:]) {
				return linkResolution{}, ns.escape(link, target)
			}
			missing := comp
			if parent := ns.pathOf(cur); parent != "." {
				missing = parent + "/" + comp
			}
			return linkResolution{kind: linkDangling, at: missing}, nil
		}
		switch ns.nodes[next].kind {
		case nsPlaceholder, nsDirectory:
			cur = next
			depth++
		case nsFile:
			if !last {
				// A real filesystem stops with ENOTDIR before applying the
				// remainder, but a remainder that would escape is checked
				// anyway: such a link is broken either way, and rejecting it
				// cannot lose a valid one.
				if remainderEscapes(depth+1, comps[i+1:]) {
					return linkResolution{}, ns.escape(link, target)
				}
				return linkResolution{kind: linkTraversesFile, at: ns.pathOf(next)}, nil
			}
			cur = next
		case nsSymlink:
			*hops++
			if *hops > maxSymlinkHops {
				return linkResolution{}, fmt.Errorf("%w: symlink %s chases more than %d links (a loop, or as good as one)",
					ErrNotContained, ns.pathOf(link), maxSymlinkHops)
			}
			res, err := ns.resolveLink(next, hops)
			if err != nil {
				return linkResolution{}, err
			}
			if res.kind != linkContained {
				// The chased link stops before its own target resolves; a
				// reader following this link fails there too and never
				// reaches the remainder. The chased link is its own entry
				// and is judged on its own.
				return res, nil
			}
			if !last && ns.nodes[res.node].kind == nsFile {
				return linkResolution{kind: linkTraversesFile, at: ns.pathOf(res.node)}, nil
			}
			cur = res.node
			depth = ns.depthOf(res.node)
		}
	}
	return linkResolution{kind: linkContained, node: cur}, nil
}

func (ns *linkNamespace) escape(link int32, target string) error {
	return fmt.Errorf("%w: symlink %s target %q steps outside the volume root", ErrNotContained, ns.pathOf(link), target)
}

// remainderEscapes judges the components past a resolution stop lexically
// from the stop's real depth: ".." pops, a name pushes, popping above the
// root is an escape. Nothing in the manifest remains to resolve there, so
// lexical is exact.
func remainderEscapes(depth int, rest []string) bool {
	for _, comp := range rest {
		if comp == ".." {
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
