package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestScopedManagementImport(t *testing.T) {
	baseline := []byte(`{"paths":{"/v1/models":{"get":{"description":"baseline"}}},"components":{"schemas":{"Existing":{"type":"string"}}}}`)
	snapshot := []byte(`{"paths":{"/v1/models":{"get":{"description":"unrelated drift"}},"/v1/sandboxes/instances":{"get":{"responses":{"200":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/Sandbox"}}}}}}},"/v1/token":{"post":{}}},"components":{"schemas":{"Existing":{"type":"number"},"Sandbox":{"type":"object","properties":{"nested":{"$ref":"#/components/schemas/Nested"}}},"Nested":{"type":"string"},"Unused":{"type":"string"}}}}`)
	merged, err := importSandboxManagement(baseline, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err = json.Unmarshal(merged, &doc); err != nil {
		t.Fatal(err)
	}
	if mapNode(mapNode(mapNode(doc["paths"])["/v1/models"])["get"])["description"] != "baseline" {
		t.Fatal("unrelated route changed")
	}
	schemas := componentSchemas(doc)
	if mapNode(schemas["Existing"])["type"] != "string" || schemas["Unused"] != nil {
		t.Fatal("unrelated component changed/imported")
	}
	if schemas["Sandbox"] == nil || schemas["Nested"] == nil {
		t.Fatal("transitive dependency omitted")
	}
	again, err := importSandboxManagement(merged, snapshot)
	if err != nil || string(again) != string(merged) {
		t.Fatalf("import not idempotent: %v", err)
	}
}
func TestScopedImportRejectsConflictsAndUnknownReferences(t *testing.T) {
	baseline := []byte(`{"paths":{},"components":{"schemas":{"Shared":{"type":"string"}}}}`)
	for _, reference := range []string{"#/components/schemas/Shared", "#/components/schemas/Missing", "https://example.com/spec.json"} {
		snapshot := []byte(`{"paths":{"/v1/sandboxes/instances":{"get":{"schema":{"$ref":"` + reference + `"}}}},"components":{"schemas":{"Shared":{"type":"number"}}}}`)
		if _, err := importSandboxManagement(baseline, snapshot); err == nil {
			t.Errorf("accepted %s", reference)
		} else if strings.Contains(reference, "Shared") && !strings.Contains(err.Error(), "shared component") {
			t.Fatal(err)
		}
	}
}

func TestScopedImportUpdatesOnlyExclusiveSandboxComponents(t *testing.T) {
	baseline := []byte(`{"paths":{"/v1/sandboxes/instances":{"get":{"schema":{"$ref":"#/components/schemas/Shared"}}}},"components":{"schemas":{"Shared":{"type":"string"}}}}`)
	snapshot := []byte(`{"paths":{"/v1/sandboxes/instances":{"get":{"schema":{"$ref":"#/components/schemas/Shared"}}}},"components":{"schemas":{"Shared":{"type":"number"}}}}`)
	updated, err := importSandboxManagement(baseline, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	json.Unmarshal(updated, &doc)
	if mapNode(componentSchemas(doc)["Shared"])["type"] != "number" {
		t.Fatal("exclusive sandbox schema not updated")
	}
	json.Unmarshal(baseline, &doc)
	mapNode(doc["paths"])["/v1/models"] = map[string]any{"get": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/Shared"}}}
	shared, _ := json.Marshal(doc)
	if _, err := importSandboxManagement(shared, snapshot); err == nil {
		t.Fatal("shared schema conflict accepted")
	}
}
