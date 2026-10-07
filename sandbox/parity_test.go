package sandbox

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/basetenlabs/baseten-go/client/managementapi"
	"github.com/basetenlabs/baseten-go/client/sandboxapi"
)

// A field the server adds to a generated type but that no hand-written type
// carries is a silent omission. These tests make it a failing test instead:
// every pair's field names must match, case-insensitively since the generated
// names keep the wire's initialisms as written (ExternalId, Pid), after the
// listed renames, and every deliberately one-sided field is listed with its
// reason. Field types may differ; the conversions and their tests own those.
type parityPair struct {
	name      string
	ours      any
	generated any
	// renamed maps our field name to the generated one it converts to or
	// from.
	renamed map[string]string
	// oursOnly and generatedOnly name the deliberately one-sided fields, with
	// the reason.
	oursOnly      map[string]string
	generatedOnly map[string]string
}

func TestTypeParity(t *testing.T) {
	ttlPolicy := map[string]string{"After": "Value"}
	ttlOnly := map[string]string{"At": "the time of DATE policies"}
	for _, pair := range []parityPair{
		{name: "Info", ours: Info{}, generated: managementapi.Sandbox{}},
		{
			name: "EnvValue", ours: EnvValue{}, generated: managementapi.SandboxEnv{},
			renamed:       map[string]string{"NonSecret": "Secret"},
			generatedOnly: map[string]string{"Name": "the key of Info.Envs"},
		},
		{name: "Lifecycle", ours: Lifecycle{}, generated: managementapi.SandboxLifecycle{}},
		{
			name: "ExpirationPolicy/TTLIdle", ours: ExpirationPolicy{}, generated: managementapi.SandboxTTLIdleExpirationPolicy{},
			renamed: ttlPolicy, oursOnly: ttlOnly,
		},
		{
			name: "ExpirationPolicy/TTLMaxAge", ours: ExpirationPolicy{}, generated: managementapi.SandboxTTLMaxAgeExpirationPolicy{},
			renamed: ttlPolicy, oursOnly: ttlOnly,
		},
		{
			name: "ExpirationPolicy/Date", ours: ExpirationPolicy{}, generated: managementapi.SandboxDateExpirationPolicy{},
			renamed:  map[string]string{"At": "Value"},
			oursOnly: map[string]string{"After": "the duration of TTL policies"},
		},
		{name: "Port", ours: Port{}, generated: managementapi.SandboxPort{}},
		{name: "Network", ours: Network{}, generated: managementapi.SandboxNetwork{}},
		{name: "NetworkProxy", ours: NetworkProxy{}, generated: managementapi.SandboxProxyConfig{}},
		{name: "NetworkProxyRoute", ours: NetworkProxyRoute{}, generated: managementapi.SandboxProxyTarget{}},
		{name: "ProcessInfo", ours: ProcessInfo{}, generated: sandboxapi.ProcessResponse{}},
		{name: "ProcessLogs", ours: ProcessLogs{}, generated: sandboxapi.ProcessLogs{}},
		{
			name: "ImageInfo", ours: ImageInfo{}, generated: managementapi.SandboxImage{},
			renamed:       map[string]string{"SizeBytes": "Size"},
			generatedOnly: map[string]string{"Tags": "always empty; ImageClient.ListTags lists them"},
		},
		{
			name: "ImageInfo/Summary", ours: ImageInfo{}, generated: managementapi.SandboxImageSummary{},
			renamed: map[string]string{"SizeBytes": "Size"},
		},
		{
			name: "ImageTagInfo", ours: ImageTagInfo{}, generated: managementapi.SandboxImageTag{},
			renamed: map[string]string{"SizeBytes": "Size"},
		},
		{
			name: "ImageLogLine", ours: ImageLogLine{}, generated: managementapi.SandboxImageBuildLog{},
			renamed: map[string]string{"Text": "Message"},
		},
		{name: "ImageCleanupResult", ours: ImageCleanupResult{}, generated: managementapi.CleanupSandboxImagesResponse{}},
		{
			name: "ImageLibraryInfo", ours: ImageLibraryInfo{}, generated: managementapi.SandboxLibraryImage{},
			oursOnly: map[string]string{
				"CreationExtraArgs": "flattened from CreationOptions",
				"CreationVolumes":   "flattened from CreationOptions",
			},
			generatedOnly: map[string]string{
				"CreationOptions": "flattened into CreationExtraArgs and CreationVolumes",
				"Hidden":          "always false in listings",
				"ComingSoon":      "always false in listings",
			},
		},
		{name: "ImageLibraryVolume", ours: ImageLibraryVolume{}, generated: managementapi.SandboxLibraryImageVolume{}},
	} {
		t.Run(pair.name, func(t *testing.T) {
			generated := map[string]bool{}
			for _, name := range exportedFields(pair.generated) {
				generated[strings.ToLower(name)] = true
			}
			for _, name := range exportedFields(pair.ours) {
				if _, ok := pair.oursOnly[name]; ok {
					continue
				}
				counterpart := name
				if renamed, ok := pair.renamed[name]; ok {
					counterpart = renamed
				}
				if !generated[strings.ToLower(counterpart)] {
					t.Errorf("%s.%s has no generated counterpart %s", pair.name, name, counterpart)
				}
				delete(generated, strings.ToLower(counterpart))
			}
			for name := range pair.generatedOnly {
				delete(generated, strings.ToLower(name))
			}
			for name := range generated {
				t.Errorf("generated field %q of %s is not carried by our type; add it or list it as generated-only", name, pair.name)
			}
		})
	}
}

// ImageLibraryInfo flattens the generated creation options into its own
// Creation fields, so a field added to them has no twin to compare against.
func TestImageLibraryCreationOptionsFlattened(t *testing.T) {
	fields := exportedFields(managementapi.SandboxLibraryImageCreationOptions{})
	if !slices.Equal(fields, []string{"ExtraArgs", "Volumes"}) {
		t.Errorf("generated creation options have fields %v; flatten the new ones into ImageLibraryInfo", fields)
	}
}

func exportedFields(value any) []string {
	var names []string
	for field := range reflect.TypeOf(value).Fields() {
		if field.IsExported() {
			names = append(names, field.Name)
		}
	}
	slices.Sort(names)
	return names
}
