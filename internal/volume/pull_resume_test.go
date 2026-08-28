//go:build linux || darwin

package volume

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPullIdentityEnforcesPathAndSelectorBudgets(t *testing.T) {
	limits := portablePathLimits{maxPathBytes: 5, maxPathComponents: 3}
	if _, err := newPullIdentityWithLimits(
		testDigest(0x74),
		"/a/b",
		[]string{"a/b"},
		1,
		3,
		limits,
	); err != nil {
		t.Fatalf("identity boundary error = %v", err)
	}
	tests := []struct {
		name             string
		destination      string
		selectors        []string
		maxSelectors     int
		maxSelectorBytes uint64
	}{
		{
			name:             "destination bytes",
			destination:      "/a/bbb",
			selectors:        []string{"a"},
			maxSelectors:     1,
			maxSelectorBytes: 1,
		},
		{
			name:             "selector components",
			destination:      "/a",
			selectors:        []string{"a/b/c/d"},
			maxSelectors:     1,
			maxSelectorBytes: 16,
		},
		{
			name:             "selector count",
			destination:      "/a",
			selectors:        []string{"a", "b"},
			maxSelectors:     1,
			maxSelectorBytes: 2,
		},
		{
			name:             "selector bytes",
			destination:      "/a",
			selectors:        []string{"a/b"},
			maxSelectors:     1,
			maxSelectorBytes: 2,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := newPullIdentityWithLimits(
				testDigest(0x75),
				test.destination,
				test.selectors,
				test.maxSelectors,
				test.maxSelectorBytes,
				limits,
			); err == nil {
				t.Fatal("identity unexpectedly accepted")
			}
		})
	}
}

