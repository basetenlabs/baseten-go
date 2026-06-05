package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"strings"
)

// preprocessedSpec is the result of preprocessSpec: the transformed JSON
// bytes plus a sidecar `discriminator.mapping` table keyed by Go schema name
// (post V1 stripping). oapi-codegen ignores discriminator mappings, so
// postProcess restores wire values from this table.
type preprocessedSpec struct {
	data                []byte
	discriminatorValues map[string]string
}

// preprocessSpec transforms an OpenAPI 3.1 spec to 3.0-compatible form so
// that oapi-codegen can process it, and cleans up schema names.
func preprocessSpec(data []byte) (*preprocessedSpec, error) {
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parsing spec JSON: %w", err)
	}

	if v, ok := doc["openapi"].(string); ok && strings.HasPrefix(v, "3.1") {
		doc["openapi"] = "3.0.3"
	}

	// Build V1-suffix rename map from schema names before walking.
	schemaRenames := buildSchemaRenames(doc)

	discriminatorValues := map[string]string{}
	if err := preprocessNode(doc, schemaRenames, discriminatorValues); err != nil {
		return nil, err
	}

	// Rename the schema keys themselves.
	if schemas := componentSchemas(doc); schemas != nil {
		for old, renamed := range schemaRenames {
			schemas[renamed] = schemas[old]
			delete(schemas, old)
		}
	}

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshaling converted spec: %w", err)
	}
	return &preprocessedSpec{data: out, discriminatorValues: discriminatorValues}, nil
}

func componentSchemas(doc map[string]any) map[string]any {
	components, _ := doc["components"].(map[string]any)
	if components == nil {
		return nil
	}
	schemas, _ := components["schemas"].(map[string]any)
	return schemas
}

// buildSchemaRenames returns a map from old schema name to new name for
// schemas whose names end with "V1". This produces cleaner Go type names
// (e.g. "Model" instead of "ModelV1").
func buildSchemaRenames(doc map[string]any) map[string]string {
	schemas := componentSchemas(doc)
	if schemas == nil {
		return nil
	}
	renames := map[string]string{}
	for name := range schemas {
		if trimmed, ok := strings.CutSuffix(name, "V1"); ok {
			renames[name] = trimmed
		}
	}
	return renames
}

