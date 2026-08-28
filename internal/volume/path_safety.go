package volume

import (
	"strings"
	"unicode/utf8"
)

type portablePathLimits struct {
	maxPathBytes      int
	maxPathComponents int
}

func defaultPortablePathLimits() portablePathLimits {
	return portablePathLimits{
		maxPathBytes:      defaultMaxPortablePathBytes,
		maxPathComponents: defaultMaxPortablePathComponents,
	}
}

func (c *Client) effectivePortablePathLimits() portablePathLimits {
	if c == nil ||
		c.portablePathLimits.maxPathBytes < 1 ||
		c.portablePathLimits.maxPathComponents < 1 ||
		c.portablePathLimits.maxPathComponents > maximumPortablePathComponents {
		return defaultPortablePathLimits()
	}
	return c.portablePathLimits
}

func selectPortablePathLimits(configured []portablePathLimits) (portablePathLimits, error) {
	limits := defaultPortablePathLimits()
	if len(configured) != 0 {
		limits = configured[0]
	}
	if len(configured) > 1 ||
		limits.maxPathBytes < 1 ||
		limits.maxPathComponents < 1 {
		return portablePathLimits{}, preconditionError(
			"validate path limits",
			"portable path limits must be positive",
		)
	}
	if limits.maxPathComponents > maximumPortablePathComponents {
		return portablePathLimits{}, preconditionError(
			"validate path limits",
			"portable path component limit exceeds the hard maximum",
		)
	}
	return limits, nil
}

func validatePathResourceLimits(value string, limits portablePathLimits) error {
	if len(value) > limits.maxPathBytes {
		return preconditionError(
			"validate path limits",
			"path exceeds the configured byte limit",
		)
	}
	components := 1
	for _, character := range value {
		if character == '/' || character == '\\' {
			components++
			if components > limits.maxPathComponents {
				return preconditionError(
					"validate path limits",
					"path exceeds the configured component limit",
				)
			}
		}
	}
	return nil
}

func validatePortablePath(value string, configured ...portablePathLimits) error {
	limits, err := selectPortablePathLimits(configured)
	if err != nil {
		return err
	}
	return validatePortablePathWithLimits(value, limits)
}

