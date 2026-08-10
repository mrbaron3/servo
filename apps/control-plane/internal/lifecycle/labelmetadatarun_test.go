package lifecycle

import (
	"testing"
)

// The inventory is what the rollback command reads before it stops anything, so
// its one remaining gate — "is a managed container still running?" — has to be
// exact about which records it counts.
//
// Phase 3B removed the other gates here along with the forward stages:
// RequireConflictFree and RequireCurrentOwnershipEverywhere both existed to
// compare the two namespaces, and redactRoots scrubbed a halted forward run's
// free-form error before it reached committed evidence. No forward run remains
// to halt.

func TestRunningManagedContainersIgnoresUnownedAndStopped(t *testing.T) {
	inventory := &OwnershipInventory{
		Totals:  map[OwnershipClass]int{},
		ByKind:  map[MetadataResourceKind]int{},
		Managed: map[MetadataResourceKind]int{},
	}
	for _, record := range []OwnershipInventoryRecord{
		{Kind: MetadataKindContainer, ID: "managed-running",
			Class: OwnershipOwned, State: "running"},
		{Kind: MetadataKindContainer, ID: "managed-stopped",
			Class: OwnershipOwned, State: "stopped"},
		// Not ours: another deployment's running container must not block us.
		{Kind: MetadataKindContainer, ID: "foreign-running",
			Class: OwnershipUnmanaged, State: "running"},
		// Since Phase 3B a legacy-only container reads as missing-label. It is
		// not ours, so it must not block a rollback either — which is the same
		// answer the unmanaged case gets, reached for a different reason.
		{Kind: MetadataKindContainer, ID: "legacy-running",
			Class: OwnershipMissingLabel, State: "running"},
		// A malformed container is neither ours nor foreign. It must not be
		// counted as a running managed container, because the rollback gate is
		// about documents the runtime is actively holding open.
		{Kind: MetadataKindContainer, ID: "malformed-running",
			Class: OwnershipMalformed, State: "running"},
		// A volume has no state and must never be counted as a running container.
		{Kind: MetadataKindVolume, ID: "vol", Class: OwnershipOwned},
	} {
		inventory.add(record)
	}
	running := inventory.RunningManagedContainers()
	if len(running) != 1 || running[0] != "managed-running" {
		t.Fatalf("running managed containers = %v", running)
	}
}

func TestOwnershipInventoryCountsEveryClassAndKind(t *testing.T) {
	inventory := &OwnershipInventory{
		Totals:  map[OwnershipClass]int{},
		ByKind:  map[MetadataResourceKind]int{},
		Managed: map[MetadataResourceKind]int{},
	}
	for _, record := range []OwnershipInventoryRecord{
		{Kind: MetadataKindContainer, ID: "b", Class: OwnershipOwned},
		{Kind: MetadataKindContainer, ID: "a", Class: OwnershipMissingLabel},
		{Kind: MetadataKindVolume, ID: "v", Class: OwnershipOwned},
		{Kind: MetadataKindNetwork, ID: "n", Class: OwnershipMalformed},
	} {
		inventory.add(record)
	}
	sortOwnershipInventory(inventory)
	if inventory.Totals[OwnershipOwned] != 2 ||
		inventory.Totals[OwnershipMissingLabel] != 1 ||
		inventory.Totals[OwnershipMalformed] != 1 {
		t.Fatalf("totals = %v", inventory.Totals)
	}
	if inventory.Managed[MetadataKindContainer] != 1 ||
		inventory.Managed[MetadataKindVolume] != 1 ||
		inventory.Managed[MetadataKindNetwork] != 0 {
		t.Fatalf("managed = %v", inventory.Managed)
	}
	// A stable order: the listing reaches committed evidence, and Go randomises
	// map iteration, so an unsorted inventory would produce a different diff on
	// every run against an unchanged host.
	if inventory.Records[0].ID != "a" || inventory.Records[1].ID != "b" {
		t.Fatalf("records are not sorted by kind then id: %#v", inventory.Records)
	}
}
