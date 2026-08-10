package lifecycle

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func legacyTriple(role, digest string) map[string]string {
	return map[string]string{
		LegacyManagedLabelKey: ManagedLabelValue,
		LegacyRoleLabelKey:    role,
		LegacySpecLabelKey:    digest,
	}
}

func dualTriple(role, digest string) map[string]string {
	return map[string]string{
		LegacyManagedLabelKey:  ManagedLabelValue,
		LegacyRoleLabelKey:     role,
		LegacySpecLabelKey:     digest,
		CurrentManagedLabelKey: ManagedLabelValue,
		CurrentRoleLabelKey:    role,
		CurrentSpecLabelKey:    digest,
	}
}

func currentTriple(role, digest string) map[string]string {
	return map[string]string{
		CurrentManagedLabelKey: ManagedLabelValue,
		CurrentRoleLabelKey:    role,
		CurrentSpecLabelKey:    digest,
	}
}

func TestPlanOwnershipLabelsPrepareRaisesLegacyOnlyToDual(t *testing.T) {
	planned, err := planOwnershipLabels(
		legacyTriple("postgres", "abc"), MetadataStagePrepare,
	)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if !reflect.DeepEqual(planned, dualTriple("postgres", "abc")) {
		t.Fatalf("prepare produced %#v", planned)
	}
}

func TestPlanOwnershipLabelsRetireDropsLegacyFromDual(t *testing.T) {
	planned, err := planOwnershipLabels(
		dualTriple("postgres", "abc"), MetadataStageRetire,
	)
	if err != nil {
		t.Fatalf("retire: %v", err)
	}
	if !reflect.DeepEqual(planned, currentTriple("postgres", "abc")) {
		t.Fatalf("retire produced %#v", planned)
	}
}

func TestPlanOwnershipLabelsIsIdempotent(t *testing.T) {
	for name, testCase := range map[string]struct {
		labels map[string]string
		stage  MetadataStage
	}{
		"prepare on dual":         {dualTriple("r", "d"), MetadataStagePrepare},
		"prepare on current only": {currentTriple("r", "d"), MetadataStagePrepare},
		"retire on current only":  {currentTriple("r", "d"), MetadataStageRetire},
	} {
		t.Run(name, func(t *testing.T) {
			planned, err := planOwnershipLabels(testCase.labels, testCase.stage)
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			if !reflect.DeepEqual(planned, testCase.labels) {
				t.Fatalf(
					"expected no change; got %#v from %#v",
					planned, testCase.labels,
				)
			}
		})
	}
}

func TestPlanOwnershipLabelsRefusesRetireBeforePrepare(t *testing.T) {
	// Retiring a legacy-only resource would erase its only ownership marker and
	// leave it unowned, which is precisely the orphaning Issue #123 exists to
	// avoid.
	if _, err := planOwnershipLabels(
		legacyTriple("postgres", "abc"), MetadataStageRetire,
	); err == nil {
		t.Fatal("expected retire on a legacy-only resource to be refused")
	}
}

func TestPlanOwnershipLabelsRefusesConflictingPairs(t *testing.T) {
	conflicting := map[string]string{
		LegacyManagedLabelKey:  ManagedLabelValue,
		CurrentManagedLabelKey: "v2",
	}
	for _, stage := range []MetadataStage{
		MetadataStagePrepare, MetadataStageRetire,
	} {
		if _, err := planOwnershipLabels(conflicting, stage); err == nil {
			t.Fatalf("expected %s to refuse a conflicting pair", stage)
		} else if !errors.Is(err, ErrConflictingLabels) {
			t.Fatalf("%s reported %v, want a conflicting-label error", stage, err)
		}
	}
}

func TestPlanOwnershipLabelsRefusesPartialPairs(t *testing.T) {
	// A pair whose two sides disagree on presence of a value is half written.
	// Phase 3A treats it the same way every other reader does: it stops.
	halfWritten := map[string]string{
		LegacyManagedLabelKey:  ManagedLabelValue,
		CurrentManagedLabelKey: "",
	}
	for _, stage := range []MetadataStage{
		MetadataStagePrepare, MetadataStageRetire,
	} {
		if _, err := planOwnershipLabels(halfWritten, stage); err == nil {
			t.Fatalf("expected %s to refuse a half written pair", stage)
		}
	}
}

func TestPlanOwnershipLabelsRefusesUnmanagedAndUnlabelled(t *testing.T) {
	for name, labels := range map[string]map[string]string{
		"unmanaged":     {LegacyManagedLabelKey: "v2"},
		"missing-label": {},
		"foreign only":  {"com.example.thing": "1"},
	} {
		for _, stage := range []MetadataStage{
			MetadataStagePrepare, MetadataStageRetire,
		} {
			if _, err := planOwnershipLabels(labels, stage); err == nil {
				t.Fatalf(
					"expected %s to refuse a %s resource", stage, name,
				)
			}
		}
	}
}

