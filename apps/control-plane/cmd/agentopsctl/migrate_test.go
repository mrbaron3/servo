package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Evidence is the record of an irreversible operation. A second run that
// happens to choose the same name must fail loudly rather than silently replace
// the first run's account of what it did.
func TestMigrationEvidenceRefusesToOverwriteAnExistingRecord(t *testing.T) {
	directory := t.TempDir()
	evidence := labelMigrationEvidence{GeneratedAt: "now", Mode: "apply"}

	path, err := writeLabelMigrationEvidence(directory, "sweep.json", evidence)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	second := labelMigrationEvidence{GeneratedAt: "later", Mode: "apply"}
	if _, err := writeLabelMigrationEvidence(
		directory, "sweep.json", second,
	); err == nil {
		t.Fatal("a second run silently replaced the first run's evidence")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Fatal("the first run's evidence was modified")
	}
}

// Two runs must not collide on a file name in the first place.
func TestEvidenceRunIdentitiesDiffer(t *testing.T) {
	seen := make(map[string]bool, 32)
	for attempt := 0; attempt < 32; attempt++ {
		identity, err := evidenceRunID()
		if err != nil {
			t.Fatal(err)
		}
		if identity == "" || strings.ContainsAny(identity, "/\\.") {
			t.Fatalf("run identity is not a safe file name component: %q",
				identity)
		}
		if seen[identity] {
			t.Fatalf("run identity repeated within 32 draws: %q", identity)
		}
		seen[identity] = true
	}
}

// splitIdentities drops empty entries rather than passing through an identity
// that could never match a container.
func TestSplitIdentitiesDropsEmptyEntries(t *testing.T) {
	identities := splitIdentities(" agentops-runner , ,agentops-triage,")
	if len(identities) != 2 ||
		identities[0] != "agentops-runner" ||
		identities[1] != "agentops-triage" {
		t.Fatalf("splitIdentities() = %#v", identities)
	}
	if len(splitIdentities("   ")) != 0 {
		t.Fatal("a blank --only produced an identity")
	}
}

// The evidence directory is created on demand, and the returned path is the one
// that was written.
func TestMigrationEvidencePathIsTheFileItWrote(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "nested", "label-p2")
	path, err := writeLabelMigrationEvidence(
		directory, "inventory.json", labelMigrationEvidence{Mode: "dry-run"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(directory, "inventory.json") {
		t.Fatalf("unexpected evidence path: %s", path)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"mode": "dry-run"`) {
		t.Fatalf("evidence body is not what was written: %s", body)
	}
}
