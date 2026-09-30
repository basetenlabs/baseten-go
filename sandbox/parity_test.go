package sandbox_test

import (
	"reflect"
	"testing"

	"github.com/basetenlabs/baseten-go/client/managementapi"
	"github.com/basetenlabs/baseten-go/client/sandboxapi"
	"github.com/basetenlabs/baseten-go/sandbox"
)

// The curated types are hand-maintained twins of the generated records, so a
// field added to a generated record but not to its twin is a silent omission.
// These tests make a drifted twin a red test: every generated field must be
// either translated under a documented rename or named in the exception list
// with its reason, and every curated field must come from somewhere.
//
// Field types may deliberately differ (string timestamps versus time.Time);
// the conversion in info.go owns those.

// sandboxRecordRenames maps generated Sandbox fields to their curated
// SandboxInfo names.
var sandboxRecordRenames = map[string]string{
	"ExternalId": "ExternalID",
	"ExpiresIn":  "ExpiresInSeconds",
	"Url":        "URL",
}

// sandboxRecordUntranslated names generated Sandbox fields the curated record
// deliberately does not carry, with the reason.
var sandboxRecordUntranslated = map[string]string{
	"Lifecycle": "raw-client only until a CLI command needs it",
	"Network":   "immutable after creation, raw-client only",
	"Ports":     "raw-client only",
}

// processRecordUntranslated names generated ProcessResponse fields the curated
// ProcessInfo deliberately does not carry, with the reason.
var processRecordUntranslated = map[string]string{
	"KeepAlive":        "process configuration, echoed back, not state",
	"MaxRestarts":      "process configuration, echoed back",
	"RestartCount":     "restart bookkeeping for named processes",
	"RestartOnFailure": "process configuration, echoed back",
	"Stdin":            "whether a stdin pipe was opened, not state",
}

func TestSandboxInfoCoversGeneratedRecord(t *testing.T) {
	collect := func(value any) map[string]bool {
		fields := map[string]bool{}
		structType := reflect.TypeOf(value)
		for i := range structType.NumField() {
			fields[structType.Field(i).Name] = true
		}
		return fields
	}
	generated := collect(managementapi.Sandbox{})
	collated := collect(sandbox.SandboxInfo{})

	renames := map[string]bool{}
	for generatedName, collatedName := range sandboxRecordRenames {
		if !collated[collatedName] {
			t.Errorf("renamed field %s has no curated twin", collatedName)
		}
		renames[generatedName] = true
	}
	for name := range generated {
		if renames[name] || sandboxRecordUntranslated[name] != "" {
			continue
		}
		if !collated[name] {
			t.Errorf("generated field %s has neither a curated twin nor an exception", name)
		}
	}
	for name := range collated {
		_, translated := generated[name]
		_, renamed := sandboxRecordRenames[name]
		if !translated && !renamed && !collatedFromRenamed(name) {
			t.Errorf("curated field %s translates no generated field", name)
		}
	}
}

func collatedFromRenamed(collatedName string) bool {
	for _, renamed := range sandboxRecordRenames {
		if renamed == collatedName {
			return true
		}
	}
	return false
}

// processRecordRenames maps generated ProcessResponse fields to their
// curated ProcessInfo names.
var processRecordRenames = map[string]string{
	"Pid": "PID",
}

func TestProcessInfoCoversGeneratedRecord(t *testing.T) {
	collect := func(value any) map[string]bool {
		fields := map[string]bool{}
		structType := reflect.TypeOf(value)
		for i := range structType.NumField() {
			fields[structType.Field(i).Name] = true
		}
		return fields
	}
	generated := collect(sandboxapi.ProcessResponse{})
	collated := collect(sandbox.ProcessInfo{})

	renames := map[string]bool{}
	for generatedName, collatedName := range processRecordRenames {
		if !collated[collatedName] {
			t.Errorf("renamed field %s has no curated twin", collatedName)
		}
		renames[generatedName] = true
	}
	for name := range generated {
		if renames[name] || processRecordUntranslated[name] != "" {
			continue
		}
		if !collated[name] {
			t.Errorf("generated field %s has neither a curated twin nor an exception", name)
		}
	}
	for name := range collated {
		_, translated := generated[name]
		_, renamed := processRecordRenames[name]
		if !translated && !renamed && !processRenameTarget(name) {
			t.Errorf("curated field %s translates no generated field", name)
		}
	}
}

func processRenameTarget(collatedName string) bool {
	for _, renamed := range processRecordRenames {
		if renamed == collatedName {
			return true
		}
	}
	return false
}

func TestExceptionListsNameRealFields(t *testing.T) {
	for name := range sandboxRecordUntranslated {
		if _, present := reflect.TypeOf(managementapi.Sandbox{}).FieldByName(name); !present {
			t.Errorf("sandbox exception %q names no generated field", name)
		}
	}
	for name := range processRecordUntranslated {
		if _, present := reflect.TypeOf(sandboxapi.ProcessResponse{}).FieldByName(name); !present {
			t.Errorf("process exception %q names no generated field", name)
		}
	}
}

// imageRecordUntranslated names generated Image fields the curated record
// deliberately does not carry, with the reason.
var imageRecordUntranslated = map[string]string{
	"Tags": "list and get return only a tag count; tags have their own endpoint",
}

func TestImageInfoCoversGeneratedRecord(t *testing.T) {
	collect := func(value any) map[string]bool {
		fields := map[string]bool{}
		structType := reflect.TypeOf(value)
		for i := range structType.NumField() {
			fields[structType.Field(i).Name] = true
		}
		return fields
	}
	generated := collect(managementapi.Image{})
	collated := collect(sandbox.ImageInfo{})

	renames := map[string]string{"Size": "SizeBytes", "LastDeployedAt": "LastDeployedAt"}
	for name := range generated {
		if renames[name] != "" || imageRecordUntranslated[name] != "" {
			continue
		}
		if !collated[name] && renames[name] == "" {
			t.Errorf("generated field %s has neither a curated twin nor an exception", name)
		}
	}
	for name := range collated {
		_, translated := generated[name]
		if !translated && name != "SizeBytes" {
			t.Errorf("curated field %s translates no generated field", name)
		}
	}
	for name := range imageRecordUntranslated {
		if _, present := reflect.TypeOf(managementapi.Image{}).FieldByName(name); !present {
			t.Errorf("image exception %q names no generated field", name)
		}
	}
}