func validatePortablePathWithLimits(value string, limits portablePathLimits) error {
	if value == "" {
		return protocolError("validate manifest path", "manifest path must not be empty")
	}
	if !utf8.ValidString(value) {
		return protocolError("validate manifest path", "manifest path must be valid UTF-8")
	}
	if err := validatePathResourceLimits(value, limits); err != nil {
		return err
	}
	if strings.HasPrefix(value, "/") || strings.HasPrefix(value, `\`) {
		return protocolError("validate manifest path", "manifest path must be relative")
	}
	if strings.Contains(value, `\`) {
		return protocolError("validate manifest path", "manifest path must use '/' separators")
	}
	firstEnd := strings.IndexByte(value, '/')
	if firstEnd < 0 {
		firstEnd = len(value)
	}
	if firstEnd >= 2 && value[1] == ':' {
		return protocolError("validate manifest path", "manifest path must not have a platform prefix")
	}
	components := 0
	for start := 0; ; {
		relativeEnd := strings.IndexByte(value[start:], '/')
		end := len(value)
		if relativeEnd >= 0 {
			end = start + relativeEnd
		}
		component := value[start:end]
		if component == "" || component == "." || component == ".." {
			return protocolError("validate manifest path", "manifest path contains an invalid component")
		}
		if strings.ContainsRune(component, 0) {
			return protocolError("validate manifest path", "manifest path contains a null byte")
		}
		components++
		if components > limits.maxPathComponents {
			return preconditionError(
				"validate path limits",
				"path exceeds the configured component limit",
			)
		}
		if relativeEnd < 0 {
			break
		}
		start = end + 1
	}
	return nil
}

func validatePortablePathSyntax(value string) error {
	maximum := int(^uint(0) >> 1)
	return validatePortablePathWithLimits(value, portablePathLimits{
		maxPathBytes:      maximum,
		maxPathComponents: maximum,
	})
}

func validateSymlinkTarget(
	linkPath string,
	target string,
	configured ...portablePathLimits,
) error {
	limits, err := selectPortablePathLimits(configured)
	if err != nil {
		return err
	}
	if target == "" ||
		!utf8.ValidString(target) ||
		strings.HasPrefix(target, "/") ||
		strings.HasPrefix(target, `\`) ||
		strings.Contains(target, `\`) {
		return protocolError("validate symlink", "symlink has an invalid target")
	}
	if err := validatePathResourceLimits(target, limits); err != nil {
		return err
	}
	firstEnd := strings.IndexByte(target, '/')
	if firstEnd < 0 {
		firstEnd = len(target)
	}
	if firstEnd >= 2 && target[1] == ':' {
		return protocolError("validate symlink", "symlink target has a platform prefix")
	}

	// This lexical depth check is a load-bearing prefilter for the graph
	// resolver below: it rejects impossible escapes before an attacker can
	// force expansion through other manifest symlinks. Graph resolution remains
	// authoritative for composed targets.
	depth := strings.Count(linkPath, "/")
	components := 0
	for start := 0; ; {
		relativeEnd := strings.IndexByte(target[start:], '/')
		end := len(target)
		if relativeEnd >= 0 {
			end = start + relativeEnd
		}
		component := target[start:end]
		components++
		if components > limits.maxPathComponents {
			return preconditionError(
				"validate path limits",
				"symlink target exceeds the configured component limit",
			)
		}
		switch component {
		case "", ".":
			return protocolError("validate symlink", "symlink target contains an invalid component")
		case "..":
			if depth == 0 {
				return protocolError("validate symlink", "symlink target escapes the manifest root")
			}
			depth--
		default:
			if strings.ContainsRune(component, 0) {
				return protocolError("validate symlink", "symlink target contains a null byte")
			}
			depth++
		}
		if relativeEnd < 0 {
			break
		}
		start = end + 1
	}
	return nil
}

type manifestPathKind uint8

const (
	manifestPathImplicitDirectory manifestPathKind = iota
	manifestPathDirectory
	manifestPathFile
	manifestPathSymlink
)

type manifestPathNode struct {
	kind          manifestPathKind
	explicit      bool
	symlinkTarget string
	components    []string
	children      map[string]*manifestPathNode
}

type manifestPathIndex struct {
	root            *manifestPathNode
	limits          portablePathLimits
	maxTotalEntries int
	totalEntries    int
	symlinks        []*manifestPathNode
}

func newManifestPathIndex(
	maxTotalEntries int,
	limits portablePathLimits,
) (*manifestPathIndex, error) {
	if maxTotalEntries < 1 {
		return nil, preconditionError(
			"validate manifest",
			"manifest total-entry limit must be positive",
		)
	}
	if _, err := selectPortablePathLimits([]portablePathLimits{limits}); err != nil {
		return nil, err
	}
	return &manifestPathIndex{
		root:            &manifestPathNode{children: make(map[string]*manifestPathNode)},
		limits:          limits,
		maxTotalEntries: maxTotalEntries,
	}, nil
}

func (index *manifestPathIndex) insert(
	path string,
	kind manifestPathKind,
	symlinkTarget string,
) error {
	if err := validatePortablePath(path, index.limits); err != nil {
		return err
	}
	if kind == manifestPathSymlink {
		if err := validateSymlinkTarget(path, symlinkTarget, index.limits); err != nil {
			return err
		}
	}
	components := strings.Split(path, "/")
	node := index.root
	for _, component := range components {
		if node.explicit &&
			node.kind != manifestPathDirectory &&
			node.kind != manifestPathImplicitDirectory {
			return protocolError(
				"validate manifest",
				"manifest path is below a non-directory path",
			)
		}
		child := node.children[component]
		if child == nil {
			if index.totalEntries == index.maxTotalEntries {
				return preconditionError(
					"validate manifest",
					"manifest exceeds the configured total-entry limit",
				)
			}
			child = &manifestPathNode{
				kind:     manifestPathImplicitDirectory,
				children: make(map[string]*manifestPathNode),
			}
			node.children[component] = child
			index.totalEntries++
		}
		node = child
	}
	if node.explicit {
		return protocolError("validate manifest", "manifest contains a duplicate or conflicting path")
	}
	if kind != manifestPathDirectory && len(node.children) != 0 {
		return protocolError(
			"validate manifest",
			"manifest non-directory path contains another entry",
		)
	}
	node.explicit = true
	node.kind = kind
	node.symlinkTarget = symlinkTarget
	if kind == manifestPathSymlink {
		node.components = append([]string(nil), components...)
		index.symlinks = append(index.symlinks, node)
	}
	return nil
}

type symlinkResolution struct {
	components     []string
	nodes          []*manifestPathNode
	pathBytes      uint64
	expansionDepth int
	expansionCount int
}

type symlinkResolveState uint8

const (
	symlinkUnresolved symlinkResolveState = iota
	symlinkResolving
	symlinkResolved
)

type manifestSymlinkResolver struct {
	index             *manifestPathIndex
	states            map[*manifestPathNode]symlinkResolveState
	resolved          map[*manifestPathNode]symlinkResolution
	expansions        int
	componentsVisited int
}

func (index *manifestPathIndex) validateSymlinks() error {
	resolver := &manifestSymlinkResolver{
		index:    index,
		states:   make(map[*manifestPathNode]symlinkResolveState, len(index.symlinks)),
		resolved: make(map[*manifestPathNode]symlinkResolution, len(index.symlinks)),
	}
	return resolver.validate()
}

func (resolver *manifestSymlinkResolver) validate() error {
	for _, symlink := range resolver.index.symlinks {
		if _, err := resolver.resolve(symlink, 1); err != nil {
			return err
		}
	}
	return nil
}

func (resolver *manifestSymlinkResolver) resolve(
	symlink *manifestPathNode,
	depth int,
) (symlinkResolution, error) {
	switch resolver.states[symlink] {
	case symlinkResolved:
		result := resolver.resolved[symlink]
		if depth+result.expansionDepth-1 > resolver.index.limits.maxPathComponents {
			return symlinkResolution{}, preconditionError(
				"validate symlink graph",
				"manifest symlink expansion exceeds the configured component limit",
			)
		}
		return result, nil
	case symlinkResolving:
		return symlinkResolution{}, protocolError(
			"validate symlink graph",
			"manifest symlink graph contains a cycle",
		)
	}
	if depth > resolver.index.limits.maxPathComponents {
		return symlinkResolution{}, preconditionError(
			"validate symlink graph",
			"manifest symlink expansion exceeds the configured component limit",
		)
	}
	resolver.states[symlink] = symlinkResolving
	resolver.expansions++

	parentComponents := symlink.components[:len(symlink.components)-1]
	components := append([]string(nil), parentComponents...)
	nodes := make([]*manifestPathNode, 0, len(parentComponents))
	node := resolver.index.root
	var pathBytes uint64
	for _, component := range parentComponents {
		node = node.children[component]
		nodes = append(nodes, node)
		pathBytes = appendPathComponentBytes(pathBytes, component)
	}

	pending := strings.Split(symlink.symlinkTarget, "/")
	expansionDepth := 1
	expansionCount := 1
	for len(pending) != 0 {
		resolver.componentsVisited++
		if len(nodes) != 0 {
			current := nodes[len(nodes)-1]
			if current != nil && current.explicit && current.kind == manifestPathFile {
				return symlinkResolution{}, protocolError(
					"validate symlink graph",
					"manifest symlink traverses a non-directory entry",
				)
			}
		}
		component := pending[0]
		pending = pending[1:]
		if component == ".." {
			if len(components) == 0 {
				return symlinkResolution{}, protocolError(
					"validate symlink graph",
					"manifest symlink graph escapes the manifest root",
				)
			}
			pathBytes = removePathComponentBytes(pathBytes, components[len(components)-1])
			components = components[:len(components)-1]
			nodes = nodes[:len(nodes)-1]
			continue
		}

		var next *manifestPathNode
		if len(nodes) == 0 {
			next = resolver.index.root.children[component]
		} else if parent := nodes[len(nodes)-1]; parent != nil {
			next = parent.children[component]
		}
		components = append(components, component)
		nodes = append(nodes, next)
		pathBytes = appendPathComponentBytes(pathBytes, component)
		if len(components) > resolver.index.limits.maxPathComponents ||
			pathBytes > uint64(resolver.index.limits.maxPathBytes) {
			return symlinkResolution{}, preconditionError(
				"validate symlink graph",
				"expanded symlink path exceeds the configured path limits",
			)
		}
		if next == nil || !next.explicit || next.kind != manifestPathSymlink {
			continue
		}
		target, err := resolver.resolve(next, depth+1)
		if err != nil {
			return symlinkResolution{}, err
		}
		components = append(components[:0], target.components...)
		nodes = append(nodes[:0], target.nodes...)
		pathBytes = target.pathBytes
		expansionDepth = max(expansionDepth, 1+target.expansionDepth)
		// Reusing an already resolved link does not bypass the expansion budget.
		// This intentionally rejects sufficiently branching acyclic graphs as
		// ambiguous instead of allowing their cumulative expansion work to grow.
		if target.expansionCount >
			resolver.index.limits.maxPathComponents-expansionCount {
			return symlinkResolution{}, preconditionError(
				"validate symlink graph",
				"manifest symlink expansion exceeds the configured component limit",
			)
		}
		expansionCount += target.expansionCount
	}

	result := symlinkResolution{
		components:     append([]string(nil), components...),
		nodes:          append([]*manifestPathNode(nil), nodes...),
		pathBytes:      pathBytes,
		expansionDepth: expansionDepth,
		expansionCount: expansionCount,
	}
	resolver.states[symlink] = symlinkResolved
	resolver.resolved[symlink] = result
	return result, nil
}

func appendPathComponentBytes(pathBytes uint64, component string) uint64 {
	if pathBytes == 0 {
		return uint64(len(component))
	}
	return pathBytes + 1 + uint64(len(component))
}

func removePathComponentBytes(pathBytes uint64, component string) uint64 {
	if pathBytes == uint64(len(component)) {
		return 0
	}
	return pathBytes - uint64(len(component)) - 1
}

func validateManifestStructure(
	manifest validatedManifest,
	maxTotalEntries int,
	limits portablePathLimits,
) error {
	index, err := newManifestPathIndex(maxTotalEntries, limits)
	if err != nil {
		return err
	}
	for _, directory := range manifest.Directories {
		if err := index.insert(directory.Path, manifestPathDirectory, ""); err != nil {
			return err
		}
	}
	for _, file := range manifest.Files {
		if err := index.insert(file.Path, manifestPathFile, ""); err != nil {
			return err
		}
	}
	for _, symlink := range manifest.Symlinks {
		if err := index.insert(symlink.Path, manifestPathSymlink, symlink.Target); err != nil {
			return err
		}
	}
	return index.validateSymlinks()
}
