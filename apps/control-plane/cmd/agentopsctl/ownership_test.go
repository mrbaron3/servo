package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

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

// The remaining cases are the Phase 1 acceptance surface on the reconciliation
// side: every classification Issue #123 enumerates reaches an explicit verdict,
// a partially migrated container stops reconciliation instead of reading as
// somebody else's name, and a dual-labelled container is accepted unchanged.

// ownershipLabelCases enumerates the six migration states an ownership marker
// can be in on a live container.
var ownershipLabelCases = []struct {
	name     string
	labels   map[string]string
	owned    bool
	conflict bool
}{
	{
		name:   "old-only",
		labels: map[string]string{lifecycle.LegacyManagedLabelKey: "v1"},
		owned:  true,
	},
	{
		name:   "new-only",
		labels: map[string]string{lifecycle.CurrentManagedLabelKey: "v1"},
		owned:  true,
	},
	{
		name: "dual-equal",
		labels: map[string]string{
			lifecycle.LegacyManagedLabelKey:  "v1",
			lifecycle.CurrentManagedLabelKey: "v1",
		},
		owned: true,
	},
	{
		name: "dual-conflicting",
		labels: map[string]string{
			lifecycle.LegacyManagedLabelKey:  "v1",
			lifecycle.CurrentManagedLabelKey: "v2",
		},
		conflict: true,
	},
	{
		name:   "unmanaged",
		labels: map[string]string{lifecycle.LegacyManagedLabelKey: "v2"},
	},
	{
		name:   "missing-label",
		labels: map[string]string{},
	},
}

// assertOwnershipRejection checks that a rejection names the right reason: a
// partially migrated container must never be reported as one this binary does
// not own, because "unowned" is what later phases read as "somebody else's".
func assertOwnershipRejection(t *testing.T, err error, conflict bool) {
	t.Helper()
	if err == nil {
		t.Fatal("a container this binary does not own was accepted")
	}
	if conflict {
		if !strings.Contains(err.Error(), "conflicting ownership labels") ||
			!strings.Contains(err.Error(), "partial container label migration") {
			t.Fatalf("a partially migrated container was not reported as such: %v", err)
		}
		if strings.Contains(err.Error(), "is not owned by agentopsctl") {
			t.Fatalf("a partially migrated container was reported as unowned: %v", err)
		}
		return
	}
	if !strings.Contains(err.Error(), "is not owned by agentopsctl") {
		t.Fatalf("a foreign container was not reported as unowned: %v", err)
	}
}

func withRole(labels map[string]string, role string) map[string]string {
	merged := map[string]string{lifecycle.LegacyRoleLabelKey: role}
	if _, current := labels[lifecycle.CurrentManagedLabelKey]; current {
		merged[lifecycle.CurrentRoleLabelKey] = role
	}
	for key, value := range labels {
		merged[key] = value
	}
	return merged
}

func TestValidateManagedActualClassifiesEveryOwnershipMigrationState(t *testing.T) {
	for _, testCase := range ownershipLabelCases {
		t.Run(testCase.name, func(t *testing.T) {
			actual := legacyOwnedActual("agentops-runner", "runner")
			actual.Configuration.Labels = withRole(testCase.labels, "runner")
			err := validateManagedActual(
				actual, "agentops-runner", "runner", "runner:test",
				[]string{"agentops-internal"}, false,
			)
			if testCase.owned {
				if err != nil {
					t.Fatalf("an owned container was rejected: %v", err)
				}
				return
			}
			assertOwnershipRejection(t, err, testCase.conflict)
		})
	}
}

