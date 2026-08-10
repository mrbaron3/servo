package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Phase 3A withdraws the Phase 2 container sweep. Two things have to stay true
// afterwards: the refusal happens before the runtime is touched at all, and the
// evidence the sweep already wrote is still readable.

// countingSweepRuntime records whether anything asked the runtime a question.
type countingSweepRuntime struct{ calls int }

func (runtime *countingSweepRuntime) Containers(
	context.Context,
) ([]ContainerActual, error) {
	runtime.calls++
	return nil, nil
}

func TestApplyRefusesBeforeTouchingTheRuntime(t *testing.T) {
	runtime := &countingSweepRuntime{}
	sweeper := NewLabelSweeper(runtime)
	report, err := sweeper.Apply(context.Background())
	if err == nil {
		t.Fatal("the retired sweep applied something")
	}
	if !errors.Is(err, ErrLabelSweepRetired) {
		t.Fatalf("unexpected refusal: %v", err)
	}
	// The refusal must come before any listing. A sweep that inventoried first
	// could be made to start the Apple Container services as a side effect of
	// being asked to do something it will not do.
	if runtime.calls != 0 {
		t.Fatalf("the retired sweep queried the runtime %d time(s)", runtime.calls)
	}
	if report.Applied {
		t.Fatal("the report claims the sweep applied")
	}
	if report.Halted == "" {
		t.Fatal("a refusal must say why in the report it returns")
	}
}

func TestPlanStillTakesAReadOnlyInventory(t *testing.T) {
	runtime := &countingSweepRuntime{}
	if _, err := NewLabelSweeper(runtime).Plan(
		context.Background(), "dry-run",
	); err != nil {
		t.Fatalf("the read-only inventory stopped working: %v", err)
	}
	if runtime.calls != 1 {
		t.Fatalf("inventory made %d listing calls, want 1", runtime.calls)
	}
}

// The schemas below outlive the code that wrote them. `evidence/label-p2/`
// holds records produced by the sweep before it was withdrawn, and they are
// part of the epic's audit trail — so they have to keep decoding into the types
// this package still exports.
//
// What "keep decoding" means is narrower than it looks, and the assertions below
// say which parts are actually guaranteed. Identity, state, disposition, and the
// volume attachments survive verbatim. The per-record `roleAgreement` and
// `specAgreement` fields do NOT: Phase 3B replaced a relation between two
// namespaces with the presence of one label, and there is no honest value to
// decode `"dual"` or `"legacy-only"` into. They are readable in the committed
// JSON, which is where that history belongs, and this test does not pretend the
// Go types still carry them.
func TestMergedPhase2EvidenceStillDecodes(t *testing.T) {
	root := repositoryRootForTest(t)
	directory := filepath.Join(root, "evidence", "label-p2")
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Skipf("no merged Phase 2 evidence on disk: %v", err)
	}
	type phase2Evidence struct {
		Mode         string               `json:"mode"`
		Plan         MigrationAudit       `json:"plan"`
		PlannedSpecs []PlannedReplacement `json:"plannedSpecs"`
		Sweep        *SweepReport         `json:"sweep"`
	}
	decoded := 0
	sawSweep, sawPlannedSpec := false, false
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		var evidence phase2Evidence
		// DisallowUnknownFields would be wrong here: this proves the retained
		// schema can still READ the merged records, not that it is their only
		// possible shape.
		if err := json.Unmarshal(raw, &evidence); err != nil {
			t.Fatalf("merged Phase 2 evidence %s no longer decodes: %v",
				entry.Name(), err)
		}
		decoded++
		if evidence.Sweep != nil && len(evidence.Sweep.Steps) > 0 {
			sawSweep = true
			for _, step := range evidence.Sweep.Steps {
				if step.Stage == "" || step.Container == "" {
					t.Fatalf(
						"%s decoded a sweep step with empty fields: %#v",
						entry.Name(), step,
					)
				}
			}
			for _, volume := range evidence.Sweep.Volumes {
				if volume.Name == "" {
					t.Fatalf("%s decoded a nameless volume record", entry.Name())
				}
			}
		}
		// Dispositions the retired sweep produced decode as plain values of the
		// defined string type, which is what keeps the totals readable without a
		// constant for each. Asserting it here is what makes migration.go's claim
		// to that effect a tested one rather than a comment.
		for disposition, count := range evidence.Plan.Totals {
			if disposition == "" {
				t.Fatalf("%s decoded a nameless disposition", entry.Name())
			}
			if count < 0 {
				t.Fatalf("%s decoded a negative total", entry.Name())
			}
		}
		for _, record := range evidence.Plan.Records {
			if record.ID == "" || record.Disposition == "" {
				t.Fatalf(
					"%s decoded an unusable inventory record: %#v",
					entry.Name(), record,
				)
			}
		}
		if len(evidence.PlannedSpecs) > 0 {
			sawPlannedSpec = true
			for _, planned := range evidence.PlannedSpecs {
				if planned.Name == "" || planned.Image == "" {
					t.Fatalf(
						"%s decoded an unusable planned replacement: %#v",
						entry.Name(), planned,
					)
				}
			}
		}
	}
	if decoded == 0 {
		t.Skip("no Phase 2 evidence files present")
	}
	// SweepReport has to be exercised by the real corpus, or this test proved
	// nothing about the schema it exists to protect.
	if !sawSweep {
		t.Error("no merged evidence exercised the SweepReport schema")
	}
	// PlannedReplacement is deliberately NOT required to appear. Every merged
	// pre-mutation record was written with an empty plannedSpecs array, which
	// `omitempty` dropped, so the committed corpus never carries one. The type
	// is retained because it is SweepReport's element type and part of the
	// schema Phase 2 published, not because these files depend on it — and
	// saying so here is more useful than an assertion that would pass by
	// accident.
	if sawPlannedSpec {
		t.Log("merged evidence exercised the PlannedReplacement schema")
	}
}

