package main

import (
	"strings"
	"testing"

	"github.com/mrbaron3/servo/apps/control-plane/internal/lifecycle"
)

// The cases below pin how the lifecycle owner reads container ownership before
// the Issue #123 label migration. Containers created by the pre-migration
// binary carry only `com.mrbaron3.workflow.*`, so reconciliation has to keep
// accepting them unchanged for the whole migration.

func legacyOwnedActual(name, role string) *lifecycle.ContainerActual {
	actual := &lifecycle.ContainerActual{}
	actual.ID = name
	actual.Status.State = "running"
	actual.Configuration.Labels = map[string]string{
		"com.mrbaron3.workflow.agentopsctl": "v1",
		"com.mrbaron3.workflow.role":        role,
	}
	actual.Configuration.Networks = []struct {
		Network string `json:"network"`
	}{{Network: "agentops-internal"}}
	actual.Configuration.Image.Reference = "runner:test"
	return actual
}

func TestValidateManagedActualAcceptsLegacyOwnershipAndRejectsForeignOrRoleDrift(t *testing.T) {
	actual := legacyOwnedActual("agentops-runner", "runner")
	if err := validateManagedActual(
		actual, "agentops-runner", "runner", "runner:test",
		[]string{"agentops-internal"}, false,
	); err != nil {
		t.Fatalf("legacy-owned container was rejected: %v", err)
	}

	foreign := legacyOwnedActual("agentops-runner", "runner")
	delete(foreign.Configuration.Labels, "com.mrbaron3.workflow.agentopsctl")
	if err := validateManagedActual(
		foreign, "agentops-runner", "runner", "runner:test",
		[]string{"agentops-internal"}, false,
	); err == nil {
		t.Fatal("an unowned container name was accepted")
	}

	misrouted := legacyOwnedActual("agentops-runner", "triage")
	if err := validateManagedActual(
		misrouted, "agentops-runner", "runner", "runner:test",
		[]string{"agentops-internal"}, false,
	); err == nil {
		t.Fatal("a container with a foreign role label was accepted")
	}
}

func TestManagedDashboardControlRequiresOwnershipAndControlRole(t *testing.T) {
	cfg := testManagerConfig()
	actual := legacyOwnedActual(cfg.ControlContainer, "control")
	actual.Configuration.PublishedPorts = []map[string]any{{
		"hostAddress":   "127.0.0.1",
		"hostPort":      float64(cfg.ControlHostPort),
		"containerPort": float64(8080),
		"count":         float64(1),
		"proto":         "tcp",
	}}
	if !managedDashboardControl(actual, cfg) {
		t.Fatal("a legacy-owned control container was not recognised")
	}

	unowned := *actual
	unowned.Configuration.Labels = map[string]string{
		"com.mrbaron3.workflow.role": "control",
	}
	if managedDashboardControl(&unowned, cfg) {
		t.Fatal("an unowned control container name was recognised")
	}
}

func TestValidateSpecActualReadsLegacySpecDigestLabel(t *testing.T) {
	spec := lifecycle.ContainerSpec{
		Name: "agentops-control", Role: "control", Image: "control:test",
		Networks: []string{"agentops-internal"}, Detach: true,
	}
	image := "sha256:" + strings.Repeat("a", 64)
	digest, err := lifecycle.SpecDigest(spec, image)
	if err != nil {
		t.Fatal(err)
	}
	spec.SpecDigest = digest
	actual := &lifecycle.ContainerActual{}
	actual.ID = spec.Name
	actual.Configuration.Image.Descriptor.Digest = image
	actual.Configuration.Labels = map[string]string{
		"com.mrbaron3.workflow.spec-sha256": digest,
	}
	if err := validateSpecActual(actual, spec); err != nil {
		t.Fatalf("legacy specification digest label was rejected: %v", err)
	}

	actual.Configuration.Labels = map[string]string{}
	if err := validateSpecActual(actual, spec); err == nil {
		t.Fatal("a container without a specification digest label was accepted")
	}
}