func preprocessNode(node any, schemaRenames map[string]string, discriminatorValues map[string]string) error {
	switch v := node.(type) {
	case map[string]any:
		if err := preprocessSchema(v, schemaRenames, discriminatorValues); err != nil {
			return err
		}
		for _, child := range v {
			if err := preprocessNode(child, schemaRenames, discriminatorValues); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range v {
			if err := preprocessNode(child, schemaRenames, discriminatorValues); err != nil {
				return err
			}
		}
	}
	return nil
}

// preprocessSchema handles nullable patterns and $ref renames in a single
// schema object, and harvests `discriminator.mapping` entries.
func preprocessSchema(schema map[string]any, schemaRenames map[string]string, discriminatorValues map[string]string) error {
	// Convert type arrays: {"type": ["string", "null"]} -> {"type": "string", "nullable": true}
	if t, ok := schema["type"]; ok {
		if arr, ok := t.([]any); ok {
			var nonNull []string
			hasNull := false
			for _, item := range arr {
				if s, ok := item.(string); ok {
					if s == "null" {
						hasNull = true
					} else {
						nonNull = append(nonNull, s)
					}
				}
			}
			if hasNull {
				schema["nullable"] = true
			}
			if len(nonNull) == 1 {
				schema["type"] = nonNull[0]
			} else if len(nonNull) == 0 {
				delete(schema, "type")
			}
		}
	}

	// Convert anyOf with null: {"anyOf": [<schema>, {"type": "null"}]} ->
	// <schema> inlined with "nullable": true
	if anyOf, ok := schema["anyOf"].([]any); ok {
		var nonNull []any
		hasNull := false
		for _, item := range anyOf {
			if m, ok := item.(map[string]any); ok {
				if t, ok := m["type"].(string); ok && t == "null" && len(m) == 1 {
					hasNull = true
					continue
				}
			}
			nonNull = append(nonNull, item)
		}
		if hasNull && len(nonNull) == 1 {
			// Inline the single remaining schema and mark nullable
			if remaining, ok := nonNull[0].(map[string]any); ok {
				delete(schema, "anyOf")
				maps.Copy(schema, remaining)
				schema["nullable"] = true
			}
		} else if hasNull {
			schema["anyOf"] = nonNull
			schema["nullable"] = true
		}
	}

	// OpenAPI 3.1 encodes exclusive bounds as numbers
	// ({"exclusiveMinimum": 0}); 3.0 expects a paired number + boolean
	// ({"minimum": 0, "exclusiveMinimum": true}). Rewrite so the
	// downgraded spec parses under the 3.0 schema kin-openapi uses. This
	// runs after the anyOf inlining above, which can copy a numeric
	// exclusive bound up from a collapsed anyOf branch onto this schema.
	rewriteExclusiveBound(schema, "exclusiveMinimum", "minimum")
	rewriteExclusiveBound(schema, "exclusiveMaximum", "maximum")

	// Rewrite $ref strings to strip V1 suffixes. This runs last because
	// anyOf inlining above can introduce a $ref onto this schema.
	if ref, ok := schema["$ref"].(string); ok {
		const prefix = "#/components/schemas/"
		if after, ok := strings.CutPrefix(ref, prefix); ok {
			if newName, ok := schemaRenames[after]; ok {
				schema["$ref"] = prefix + newName
			}
		}
	}

	// Harvest `discriminator.mapping` keyed by Go schema name (post V1
	// stripping), so postProcess can put wire values back where
	// oapi-codegen dropped them.
	if d, ok := schema["discriminator"].(map[string]any); ok {
		if m, ok := d["mapping"].(map[string]any); ok {
			const prefix = "#/components/schemas/"
			for key, refAny := range m {
				ref, _ := refAny.(string)
				name, ok := strings.CutPrefix(ref, prefix)
				if !ok {
					continue
				}
				if renamed, ok := schemaRenames[name]; ok {
					name = renamed
				}
				if existing, ok := discriminatorValues[name]; ok && existing != key {
					return fmt.Errorf("schema %q has conflicting discriminator keys %q and %q", name, existing, key)
				}
				discriminatorValues[name] = key
			}
		}
	}
	return nil
}

func rewriteExclusiveBound(schema map[string]any, exclusiveKey, boundKey string) {
	v, ok := schema[exclusiveKey]
	if !ok {
		return
	}
	if _, isBool := v.(bool); isBool {
		return
	}
	switch n := v.(type) {
	case float64:
		schema[boundKey] = n
	case json.Number:
		schema[boundKey] = n
	default:
		return
	}
	schema[exclusiveKey] = true
}

// preprocessConfigSchema transforms the truss config JSON Schema for
// go-jsonschema:
//
//  1. Renames Truss*-prefixed $defs to Model* (keys, $refs, root title).
//  2. Collapses anyOf [T, {type:null}] -> T, and type:["X","null"] -> type:"X".
//     go-jsonschema doesn't model nullability cleanly; the truss schema has
//     zero required+nullable fields, so the null branch carries no information
//     beyond what Go's pointer/omitempty already expresses for optional
//     fields. We assert that invariant first.
func preprocessConfigSchema(data []byte) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parsing config schema JSON: %w", err)
	}
	if err := assertNoRequiredNullable(doc, ""); err != nil {
		return nil, err
	}

	renames := buildConfigSchemaRenames(doc)
	renameConfigSchemaRefs(doc, renames)
	if defs := configSchemaDefs(doc); defs != nil {
		for old, renamed := range renames {
			if _, hasOld := defs[old]; hasOld {
				defs[renamed] = defs[old]
				delete(defs, old)
			}
		}
	}
	collapseNullables(doc)

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshaling config schema: %w", err)
	}
	return out, nil
}

func configSchemaDefs(doc map[string]any) map[string]any {
	defs, _ := doc["$defs"].(map[string]any)
	return defs
}

func buildConfigSchemaRenames(doc map[string]any) map[string]string {
	renames := map[string]string{}
	if defs := configSchemaDefs(doc); defs != nil {
		for name := range defs {
			if after, ok := strings.CutPrefix(name, "Truss"); ok {
				renames[name] = "Model" + after
			}
		}
	}
	if title, ok := doc["title"].(string); ok {
		if after, ok := strings.CutPrefix(title, "Truss"); ok {
			renames[title] = "Model" + after
		}
	}
	return renames
}