// TestPlannedReplacementSchemaStillRoundTrips covers the element type the
// committed corpus does not reach, so retiring the sweep cannot silently break
// a Phase 2 record written on another host.
func TestPlannedReplacementSchemaStillRoundTrips(t *testing.T) {
	original := PlannedReplacement{
		Name: "agentops-runner", Role: "runner", Image: "agentops-runner:dev",
		ImageDigest: "sha256:abc", SpecDigest: "def",
		Networks: []string{"agentops-internal"}, Tmpfs: []string{"/tmp"},
		EnvironmentKeys: []string{"AGENTOPS_DATABASE_URL"},
		ReadOnly:        true, CapDropAll: true, Init: true,
		User: "agentops", Entrypoint: "node", Command: []string{"cli.js"},
		ObservedState: "running", RecreateVerb: "run",
		CPUs: 4, MemoryMiB: 1024,
		ObservedWorkingDirectory: "/app",
	}
	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var decoded PlannedReplacement
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("PlannedReplacement no longer round trips: %v", err)
	}
	if decoded.Name != original.Name || decoded.CPUs != original.CPUs ||
		decoded.MemoryMiB != original.MemoryMiB ||
		len(decoded.EnvironmentKeys) != 1 {
		t.Fatalf("round trip lost fields: %#v", decoded)
	}
	// Environment VALUES were never part of this schema and must not become so.
	// The check is against the marshalled document: the schema carries keys
	// only, so a value that reached it would appear here.
	rendered := string(encoded)
	if !strings.Contains(rendered, `"environmentKeys"`) {
		t.Fatalf("the schema lost its environment key list: %s", rendered)
	}
	for _, forbidden := range []string{
		"environmentValues", "\"environment\":", "postgres://",
	} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("the schema now carries %q: %s", forbidden, rendered)
		}
	}
}

// repositoryRootForTest walks up to the directory holding go.work or .git.
func repositoryRootForTest(t *testing.T) string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(directory, ".git")); err == nil {
			return directory
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			t.Skip("not running inside a repository checkout")
		}
		directory = parent
	}
}
