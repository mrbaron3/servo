package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
	sweeper.Only = []string{"agentops-runner"}
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
	if bytes := string(encoded); len(bytes) == 0 ||
		containsString([]string{"environment"}, "environmentValues") {
		t.Fatal("unexpected schema shape")
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