func TestValidateManagedActualFailsClosedOnConflictingRoleLabels(t *testing.T) {
	actual := legacyOwnedActual("agentops-runner", "runner")
	actual.Configuration.Labels = map[string]string{
		lifecycle.LegacyManagedLabelKey:  "v1",
		lifecycle.CurrentManagedLabelKey: "v1",
		lifecycle.LegacyRoleLabelKey:     "runner",
		lifecycle.CurrentRoleLabelKey:    "triage",
	}
	err := validateManagedActual(
		actual, "agentops-runner", "runner", "runner:test",
		[]string{"agentops-internal"}, false,
	)
	if err == nil {
		t.Fatal("conflicting role labels were accepted")
	}
	if !strings.Contains(err.Error(), "conflicting role labels") ||
		!strings.Contains(err.Error(), "partial container label migration") {
		t.Fatalf("a partially migrated role was not reported as such: %v", err)
	}

	missing := legacyOwnedActual("agentops-runner", "runner")
	missing.Configuration.Labels = map[string]string{
		lifecycle.CurrentManagedLabelKey: "v1",
	}
	if err := validateManagedActual(
		missing, "agentops-runner", "runner", "runner:test",
		[]string{"agentops-internal"}, false,
	); err == nil {
		t.Fatal("a managed container without any role label was accepted")
	}
}

func TestValidateSpecActualReadsEitherNamespaceAndFailsClosedOnConflict(t *testing.T) {
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
	newActual := func(labels map[string]string) *lifecycle.ContainerActual {
		actual := &lifecycle.ContainerActual{}
		actual.ID = spec.Name
		actual.Configuration.Image.Descriptor.Digest = image
		actual.Configuration.Labels = labels
		return actual
	}

	for _, labels := range []map[string]string{
		{lifecycle.LegacySpecLabelKey: digest},
		{lifecycle.CurrentSpecLabelKey: digest},
		{
			lifecycle.LegacySpecLabelKey:  digest,
			lifecycle.CurrentSpecLabelKey: digest,
		},
	} {
		if err := validateSpecActual(newActual(labels), spec); err != nil {
			t.Fatalf("a sealed container was rejected for %v: %v", labels, err)
		}
	}

	conflicting := newActual(map[string]string{
		lifecycle.LegacySpecLabelKey:  digest,
		lifecycle.CurrentSpecLabelKey: strings.Repeat("b", 64),
	})
	err = validateSpecActual(conflicting, spec)
	if err == nil {
		t.Fatal("conflicting specification digest labels were accepted")
	}
	if !strings.Contains(err.Error(), "conflicting specification digest labels") {
		t.Fatalf("a partially migrated digest was reported as plain drift: %v", err)
	}
	if strings.Contains(err.Error(), "drifted") {
		t.Fatalf("a partial migration was reported as image/spec drift: %v", err)
	}

	drifted := newActual(map[string]string{
		lifecycle.CurrentSpecLabelKey: strings.Repeat("b", 64),
	})
	if err := validateSpecActual(drifted, spec); err == nil ||
		!strings.Contains(err.Error(), "drifted") {
		t.Fatalf("genuine specification drift was not reported: %v", err)
	}
}

func TestManagedDashboardControlAcceptsEitherNamespaceAndRejectsConflict(t *testing.T) {
	cfg := testManagerConfig()
	for _, testCase := range ownershipLabelCases {
		t.Run(testCase.name, func(t *testing.T) {
			actual := legacyOwnedActual(cfg.ControlContainer, "control")
			actual.Configuration.Labels = withRole(testCase.labels, "control")
			actual.Configuration.PublishedPorts = []map[string]any{{
				"hostAddress":   "127.0.0.1",
				"hostPort":      float64(cfg.ControlHostPort),
				"containerPort": float64(8080),
				"count":         float64(1),
				"proto":         "tcp",
			}}
			if managedDashboardControl(actual, cfg) != testCase.owned {
				t.Fatalf("dashboard control verdict is wrong for %q", testCase.name)
			}
		})
	}

	conflictingRole := legacyOwnedActual(cfg.ControlContainer, "control")
	conflictingRole.Configuration.Labels = map[string]string{
		lifecycle.LegacyManagedLabelKey:  "v1",
		lifecycle.CurrentManagedLabelKey: "v1",
		lifecycle.LegacyRoleLabelKey:     "control",
		lifecycle.CurrentRoleLabelKey:    "runner",
	}
	conflictingRole.Configuration.PublishedPorts = []map[string]any{{
		"hostAddress":   "127.0.0.1",
		"hostPort":      float64(cfg.ControlHostPort),
		"containerPort": float64(8080),
		"count":         float64(1),
		"proto":         "tcp",
	}}
	if managedDashboardControl(conflictingRole, cfg) {
		t.Fatal("a container with conflicting role labels was opened as the dashboard")
	}
}

