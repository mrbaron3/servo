package lifecycle

import (
	"context"
	"strings"
	"testing"
)

// The cases below pin the ownership contract that Issue #123 migrates from
// `com.mrbaron3.workflow.*` to `com.mrbaron3.servo.*`. They must keep passing
// after the dual-label change: a rollback to the pre-migration binary reads
// only the legacy namespace, so every resource this binary creates has to stay
// discoverable through the legacy keys alone.

const legacyManagedLabel = "com.mrbaron3.workflow.agentopsctl"

func TestRunArgsKeepLegacyOwnershipAndRoleLabelsForRollback(t *testing.T) {
	args, _, err := buildContainerArgs(ContainerSpec{
		Name: "agentops-runner", Role: "runner", Image: "runner:test",
		Networks: []string{"agentops-internal"}, Detach: true,
		SpecDigest: strings.Repeat("a", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	rendered := strings.Join(args, " ")
	for _, expected := range []string{
		"--label com.mrbaron3.workflow.agentopsctl=v1",
		"--label com.mrbaron3.workflow.role=runner",
		"--label com.mrbaron3.workflow.spec-sha256=" + strings.Repeat("a", 64),
	} {
		if !strings.Contains(rendered, expected) {
			t.Fatalf("legacy label %q is absent from %s", expected, rendered)
		}
	}
}

func TestEnsureNetworkAndVolumeKeepLegacyOwnershipLabelOnCreate(t *testing.T) {
	network := &fakeRuntimeRunner{results: []CommandResult{{Status: 0, Stdout: `[]`}}}
	if err := NewAppleRuntimeForTest(network).EnsureNetwork(
		context.Background(),
		"agentops-internal",
	); err != nil {
		t.Fatal(err)
	}
	if len(network.args) != 2 ||
		!strings.Contains(
			strings.Join(network.args[1], " "),
			"--label com.mrbaron3.workflow.agentopsctl=v1",
		) {
		t.Fatalf("network create lost the legacy ownership label: %#v", network.args)
	}

	volume := &fakeRuntimeRunner{results: []CommandResult{{Status: 0, Stdout: `[]`}}}
	if err := NewAppleRuntimeForTest(volume).EnsureVolume(
		context.Background(),
		"agentops-postgres-data",
	); err != nil {
		t.Fatal(err)
	}
	if len(volume.args) != 2 ||
		!strings.Contains(
			strings.Join(volume.args[1], " "),
			"--label com.mrbaron3.workflow.agentopsctl=v1",
		) {
		t.Fatalf("volume create lost the legacy ownership label: %#v", volume.args)
	}
}

func TestEnsureVolumeAcceptsLegacyOwnedVolumeAndRejectsForeignVolume(t *testing.T) {
	owned := &fakeRuntimeRunner{results: []CommandResult{{
		Status: 0,
		Stdout: `[{"id":"agentops-postgres-data","configuration":{"labels":` +
			`{"com.mrbaron3.workflow.agentopsctl":"v1"}}}]`,
	}}}
	if err := NewAppleRuntimeForTest(owned).EnsureVolume(
		context.Background(),
		"agentops-postgres-data",
	); err != nil {
		t.Fatalf("legacy-owned volume was rejected: %v", err)
	}
	if len(owned.args) != 1 {
		t.Fatalf("an owned volume was recreated: %#v", owned.args)
	}

	foreign := &fakeRuntimeRunner{results: []CommandResult{{
		Status: 0,
		Stdout: `[{"id":"agentops-postgres-data","configuration":{"labels":` +
			`{"com.example.other":"v1"}}}]`,
	}}}
	if err := NewAppleRuntimeForTest(foreign).EnsureVolume(
		context.Background(),
		"agentops-postgres-data",
	); err == nil {
		t.Fatal("a foreign volume name was accepted")
	}
}

func TestDeleteRefusesContainerWithoutOwnershipLabel(t *testing.T) {
	fake := &fakeRuntimeRunner{results: []CommandResult{{
		Status: 0,
		Stdout: `[{"id":"agentops-runner","configuration":{"labels":{}},` +
			`"status":{"state":"stopped"}}]`,
	}}}
	if err := NewAppleRuntimeForTest(fake).Delete(
		context.Background(),
		"agentops-runner",
	); err == nil {
		t.Fatal("an unlabeled container was deleted")
	}
	for _, args := range fake.args {
		if len(args) != 0 && args[0] == "delete" {
			t.Fatalf("an unlabeled container reached delete: %#v", fake.args)
		}
	}

	owned := &fakeRuntimeRunner{results: []CommandResult{{
		Status: 0,
		Stdout: `[{"id":"agentops-runner","configuration":{"labels":` +
			`{"` + legacyManagedLabel + `":"v1"}},"status":{"state":"stopped"}}]`,
	}}}
	if err := NewAppleRuntimeForTest(owned).Delete(
		context.Background(),
		"agentops-runner",
	); err != nil {
		t.Fatalf("a legacy-owned container was not deleted: %v", err)
	}
	if len(owned.args) != 2 || owned.args[1][0] != "delete" {
		t.Fatalf("delete was not issued: %#v", owned.args)
	}
}
