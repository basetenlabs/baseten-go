package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"strings"
)

// preprocessSpec transforms an OpenAPI 3.1 spec to 3.0-compatible form so
// that oapi-codegen can process it, and cleans up schema names. The input and
// output are JSON bytes.
func preprocessSpec(data []byte) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parsing spec JSON: %w", err)
	}

	if v, ok := doc["openapi"].(string); ok && strings.HasPrefix(v, "3.1") {
		doc["openapi"] = "3.0.3"
	}

	// Build V1-suffix rename map from schema names before walking.
	schemaRenames := buildSchemaRenames(doc)

	preprocessNode(doc, schemaRenames)

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
	return out, nil
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

func preprocessNode(node any, schemaRenames map[string]string) {
	switch v := node.(type) {
	case map[string]any:
		preprocessSchema(v, schemaRenames)
		for _, child := range v {
			preprocessNode(child, schemaRenames)
		}
	case []any:
		for _, child := range v {
			preprocessNode(child, schemaRenames)
		}
	}
}

// preprocessSchema handles nullable patterns and $ref renames in a single
// schema object.
func preprocessSchema(schema map[string]any, schemaRenames map[string]string) {
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
}