func TestGracefulStopAndRemoveRunnerNeverMutateUnownedOrConflictingContainers(t *testing.T) {
	cfg := testManagerConfig()
	for _, testCase := range ownershipLabelCases {
		t.Run(testCase.name, func(t *testing.T) {
			encoded, err := json.Marshal(withRole(testCase.labels, "runner"))
			if err != nil {
				t.Fatal(err)
			}
			listing := `[{"id":"agentops-runner","configuration":{"labels":` +
				string(encoded) + `},"status":{"state":"running"}}]`
			fake := &managerRuntimeRunner{results: []lifecycle.CommandResult{
				{Status: 0, Stdout: listing},
				{Status: 0},
				{Status: 0, Stdout: `[{"id":"agentops-runner","configuration":` +
					`{"labels":` + string(encoded) + `},"status":{"state":"stopped"}}]`},
			}}
			subject := newManager(cfg, lifecycle.NewAppleRuntimeForTest(fake))
			stopErr := subject.gracefulStop(
				context.Background(),
				cfg.RunnerContainer,
				time.Second,
			)
			signalled := false
			for _, args := range fake.args {
				if len(args) != 0 && args[0] == "kill" {
					signalled = true
				}
			}
			if signalled != testCase.owned {
				t.Fatalf("TERM signalled=%v for %q (err=%v)", signalled, testCase.name, stopErr)
			}
			if testCase.owned != (stopErr == nil) {
				t.Fatalf("unexpected gracefulStop error for %q: %v", testCase.name, stopErr)
			}
			if !testCase.owned {
				assertOwnershipRejection(t, stopErr, testCase.conflict)
			}

			removeFake := &managerRuntimeRunner{results: []lifecycle.CommandResult{
				{Status: 0, Stdout: listing},
			}}
			removeSubject := newManager(cfg, lifecycle.NewAppleRuntimeForTest(removeFake))
			receipt, removeErr := removeSubject.removeRunner(context.Background())
			if testCase.owned {
				if !receipt.Mutated {
					t.Fatalf("an owned runner was not scheduled for removal: %v", removeErr)
				}
				return
			}
			assertOwnershipRejection(t, removeErr, testCase.conflict)
			if receipt.Mutated {
				t.Fatalf("an unowned runner produced a mutation receipt: %#v", receipt)
			}
			for _, args := range removeFake.args {
				if len(args) != 0 && (args[0] == "delete" || args[0] == "kill") {
					t.Fatalf("an unowned runner reached a mutation: %#v", removeFake.args)
				}
			}
		})
	}
}

func TestEnsurePostgresRefusesConflictingAndForeignOwnershipBeforeMutating(t *testing.T) {
	cfg := testManagerConfig()
	for _, testCase := range ownershipLabelCases {
		if testCase.owned {
			continue
		}
		t.Run(testCase.name, func(t *testing.T) {
			encoded, err := json.Marshal(withRole(testCase.labels, "postgres"))
			if err != nil {
				t.Fatal(err)
			}
			fake := &managerRuntimeRunner{results: []lifecycle.CommandResult{
				{Status: 0, Stdout: `{"configuration":{"descriptor":{"digest":"sha256:` +
					strings.Repeat("a", 64) + `"}}}`},
				{Status: 0, Stdout: `[{"id":"agentops-postgres","configuration":{"labels":` +
					string(encoded) + `},"status":{"state":"running"}}]`},
			}}
			subject := newManager(cfg, lifecycle.NewAppleRuntimeForTest(fake))
			_, err = subject.ensurePostgres(context.Background())
			assertOwnershipRejection(t, err, testCase.conflict)
			for _, args := range fake.args {
				if len(args) != 0 &&
					(args[0] == "delete" || args[0] == "run" || args[0] == "kill") {
					t.Fatalf("an unowned PostgreSQL container reached a mutation: %#v", fake.args)
				}
			}
		})
	}
}
