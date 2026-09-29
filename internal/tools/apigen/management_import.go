package main

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
)

// importSandboxManagement preserves the existing API baseline and imports only
// sandbox routes and the components reachable from them. A differing shared
// component requires deliberate review rather than an unrelated API change.
func importSandboxManagement(baseline, snapshot []byte) ([]byte, error) {
	var base, source map[string]any
	if err := json.Unmarshal(baseline, &base); err != nil {
		return nil, fmt.Errorf("baseline: %w", err)
	}
	if err := json.Unmarshal(snapshot, &source); err != nil {
		return nil, fmt.Errorf("snapshot: %w", err)
	}
	paths := mapNode(base["paths"])
	if paths == nil {
		return nil, fmt.Errorf("baseline has no paths")
	}
	components := mapNode(base["components"])
	if components == nil {
		components = map[string]any{}
		base["components"] = components
	}
	sandboxRoots, otherRoots := []any{}, []any{}
	for path, value := range paths {
		if sandboxManagementPath(path) {
			sandboxRoots = append(sandboxRoots, value)
		} else {
			otherRoots = append(otherRoots, value)
		}
	}
	sandboxRefs := collectComponentRefs(base, sandboxRoots)
	otherRefs := collectComponentRefs(base, otherRoots)
	seen := map[string]bool{}
	var visit func(any) error
	visit = func(value any) error {
		switch node := value.(type) {
		case map[string]any:
			for key, child := range node {
				if ref, ok := child.(string); ok && (key == "$ref" || strings.HasPrefix(ref, "#/components/")) {
					if !strings.HasPrefix(ref, "#/components/") {
						return fmt.Errorf("unsupported reference %q", ref)
					}
					if seen[ref] {
						continue
					}
					seen[ref] = true
					parts := strings.Split(strings.TrimPrefix(ref, "#/components/"), "/")
					if len(parts) != 2 {
						return fmt.Errorf("unsupported component reference %q", ref)
					}
					kind, name := parts[0], strings.ReplaceAll(strings.ReplaceAll(parts[1], "~1", "/"), "~0", "~")
					group := mapNode(mapNode(source["components"])[kind])
					component, ok := group[name]
					if !ok {
						return fmt.Errorf("missing component %q", ref)
					}
					dest := mapNode(components[kind])
					if dest == nil {
						dest = map[string]any{}
						components[kind] = dest
					}
					if existing, ok := dest[name]; ok && !reflect.DeepEqual(existing, component) && (!sandboxRefs[ref] || otherRefs[ref]) {
						return fmt.Errorf("sandbox import changes shared component %q; review the conflict explicitly", ref)
					}
					dest[name] = component
					if err := visit(component); err != nil {
						return err
					}
				} else if err := visit(child); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range node {
				if err := visit(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	imported := 0
	for path, operation := range mapNode(source["paths"]) {
		if !sandboxManagementPath(path) {
			continue
		}
		if err := visit(operation); err != nil {
			return nil, err
		}
		paths[path] = operation
		imported++
	}
	if imported == 0 {
		return nil, fmt.Errorf("snapshot has no sandbox/token operations")
	}
	return json.MarshalIndent(base, "", "    ")
}

func downloadSandboxManagementSpec(url, destination string) error {
	baseline, err := os.ReadFile(destination)
	if err != nil {
		return err
	}
	snapshot, err := downloadSpec(url)
	if err != nil {
		return err
	}
	merged, err := importSandboxManagement(baseline, snapshot)
	if err != nil {
		return err
	}
	return os.WriteFile(destination, merged, 0644)
}

func sandboxManagementPath(path string) bool {
	return path == "/v1/token" || strings.HasPrefix(path, "/v1/sandboxes/")
}

// Count baseline reachability before replacing any selected path. Components
// shared with an unrelated endpoint remain protected from sandbox-only refresh.
func collectComponentRefs(document map[string]any, roots []any) map[string]bool {
	refs := map[string]bool{}
	var visit func(any)
	visit = func(value any) {
		switch node := value.(type) {
		case map[string]any:
			for _, child := range node {
				if ref, ok := child.(string); ok && strings.HasPrefix(ref, "#/components/") {
					if !refs[ref] {
						refs[ref] = true
						visit(resolveRef(document, map[string]any{"$ref": ref}))
					}
				} else {
					visit(child)
				}
			}
		case []any:
			for _, child := range node {
				visit(child)
			}
		}
	}
	for _, root := range roots {
		visit(root)
	}
	return refs
}