func TestPullResumeSupportsLargeDirectoryOnlySubsetIdentity(t *testing.T) {
	const selectorCount = 100
	selectors := make([]string, selectorCount)
	directories := make([]directoryEntry, selectorCount)
	for index := range selectorCount {
		selector := fmt.Sprintf("selector-%03d", index)
		for range 4 {
			selector += "/" + strings.Repeat("a", 246)
		}
		if len(selector) != 1_000 {
			t.Fatalf("selector length = %d, want 1000", len(selector))
		}
		selectors[index] = selector
		directories[index] = directoryEntry{Mode: 0o700, Path: selector}
	}
	manifest := validatedManifest{Directories: directories}
	if err := validateManifestStructure(
		manifest,
		defaultMaxFiles,
		defaultPortablePathLimits(),
	); err != nil {
		t.Fatalf("directory-only manifest validation error = %v", err)
	}
	selected, err := selectManifest(
		manifest,
		selectors,
		defaultMaxFiles,
		defaultPortablePathLimits(),
	)
	if err != nil {
		t.Fatalf("directory-only subset selection error = %v", err)
	}
	if len(selected.Files) != 0 || len(selected.Directories) == 0 {
		t.Fatalf("directory-only subset = %+v", selected)
	}
	expected, err := expectedPullChunks(
		t.Context(),
		pullPlan{directories: selected.Directories},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(expected) != 0 {
		t.Fatalf("directory-only expected chunks = %d, want 0", len(expected))
	}

	fixture := newPullFixture(t)
	parent := t.TempDir()
	identity, err := newPullIdentity(
		fixture.manifestDigest,
		filepath.Join(parent, "output"),
		selectors,
	)
	if err != nil {
		t.Fatalf("large pull identity error = %v", err)
	}
	checkpoint, err := initialPullCheckpoint(identity)
	if err != nil {
		t.Fatal(err)
	}
	checkpointBody, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if len(checkpointBody) <= pullStateOverheadBytes {
		t.Fatalf(
			"large identity checkpoint bytes = %d, want more than %d",
			len(checkpointBody),
			pullStateOverheadBytes,
		)
	}
	maxStateBytes, err := pullCheckpointSizeLimit(identity, expected)
	if err != nil {
		t.Fatal(err)
	}
	if uint64(len(checkpointBody)) > maxStateBytes {
		t.Fatalf(
			"checkpoint bytes = %d, computed limit %d",
			len(checkpointBody),
			maxStateBytes,
		)
	}

	resume, err := fixture.client.openPullResume(parent, identity, expected, false)
	if err != nil {
		t.Fatalf("initialize large directory-only resume: %v", err)
	}
	if resume.maxStateBytes != maxStateBytes {
		t.Fatalf(
			"resume state limit = %d, want %d",
			resume.maxStateBytes,
			maxStateBytes,
		)
	}
	identityInfo, err := os.Stat(filepath.Join(resume.stagingRoot, pullIdentityName))
	if err != nil {
		t.Fatal(err)
	}
	if identityInfo.Size() <= pullStateOverheadBytes {
		t.Fatalf(
			"encoded identity bytes = %d, want more than %d",
			identityInfo.Size(),
			pullStateOverheadBytes,
		)
	}
	if err := resume.close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := fixture.client.openPullResume(parent, identity, expected, false)
	if err != nil {
		t.Fatalf("reopen large directory-only resume: %v", err)
	}
	if reopened.maxStateBytes != maxStateBytes {
		t.Fatalf(
			"reopened state limit = %d, want %d",
			reopened.maxStateBytes,
			maxStateBytes,
		)
	}
	if err := reopened.close(); err != nil {
		t.Fatal(err)
	}

	journalParent := t.TempDir()
	journalIdentity, err := newPullIdentity(
		fixture.manifestDigest,
		filepath.Join(journalParent, "output"),
		selectors,
	)
	if err != nil {
		t.Fatal(err)
	}
	completed := completedPullChunk{
		Path: "selected-file", Length: 1, Digest: testDigest(0x76),
	}
	journalExpected := map[completedPullChunk]struct{}{completed: {}}
	journalResume, err := fixture.client.openPullResume(
		journalParent,
		journalIdentity,
		journalExpected,
		false,
	)
	if err != nil {
		t.Fatalf("initialize large-identity journal: %v", err)
	}
	if err := journalResume.markCompleted(completed); err != nil {
		t.Fatalf("append large-identity journal: %v", err)
	}
	if err := journalResume.close(); err != nil {
		t.Fatal(err)
	}
	journalReopened, err := fixture.client.openPullResume(
		journalParent,
		journalIdentity,
		journalExpected,
		false,
	)
	if err != nil {
		t.Fatalf("reopen large-identity journal: %v", err)
	}
	if !journalReopened.contains(completed) {
		t.Fatal("large-identity journal lost its completed chunk")
	}
	if err := journalReopened.close(); err != nil {
		t.Fatal(err)
	}
}

func TestPullCheckpointSizeLimitRejectsOverflowBudget(t *testing.T) {
	identity := pullIdentity{
		FormatVersion:  pullCheckpointVersion,
		ManifestDigest: testDigest(0x75),
		Destination:    "/output",
		Selectors:      []string{"selected"},
	}
	chunk := completedPullChunk{
		Path:   strings.Repeat("p", 128),
		Length: 1,
		Digest: testDigest(0x76),
	}
	expected := map[completedPullChunk]struct{}{chunk: {}}
	exact, err := pullCheckpointSizeLimit(identity, expected)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pullCheckpointSizeLimit(identity, expected, exact); err != nil {
		t.Fatalf("exact checkpoint limit error = %v", err)
	}
	if _, err := pullCheckpointSizeLimit(identity, expected, exact-1); err == nil ||
		!IsCode(err, ErrorPreconditionFailed) {
		t.Fatalf("checkpoint overflow error = %v, want %s", err, ErrorPreconditionFailed)
	}
	if _, overflow := addPullStateBytes(math.MaxUint64, 1, math.MaxUint64); !overflow {
		t.Fatal("pull state byte addition unexpectedly accepted uint64 overflow")
	}
}

func TestPullResumeRejectsMalformedOversizedState(t *testing.T) {
	type oversizedStateFixture struct {
		client         *Client
		parent         string
		identity       pullIdentity
		expected       map[completedPullChunk]struct{}
		checkpointPath string
		journalPath    string
		maxStateBytes  uint64
	}
	newFixture := func(t *testing.T) oversizedStateFixture {
		t.Helper()
		fixture := newPullFixture(t)
		parent := t.TempDir()
		identity, err := newPullIdentity(
			fixture.manifestDigest,
			filepath.Join(parent, "output"),
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		chunk := completedPullChunk{
			Path: "file", Length: 1, Digest: testDigest(0x77),
		}
		expected := map[completedPullChunk]struct{}{chunk: {}}
		resume, err := fixture.client.openPullResume(parent, identity, expected, false)
		if err != nil {
			t.Fatal(err)
		}
		result := oversizedStateFixture{
			client:         fixture.client,
			parent:         parent,
			identity:       identity,
			expected:       expected,
			checkpointPath: resume.checkpointPath,
			journalPath:    resume.journalPath,
			maxStateBytes:  resume.maxStateBytes,
		}
		if err := resume.close(); err != nil {
			t.Fatal(err)
		}
		if result.maxStateBytes >= uint64(^uint(0)>>1) {
			t.Fatalf("test state size is too large: %d", result.maxStateBytes)
		}
		return result
	}
	for _, stateName := range []string{"checkpoint", "journal"} {
		t.Run(stateName, func(t *testing.T) {
			fixture := newFixture(t)
			path := fixture.checkpointPath
			if stateName == "journal" {
				path = fixture.journalPath
			}
			malformed := bytes.Repeat([]byte("{"), int(fixture.maxStateBytes)+1)
			if err := os.WriteFile(path, malformed, 0o600); err != nil {
				t.Fatal(err)
			}
			reopened, err := fixture.client.openPullResume(
				fixture.parent,
				fixture.identity,
				fixture.expected,
				false,
			)
			if reopened != nil {
				_ = reopened.close()
			}
			if err == nil || !IsCode(err, ErrorIntegrity) {
				t.Fatalf(
					"oversized malformed %s error = %v, want %s",
					stateName,
					err,
					ErrorIntegrity,
				)
			}
		})
	}
}

func TestReusableResumeRehashUsesByteBudget(t *testing.T) {
	content := []byte("resume")
	rootPath := t.TempDir()
	if err := os.Chmod(rootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "file"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := openRootedDirectory(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.close()
	rootStat, err := root.currentStat()
	if err != nil {
		t.Fatal(err)
	}
	client := newTestVolumeClient(t)
	digest, err := client.digest(content)
	if err != nil {
		t.Fatal(err)
	}
	chunk := chunkEntry{Digest: digest, Length: uint64(len(content))}
	completed := completedPullChunk{
		Path: "file", Length: chunk.Length, Digest: chunk.Digest,
	}
	plan := pullPlan{
		files: []plannedFile{{
			mode: 0o600, path: "file", size: chunk.Length, chunks: []chunkEntry{chunk},
		}},
		totalSize:  chunk.Length,
		chunkCount: 1,
	}
	resume := &pullResume{completed: map[completedPullChunk]struct{}{completed: {}}}
	directories := map[string]rootedFileStat{"": rootStat}

	tooSmall := newByteGate(chunk.Length - 1)
	if _, err := client.reusableResumeBytes(
		t.Context(),
		root,
		directories,
		plan,
		resume,
		tooSmall,
	); err == nil || !IsCode(err, ErrorPreconditionFailed) {
		t.Fatalf("under-budget resume rehash error = %v, want %s", err, ErrorPreconditionFailed)
	}
	if got := tooSmall.bytesInUse(); got != 0 {
		t.Fatalf("failed resume rehash retained %d bytes", got)
	}

	exact := newByteGate(chunk.Length)
	reused, err := client.reusableResumeBytes(
		t.Context(),
		root,
		directories,
		plan,
		resume,
		exact,
	)
	if err != nil {
		t.Fatal(err)
	}
	if reused != chunk.Length || exact.bytesInUse() != 0 {
		t.Fatalf("resume rehash = reused %d bytes in use %d", reused, exact.bytesInUse())
	}
}

func TestCheckpointWireRejectsUnknownCompletedChunk(t *testing.T) {
	fixture := newPullFixture(t)
	parent := t.TempDir()
	identity, err := newPullIdentity(
		fixture.manifestDigest,
		filepath.Join(parent, "output"),
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	expectedChunk := completedPullChunk{Path: "expected", Length: 1, Digest: testDigest(1)}
	resume, err := fixture.client.openPullResume(
		parent,
		identity,
		map[completedPullChunk]struct{}{expectedChunk: {}},
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	checkpointPath := resume.checkpointPath
	if err := resume.close(); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(checkpointPath)
	if err != nil {
		t.Fatal(err)
	}
	var checkpoint pullCheckpoint
	if err := json.Unmarshal(body, &checkpoint); err != nil {
		t.Fatal(err)
	}
	checkpoint.CompletedChunks = []completedPullChunk{{
		Path: "unexpected", Length: 1, Digest: testDigest(2),
	}}
	body, err = json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(checkpointPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, err := fixture.client.openPullResume(
		parent,
		identity,
		map[completedPullChunk]struct{}{expectedChunk: {}},
		false,
	)
	if reopened != nil {
		reopened.close()
	}
	if err == nil || !IsCode(err, ErrorIntegrity) {
		t.Fatalf("unexpected checkpoint chunk error = %v, want %s", err, ErrorIntegrity)
	}
}

func TestCheckpointWireRejectsIdentityMismatch(t *testing.T) {
	fixture := newPullFixture(t)
	parent := t.TempDir()
	identity, err := newPullIdentity(
		fixture.manifestDigest,
		filepath.Join(parent, "output"),
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	expectedChunk := completedPullChunk{Path: "expected", Length: 1, Digest: testDigest(1)}
	expected := map[completedPullChunk]struct{}{expectedChunk: {}}
	resume, err := fixture.client.openPullResume(parent, identity, expected, false)
	if err != nil {
		t.Fatal(err)
	}
	checkpointPath := resume.checkpointPath
	if err := resume.close(); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(checkpointPath)
	if err != nil {
		t.Fatal(err)
	}
	var checkpoint pullCheckpoint
	if err := json.Unmarshal(body, &checkpoint); err != nil {
		t.Fatal(err)
	}
	checkpoint.Identity.Destination += "-different"
	body, err = json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(checkpointPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, err := fixture.client.openPullResume(parent, identity, expected, false)
	if reopened != nil {
		reopened.close()
	}
	if err == nil || !IsCode(err, ErrorIntegrity) {
		t.Fatalf("checkpoint identity error = %v, want %s", err, ErrorIntegrity)
	}
}