func TestPlanOwnershipLabelsNeverTouchesForeignLabels(t *testing.T) {
	labels := dualTriple("postgres", "abc")
	labels["com.example.owner"] = "someone-else"
	labels["com.apple.thing"] = "1"
	planned, err := planOwnershipLabels(labels, MetadataStageRetire)
	if err != nil {
		t.Fatalf("retire: %v", err)
	}
	if planned["com.example.owner"] != "someone-else" ||
		planned["com.apple.thing"] != "1" {
		t.Fatalf("foreign labels were disturbed: %#v", planned)
	}
	// Only the six ownership keys may differ between before and after.
	for key := range planned {
		if _, owned := ownershipKeySet()[key]; owned {
			continue
		}
		if planned[key] != labels[key] {
			t.Fatalf("non ownership label %q changed", key)
		}
	}
}

func TestChangedKeysAreOnlyOwnershipKeys(t *testing.T) {
	before := dualTriple("postgres", "abc")
	after, err := planOwnershipLabels(before, MetadataStageRetire)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range changedLabelKeys(before, after) {
		if _, owned := ownershipKeySet()[key]; !owned {
			t.Fatalf("stage changed non ownership key %q", key)
		}
	}
}

// --- target resolution -------------------------------------------------

func seedAppRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(
		filepath.Join(root, "volumes", "vol-a", "entity.json"),
		`{"name":"vol-a","labels":{"`+LegacyManagedLabelKey+`":"v1"}}`,
	)
	write(filepath.Join(root, "volumes", "vol-a", "volume.img"), "")
	write(
		filepath.Join(root, "networks", "net-a", "entity.json"),
		`{"name":"net-a","labels":{"`+LegacyManagedLabelKey+`":"v1"}}`,
	)
	write(filepath.Join(root, "networks", "net-a", "service.plist"), "")
	// A container that was started carries both documents; one that was only
	// created carries just the runtime configuration.
	write(
		filepath.Join(root, "containers", "ctr-started", "config.json"),
		`{"id":"ctr-started","labels":{"`+LegacyManagedLabelKey+`":"v1"}}`,
	)
	write(
		filepath.Join(
			root, "containers", "ctr-started", "runtime-configuration.json",
		),
		`{"containerConfiguration":{"id":"ctr-started","labels":{"`+
			LegacyManagedLabelKey+`":"v1"}}}`,
	)
	write(
		filepath.Join(
			root, "containers", "ctr-created", "runtime-configuration.json",
		),
		`{"containerConfiguration":{"id":"ctr-created","labels":{"`+
			LegacyManagedLabelKey+`":"v1"}}}`,
	)
	return root
}

func TestResolveMetadataTargetFindsEveryPresentDocument(t *testing.T) {
	root := seedAppRoot(t)
	target, err := resolveMetadataTarget(root, MetadataKindContainer, "ctr-started")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(target.Files) != 2 {
		t.Fatalf("expected both container documents, got %#v", target.Files)
	}
	created, err := resolveMetadataTarget(root, MetadataKindContainer, "ctr-created")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(created.Files) != 1 {
		t.Fatalf("expected one document, got %#v", created.Files)
	}
}

func TestResolveMetadataTargetRejectsAbsentResource(t *testing.T) {
	root := seedAppRoot(t)
	if _, err := resolveMetadataTarget(
		root, MetadataKindVolume, "does-not-exist",
	); err == nil {
		t.Fatal("expected an absent target to be refused")
	}
}

func TestResolveMetadataTargetRejectsIdentityEscapingTheAppRoot(t *testing.T) {
	root := seedAppRoot(t)
	for _, identity := range []string{
		"../escape", "a/b", ".", "", "..",
	} {
		if _, err := resolveMetadataTarget(
			root, MetadataKindVolume, identity,
		); err == nil {
			t.Fatalf("expected identity %q to be refused", identity)
		}
	}
}

func TestResolveMetadataTargetRejectsUnknownFilesInTheResourceDirectory(t *testing.T) {
	root := seedAppRoot(t)
	if err := os.WriteFile(
		filepath.Join(root, "volumes", "vol-a", "surprise.json"),
		[]byte("{}"), 0o644,
	); err != nil {
		t.Fatal(err)
	}
	_, err := resolveMetadataTarget(root, MetadataKindVolume, "vol-a")
	if err == nil || !strings.Contains(err.Error(), "surprise.json") {
		t.Fatalf("expected an unknown file to be refused, got %v", err)
	}
}

