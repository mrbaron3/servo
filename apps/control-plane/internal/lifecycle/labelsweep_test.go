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
	applied, err := applyMetadataStage(target, MetadataStagePrepare, backups, root)
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
	target, err := resolveMetadataTarget(root, MetadataKindContainer, "ctr-started")
	if err != nil {
		t.Fatal(err)
	}
	if len(target.Files) != 2 {
		t.Fatalf("expected two documents, got %d", len(target.Files))
	}
	originals := make(map[string][]byte, len(target.Files))
	for _, file := range target.Files {
		data, err := os.ReadFile(file.Path)
		if err != nil {
			t.Fatal(err)
		}
		originals[file.Path] = data
	}
	// The second document's backup destination is pre-occupied. Both container
	// documents live in the same directory, so making that directory unwritable
	// — the obvious way to fail the second write — fails the FIRST one too, and
	// the test would then pass without the unwind path ever running. Blocking
	// only the second document's O_EXCL backup fails exactly one write, after
	// the other has already succeeded.
	backups := filepath.Join(t.TempDir(), "backups")
	second := filepath.Base(target.Files[1].Path)
	occupied := filepath.Join(backups, "container", "ctr-started", second)
	if err := os.MkdirAll(filepath.Dir(occupied), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(occupied, []byte("earlier run"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := applyMetadataStage(
		target, MetadataStagePrepare, backups, root,
	); err == nil {
		t.Fatal("expected the stage to fail on the second document")
	}
	// A resource whose documents disagree is worse than one that was never
	// touched, so the failure has to unwind what it already wrote.
	for path, want := range originals {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Fatalf(
				"a failed stage left %s migrated:\n want %s\n got  %s",
				filepath.Base(path), want, got,
			)
		}
	}
}

// TestApplyMetadataStageUnwindActuallyRuns proves the previous test exercises
// the unwind rather than trivially observing an untouched first document.
func TestApplyMetadataStageUnwindActuallyRuns(t *testing.T) {
	root := seedAppRoot(t)
	target, err := resolveMetadataTarget(root, MetadataKindContainer, "ctr-started")
	if err != nil {
		t.Fatal(err)
	}
	backups := filepath.Join(t.TempDir(), "backups")
	second := filepath.Base(target.Files[1].Path)
	occupied := filepath.Join(backups, "container", "ctr-started", second)
	if err := os.MkdirAll(filepath.Dir(occupied), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(occupied, []byte("earlier run"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := applyMetadataStage(
		target, MetadataStagePrepare, backups, root,
	); err == nil {
		t.Fatal("expected failure")
	}
	// The first document's backup exists, which is only true if that document
	// was actually written before the second one failed.
	first := filepath.Base(target.Files[0].Path)
	if _, err := os.Stat(
		filepath.Join(backups, "container", "ctr-started", first),
	); err != nil {
		t.Fatalf(
			"the first document was never written, so the unwind path was "+
				"never exercised: %v", err,
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
		target, MetadataStagePrepare, backups, root,
	); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	// Re-running into the same backup directory would overwrite the only record
	// of the original bytes.
	if _, err := applyMetadataStage(
		target, MetadataStageRetire, backups, root,
	); err == nil {
		t.Fatal("expected the second run to refuse the used backup directory")
	}
}

// The retire gate is keyed on ClassifyOwnership, which resolves the managed key
// alone. A resource whose ownership marker is dual but whose role pair is still
// legacy-only classifies as "dual" and would otherwise pass a host-wide gate,
// leaving the legacy namespace behind after the phase claimed it was gone.
func TestRetireGateSeesALegacyOnlyRolePairOnADualResource(t *testing.T) {
	inventory := &OwnershipInventory{
		Totals:  map[OwnershipClass]int{},
		ByKind:  map[MetadataResourceKind]int{},
		Managed: map[MetadataResourceKind]int{},
	}
	inventory.add(OwnershipInventoryRecord{
		Kind:  MetadataKindContainer,
		ID:    "agentops-runner",
		Class: OwnershipDual,
		Labels: map[string]string{
			LegacyManagedLabelKey:  ManagedLabelValue,
			CurrentManagedLabelKey: ManagedLabelValue,
			// The role pair never made it across.
			LegacyRoleLabelKey: "runner",
		},
	})
	if inventory.Records[0].Class != OwnershipDual {
		t.Fatalf("fixture is not dual: %q", inventory.Records[0].Class)
	}
	if err := inventory.RequireCurrentOwnershipEverywhere(); err == nil {
		t.Fatal("retire gate passed a resource with a legacy-only role pair")
	}
}

func TestOwnershipInventoryLegacyListIsDeterministic(t *testing.T) {
	build := func() []string {
		inventory := &OwnershipInventory{
			Totals:  map[OwnershipClass]int{},
			ByKind:  map[MetadataResourceKind]int{},
			Managed: map[MetadataResourceKind]int{},
		}
		for _, record := range []OwnershipInventoryRecord{
			{Kind: MetadataKindVolume, ID: "vol-b", Class: OwnershipLegacyOnly},
			{Kind: MetadataKindNetwork, ID: "net-a", Class: OwnershipLegacyOnly},
			{Kind: MetadataKindVolume, ID: "vol-a", Class: OwnershipLegacyOnly},
		} {
			inventory.add(record)
		}
		sortOwnershipInventory(inventory)
		rendered := make([]string, 0, len(inventory.Legacy))
		for _, record := range inventory.Legacy {
			rendered = append(rendered, string(record.Kind)+"/"+record.ID)
		}
		return rendered
	}
	first := build()
	for attempt := 0; attempt < 5; attempt++ {
		if got := build(); !reflect.DeepEqual(got, first) {
			t.Fatalf("legacy list reordered between runs: %v vs %v", first, got)
		}
	}
	want := []string{"network/net-a", "volume/vol-a", "volume/vol-b"}
	if !reflect.DeepEqual(first, want) {
		t.Fatalf("legacy list = %v, want %v", first, want)
	}
}