func renameConfigSchemaRefs(node any, renames map[string]string) {
	switch v := node.(type) {
	case map[string]any:
		if ref, ok := v["$ref"].(string); ok {
			const prefix = "#/$defs/"
			if after, ok := strings.CutPrefix(ref, prefix); ok {
				if renamed, ok := renames[after]; ok {
					v["$ref"] = prefix + renamed
				}
			}
		}
		// go-jsonschema with --struct-name-from-title uses nested `title`
		// fields as struct names, so we also rename Truss-prefixed titles
		// inside $defs (which mirror their parent key).
		if title, ok := v["title"].(string); ok {
			if renamed, ok := renames[title]; ok {
				v["title"] = renamed
			}
		}
		for _, child := range v {
			renameConfigSchemaRefs(child, renames)
		}
	case []any:
		for _, child := range v {
			renameConfigSchemaRefs(child, renames)
		}
	}
}

// assertNoRequiredNullable errors if any required property has a nullable
// shape. The collapse below would silently lose null semantics for required
// fields, since Go's pointer/omitempty would no longer be available.
func assertNoRequiredNullable(node any, path string) error {
	switch v := node.(type) {
	case map[string]any:
		if props, ok := v["properties"].(map[string]any); ok {
			if req, ok := v["required"].([]any); ok {
				for _, r := range req {
					name, _ := r.(string)
					prop, _ := props[name].(map[string]any)
					if prop != nil && propIsNullable(prop) {
						return fmt.Errorf("required+nullable property at %s.%s: preprocess would lose null semantics", path, name)
					}
				}
			}
		}
		for k, child := range v {
			if err := assertNoRequiredNullable(child, path+"."+k); err != nil {
				return err
			}
		}
	case []any:
		for i, child := range v {
			if err := assertNoRequiredNullable(child, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

func propIsNullable(prop map[string]any) bool {
	if anyOf, ok := prop["anyOf"].([]any); ok {
		for _, item := range anyOf {
			if m, ok := item.(map[string]any); ok {
				if t, ok := m["type"].(string); ok && t == "null" && len(m) == 1 {
					return true
				}
			}
		}
	}
	if t, ok := prop["type"].([]any); ok {
		for _, item := range t {
			if s, ok := item.(string); ok && s == "null" {
				return true
			}
		}
	}
	return false
}

func collapseNullables(node any) {
	collapseNullablesImpl(node, false)
}

// collapseNullablesImpl walks the schema collapsing anyOf-null and type-array-null.
// When skipTop is true, the top-level anyOf/type of the current node is left
// alone, but children are still processed. We set skipTop when descending into
// an `additionalProperties` schema, because nullability there carries meaning
// (e.g. `secrets` allows null placeholder values per the truss schema; collapsing
// would produce map[string]string which can't represent JSON null).
func collapseNullablesImpl(node any, skipTop bool) {
	switch v := node.(type) {
	case map[string]any:
		if !skipTop {
			if anyOf, ok := v["anyOf"].([]any); ok {
				var nonNull []any
				for _, item := range anyOf {
					if m, ok := item.(map[string]any); ok {
						if t, ok := m["type"].(string); ok && t == "null" && len(m) == 1 {
							continue
						}
					}
					nonNull = append(nonNull, item)
				}
				if len(nonNull) != len(anyOf) {
					if len(nonNull) == 1 {
						delete(v, "anyOf")
						if remaining, ok := nonNull[0].(map[string]any); ok {
							maps.Copy(v, remaining)
						}
					} else {
						v["anyOf"] = nonNull
					}
				}
			}
			if t, ok := v["type"].([]any); ok {
				var nonNull []string
				for _, item := range t {
					if s, ok := item.(string); ok && s != "null" {
						nonNull = append(nonNull, s)
					}
				}
				switch {
				case len(nonNull) == 1:
					v["type"] = nonNull[0]
				case len(nonNull) > 1:
					arr := make([]any, len(nonNull))
					for i, s := range nonNull {
						arr[i] = s
					}
					v["type"] = arr
				default:
					delete(v, "type")
				}
			}
		}
		for k, child := range v {
			collapseNullablesImpl(child, k == "additionalProperties")
		}
	case []any:
		for _, child := range v {
			collapseNullablesImpl(child, false)
		}
	}
}