func TestReadMetadataTargetRequiresEveryDocumentToAgree(t *testing.T) {
	root := seedAppRoot(t)
	// Diverge the two container documents: only one carries the current pair.
	path := filepath.Join(
		root, "containers", "ctr-started", "runtime-configuration.json",
	)
	if err := os.WriteFile(path, []byte(
		`{"containerConfiguration":{"id":"ctr-started","labels":{"`+
			LegacyManagedLabelKey+`":"v1","`+CurrentManagedLabelKey+`":"v1"}}}`,
	), 0o644); err != nil {
		t.Fatal(err)
	}
	target, err := resolveMetadataTarget(root, MetadataKindContainer, "ctr-started")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// A container whose two on-disk documents disagree is exactly the state a
	// half-completed migration leaves behind, and continuing from it would
	// migrate one document and silently skip the other.
	if _, err := readMetadataTarget(target); err == nil {
		t.Fatal("expected disagreeing documents to be refused")
	}
}

func TestReadMetadataTargetReturnsAgreedLabels(t *testing.T) {
	root := seedAppRoot(t)
	target, err := resolveMetadataTarget(root, MetadataKindContainer, "ctr-started")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	state, err := readMetadataTarget(target)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if state.Labels[LegacyManagedLabelKey] != "v1" {
		t.Fatalf("unexpected labels: %#v", state.Labels)
	}
	if len(state.Files) != 2 {
		t.Fatalf("expected two verified documents, got %d", len(state.Files))
	}
}

// --- staged application ------------------------------------------------

func TestApplyMetadataStageWritesEveryDocumentAndRollsBackExactly(t *testing.T) {
	root := seedAppRoot(t)
	backups := filepath.Join(t.TempDir(), "backup")
	target, err := resolveMetadataTarget(root, MetadataKindContainer, "ctr-started")
	if err != nil {
		t.Fatal(err)
	}
	originals := map[string][]byte{}
	for _, file := range target.Files {
		data, err := os.ReadFile(file.Path)
		if err != nil {
			t.Fatal(err)
		}
		originals[file.Path] = data
	}
	applied, err := applyMetadataStage(target, MetadataStagePrepare, backups)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(applied.Files) != 2 {
		t.Fatalf("expected both documents rewritten, got %d", len(applied.Files))
	}
	for _, file := range target.Files {
		data, err := os.ReadFile(file.Path)
		if err != nil {
			t.Fatal(err)
		}
		labels, err := readLabelsAtPath(data, file.LabelPath)
		if err != nil {
			t.Fatal(err)
		}
		if labels[CurrentManagedLabelKey] != "v1" {
			t.Fatalf("%s did not gain the current label: %s", file.Path, data)
		}
	}
	if err := rollbackMetadataApplication(applied); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	for path, want := range originals {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Fatalf(
				"rollback did not restore %s byte for byte:\n want %s\n got  %s",
				path, want, got,
			)
		}
	}
}

func TestApplyMetadataStageIsAtomicAcrossDocuments(t *testing.T) {
	root := seedAppRoot(t)
	backups := filepath.Join(t.TempDir(), "backup")
	target, err := resolveMetadataTarget(root, MetadataKindContainer, "ctr-started")
	if err != nil {
		t.Fatal(err)
	}
	// Make the second document unwritable so the stage fails partway.
	second := target.Files[1].Path
	original, err := os.ReadFile(target.Files[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(second), 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(filepath.Dir(second), 0o755)
	_, err = applyMetadataStage(target, MetadataStagePrepare, backups)
	os.Chmod(filepath.Dir(second), 0o755)
	if err == nil {
		t.Fatal("expected the stage to fail")
	}
	// A resource whose documents disagree is worse than one that was never
	// touched, so a failure has to unwind what it already wrote.
	restored, readErr := os.ReadFile(target.Files[0].Path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(restored) != string(original) {
		t.Fatalf(
			"first document was left migrated after a failed stage:\n%s",
			restored,
		)
	}
}

func TestApplyMetadataStageRefusesToReuseABackupDirectory(t *testing.T) {
	root := seedAppRoot(t)
	backups := filepath.Join(t.TempDir(), "backup")
	target, err := resolveMetadataTarget(root, MetadataKindVolume, "vol-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applyMetadataStage(
		target, MetadataStagePrepare, backups,
	); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	// Re-running into the same backup directory would overwrite the only record
	// of the original bytes.
	if _, err := applyMetadataStage(
		target, MetadataStageRetire, backups,
	); err == nil {
		t.Fatal("expected the second run to refuse the used backup directory")
	}
}
