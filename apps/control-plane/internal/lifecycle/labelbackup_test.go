package lifecycle

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// Evidence and the rollback plan are deliberately different artifacts, and these
// cases pin the difference. The evidence is committed to the repository, so an
// absolute path in it leaks the operator's home directory and a backup path in
// it points a reader at files containing environment values. The plan is not
// committed, and it has to carry exactly what the evidence omits, because
// nothing else can drive a recovery.
//
// Phase 3B removes the code that chose where a forward run would write its
// backups, along with the forward run itself. The separation these cases
// describe survives it: a retained plan is still read from a private root, and
// the record it carries still has to serialise without host paths.

func TestCommittedEvidenceCarriesNoHostPath(t *testing.T) {
	root := seedAppRoot(t)
	backups := filepath.Join(t.TempDir(), "private-backups")
	record := phase3ARecord(
		t, MetadataKindContainer, "ctr-started", root, backups, legacyTriple(),
	)
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{root, backups} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf(
				"committed evidence carries the absolute path %q:\n%s",
				forbidden, encoded,
			)
		}
	}
	// The relative renderings must still be there, otherwise the evidence would
	// be sanitised into uselessness.
	if !strings.Contains(string(encoded), "containers/ctr-started/config.json") {
		t.Fatalf("evidence lost its relative document path:\n%s", encoded)
	}
	// A foreign label's value is arbitrary third-party text and never belongs in
	// a committed artifact, so the complete maps stay in the private plan.
	if strings.Contains(string(encoded), "keep-me") {
		t.Fatalf("committed evidence carries a foreign label value:\n%s", encoded)
	}
}

// TestRollbackPlanRoundTripsTheAbsolutePaths is the other half: the evidence
// cannot drive a rollback, so the private plan must.
func TestRollbackPlanRoundTripsTheAbsolutePaths(t *testing.T) {
	root := seedAppRoot(t)
	backups := filepath.Join(t.TempDir(), "private-backups")
	before := legacyTriple()
	record := phase3ARecord(
		t, MetadataKindVolume, "vol-a", root, backups, before,
	)
	encoded, err := json.Marshal(&RollbackPlan{
		Stage:   "retire",
		Applied: []*MetadataApplication{record},
		Locations: [][]RollbackLocation{{{
			Path:       record.Files[0].Path,
			BackupPath: record.Files[0].BackupPath,
		}}},
		Labels: []RollbackLabels{{
			Before: before, After: record.AfterLabels,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := ParseRollbackPlan(encoded)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := plan.Applied[0].Files[0].Path; got != record.Files[0].Path {
		t.Fatalf("plan lost the document path: %q", got)
	}
	if got := plan.Applied[0].Files[0].BackupPath; got != record.Files[0].BackupPath {
		t.Fatalf("plan lost the backup path: %q", got)
	}
	// The stage name is carried verbatim even though this binary no longer
	// defines it: the plan is an opaque restoration record.
	if plan.Applied[0].Stage != "retire" ||
		plan.Applied[0].ClassBefore != "legacy-only" {
		t.Fatalf("the plan lost the Phase 3A record it carries: %#v", plan.Applied[0])
	}
	if err := RollbackMetadataSweep(plan); err != nil {
		t.Fatalf("rollback from the parsed plan: %v", err)
	}
	restored := labelsOnDisk(
		t, record.Files[0].Path, record.Files[0].LabelPath,
	)
	if _, present := restored[CurrentManagedLabelKey]; present {
		t.Fatalf("rollback did not remove the migrated label: %v", restored)
	}
	if !sameLabels(restored, before) {
		t.Fatalf("rollback restored %v, want %v", restored, before)
	}
}

func TestParseRollbackPlanRefusesAMismatchedPlan(t *testing.T) {
	for name, body := range map[string]string{
		"no applied entries": `{"stage":"retire","applied":[],"locations":[]}`,
		"location count mismatch": `{"stage":"retire","applied":[` +
			`{"kind":"volume","id":"v","files":[{"document":"a"}]}],` +
			`"locations":[]}`,
		"document count mismatch": `{"stage":"retire","applied":[` +
			`{"kind":"volume","id":"v","files":[{"document":"a"}]}],` +
			`"locations":[[]],"labels":[{"before":{"a":"b"},"after":{"c":"d"}}]}`,
		"no label maps": `{"stage":"retire","applied":[` +
			`{"kind":"volume","id":"v","files":[{"document":"a"}]}],` +
			`"locations":[[{"path":"/tmp/a","backupPath":"/tmp/b"}]],` +
			`"labels":[{"before":{},"after":{}}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseRollbackPlan([]byte(body)); err == nil {
				t.Fatalf("expected %s to be refused", name)
			}
		})
	}
}
