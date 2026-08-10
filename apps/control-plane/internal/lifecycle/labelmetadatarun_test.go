package lifecycle

import (
	"strings"
	"testing"
)

// Claude round 2 noted that the gates below had no headless coverage. redactRoots
// in particular is the only thing keeping the operator's home directory out of
// report.Halted, which is the one free-form string that reaches committed
// evidence.

func TestRedactRootsRemovesMachineSpecificPrefixes(t *testing.T) {
	appRoot := "/Users/operator/Library/Application Support/com.apple.container"
	backupRoot := "/Users/operator/.local/state/agentops/label-metadata-backups"
	message := "open " + appRoot + "/volumes/x/entity.json: permission denied; " +
		"backup " + backupRoot + "/prepare-1/volume/x/entity.json exists"
	redacted := redactRoots(message, appRoot, backupRoot)
	for _, forbidden := range []string{appRoot, backupRoot, "/Users/operator"} {
		if strings.Contains(redacted, forbidden) {
			t.Fatalf("redaction left %q in %q", forbidden, redacted)
		}
	}
	// The message still has to be usable: redaction is not deletion.
	if !strings.Contains(redacted, "permission denied") ||
		!strings.Contains(redacted, "volumes/x/entity.json") {
		t.Fatalf("redaction destroyed the diagnosis: %q", redacted)
	}
	if redactRoots("nothing to redact", "", "") != "nothing to redact" {
		t.Fatal("empty roots must not alter the message")
	}
}

func TestRunningManagedContainersIgnoresUnownedAndStopped(t *testing.T) {
	inventory := &OwnershipInventory{
		Totals:  map[OwnershipClass]int{},
		ByKind:  map[MetadataResourceKind]int{},
		Managed: map[MetadataResourceKind]int{},
	}
	for _, record := range []OwnershipInventoryRecord{
		{Kind: MetadataKindContainer, ID: "managed-running",
			Class: OwnershipDual, State: "running"},
		{Kind: MetadataKindContainer, ID: "managed-stopped",
			Class: OwnershipDual, State: "stopped"},
		// Not ours: another deployment's running container must not block us.
		{Kind: MetadataKindContainer, ID: "foreign-running",
			Class: OwnershipUnmanaged, State: "running"},
		// A volume has no state and must never be counted as a running container.
		{Kind: MetadataKindVolume, ID: "vol", Class: OwnershipLegacyOnly},
	} {
		inventory.add(record)
	}
	running := inventory.RunningManagedContainers()
	if len(running) != 1 || running[0] != "managed-running" {
		t.Fatalf("running managed containers = %v", running)
	}
}

func TestRequireConflictFreeNamesEveryConflictingResource(t *testing.T) {
	inventory := &OwnershipInventory{
		Totals:  map[OwnershipClass]int{},
		ByKind:  map[MetadataResourceKind]int{},
		Managed: map[MetadataResourceKind]int{},
	}
	inventory.add(OwnershipInventoryRecord{
		Kind: MetadataKindVolume, ID: "vol-a", Class: OwnershipConflicting,
	})
	inventory.add(OwnershipInventoryRecord{
		Kind: MetadataKindContainer, ID: "ctr-a", Class: OwnershipDual,
	})
	err := inventory.RequireConflictFree()
	if err == nil {
		t.Fatal("a conflicting resource passed the gate")
	}
	if !strings.Contains(err.Error(), "vol-a") {
		t.Fatalf("the refusal does not name the resource: %v", err)
	}
	// A clean host must pass.
	clean := &OwnershipInventory{
		Totals:  map[OwnershipClass]int{},
		ByKind:  map[MetadataResourceKind]int{},
		Managed: map[MetadataResourceKind]int{},
	}
	clean.add(OwnershipInventoryRecord{
		Kind: MetadataKindContainer, ID: "ctr-a", Class: OwnershipDual,
	})
	if err := clean.RequireConflictFree(); err != nil {
		t.Fatalf("a conflict-free host was refused: %v", err)
	}
}
