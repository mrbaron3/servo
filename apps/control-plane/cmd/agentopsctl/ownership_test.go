package main

import (
	"context"
	"encoding/json"
	"errors"
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
	if err := managedDashboardControl(actual, cfg); err != nil {
		t.Fatalf("a legacy-owned control container was not recognised: %v", err)
	}

	unowned := *actual
	unowned.Configuration.Labels = map[string]string{
		"com.mrbaron3.workflow.role": "control",
	}
	if err := managedDashboardControl(&unowned, cfg); err == nil {
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

// withRole adds the role label to the same namespaces the ownership marker
// occupies. A `current-only` container must carry a `current-only` role too —
// that is the shape Phase 3 leaves behind, and pinning it here is what keeps the
// current-namespace role reader covered on the reconciliation side.
func withRole(labels map[string]string, role string) map[string]string {
	_, legacy := labels[lifecycle.LegacyManagedLabelKey]
	_, current := labels[lifecycle.CurrentManagedLabelKey]
	merged := make(map[string]string, len(labels)+2)
	if legacy || !current {
		merged[lifecycle.LegacyRoleLabelKey] = role
	}
	if current {
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
			err := managedDashboardControl(actual, cfg)
			if testCase.owned {
				if err != nil {
					t.Fatalf("an owned control container was rejected: %v", err)
				}
				return
			}
			assertOwnershipRejection(t, err, testCase.conflict)
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
	err := managedDashboardControl(conflictingRole, cfg)
	if err == nil {
		t.Fatal("a container with conflicting role labels was opened as the dashboard")
	}
	// A partially migrated control container is reachable and healthy. Reporting
	// it as "not running" would send the operator hunting for a crash.
	if !errors.Is(err, lifecycle.ErrConflictingLabels) ||
		strings.Contains(err.Error(), "is not running") {
		t.Fatalf("a partial migration was diagnosed as a dead Control API: %v", err)
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

			stoppedListing := `[{"id":"agentops-runner","configuration":{"labels":` +
				string(encoded) + `},"status":{"state":"stopped"}}]`
			removeFake := &managerRuntimeRunner{results: []lifecycle.CommandResult{
				{Status: 0, Stdout: listing},        // removeRunner lookup
				{Status: 0, Stdout: listing},        // gracefulStop lookup
				{Status: 0},                         // kill --signal TERM
				{Status: 0, Stdout: stoppedListing}, // WaitState poll
				{Status: 0, Stdout: stoppedListing}, // Delete lookup
				{Status: 0},                         // delete
			}}
			removeSubject := newManager(cfg, lifecycle.NewAppleRuntimeForTest(removeFake))
			receipt, removeErr := removeSubject.removeRunner(context.Background())
			if testCase.owned {
				if removeErr != nil || !receipt.Mutated {
					t.Fatalf("an owned runner was not removed: %v (%#v)", removeErr, receipt)
				}
				var signalled, deleted bool
				for _, args := range removeFake.args {
					switch {
					case len(args) != 0 && args[0] == "kill":
						signalled = true
					case len(args) != 0 && args[0] == "delete":
						deleted = true
					}
				}
				if !signalled || !deleted {
					t.Fatalf(
						"removeRunner did not reach stop and delete: %#v",
						removeFake.args,
					)
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

// The cases below are regressions for the Phase 1 review. Each one failed
// before its fix, so each pins one way a partially migrated container could
// still have been mutated or misdiagnosed.

func TestDrainNeverSignalsUnownedOrConflictingWorkers(t *testing.T) {
	cfg := testManagerConfig()
	for _, testCase := range ownershipLabelCases {
		if testCase.owned {
			continue
		}
		t.Run(testCase.name, func(t *testing.T) {
			workers, err := json.Marshal(withRole(testCase.labels, "triage"))
			if err != nil {
				t.Fatal(err)
			}
			listing := `[{"id":"` + cfg.PostgresContainer + `","configuration":{"labels":` +
				`{"` + lifecycle.LegacyManagedLabelKey + `":"v1"}},"status":{"state":"running",` +
				`"networks":[{"network":"agentops-internal","ipv4Address":"192.0.2.10/24"}]}},` +
				`{"id":"` + cfg.TriageContainer + `","configuration":{"labels":` +
				string(workers) + `},"status":{"state":"running"}}]`
			status := `{"state":{"mode":"ACTIVE","generation":1,` +
				`"updatedAt":"2026-08-10T00:00:00Z"},` +
				`"databaseTime":"2026-08-10T00:00:00Z"}`
			drained := `{"state":{"mode":"DRAINING","generation":2,` +
				`"drainDeadlineAt":"2026-08-10T00:10:00Z",` +
				`"updatedAt":"2026-08-10T00:00:01Z"}}`
			fake := &managerRuntimeRunner{results: []lifecycle.CommandResult{
				{Status: 0, Stdout: "container 1.1.0"}, // Capability --version
				{Status: 0},                            // system status
				{Status: 0, Stdout: listing},           // databaseHost -> list
				{Status: 0, Stdout: status},            // admin: lifecycle status
				{Status: 0, Stdout: listing},           // databaseHost -> list
				{Status: 0, Stdout: drained},           // admin: lifecycle transition
				{Status: 0, Stdout: listing},           // triage worker lookup
				{Status: 0, Stdout: listing},           // runner worker lookup (absent)
			}}
			subject := newManager(cfg, lifecycle.NewAppleRuntimeForTest(fake))

			err = subject.Drain(context.Background(), time.Minute, "drain-request-001")
			assertOwnershipRejection(t, err, testCase.conflict)
			for _, args := range fake.args {
				if len(args) != 0 && (args[0] == "kill" || args[0] == "stop" ||
					args[0] == "delete") {
					t.Fatalf(
						"a worker this binary does not own reached %q: %#v",
						args[0], fake.args,
					)
				}
			}
		})
	}
}

func TestEnsurePostgresDoesNotOfferDriftRemediationForAPartialMigration(t *testing.T) {
	cfg := testManagerConfig()
	image := "sha256:" + strings.Repeat("c", 64)
	digest, err := lifecycle.SpecDigest(newManager(cfg, nil).postgresSpec(), image)
	if err != nil {
		t.Fatal(err)
	}
	labels, err := json.Marshal(map[string]string{
		lifecycle.LegacyManagedLabelKey:  "v1",
		lifecycle.CurrentManagedLabelKey: "v1",
		lifecycle.LegacyRoleLabelKey:     "postgres",
		lifecycle.CurrentRoleLabelKey:    "postgres",
		lifecycle.LegacySpecLabelKey:     digest,
		lifecycle.CurrentSpecLabelKey:    strings.Repeat("d", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	mounts := `[{"destination":"/tmp","type":{"tmpfs":{}}},` +
		`{"destination":"/run/postgresql","type":{"tmpfs":{}}},` +
		`{"destination":"/var/lib/postgresql","type":{"volume":{"name":"` +
		cfg.PostgresVolume + `"}}}]`
	listing := `[{"id":"` + cfg.PostgresContainer + `","configuration":{"labels":` +
		string(labels) + `,"image":{"reference":"` + cfg.PostgresImage +
		`","descriptor":{"digest":"` + image + `"}},` +
		`"networks":[{"network":"agentops-internal"}],"mounts":` + mounts +
		`,"publishedPorts":[],"publishedSockets":[]` +
		`},"status":{"state":"running"}}]`
	fake := &managerRuntimeRunner{results: []lifecycle.CommandResult{
		{Status: 0, Stdout: `{"configuration":{"descriptor":{"digest":"` + image + `"}}}`},
		{Status: 0, Stdout: listing},
	}}
	subject := newManager(cfg, lifecycle.NewAppleRuntimeForTest(fake))

	_, err = subject.ensurePostgres(context.Background())
	if !errors.Is(err, lifecycle.ErrConflictingLabels) {
		t.Fatalf("ensurePostgres did not fail closed on a partial migration: %v", err)
	}
	// The drift remediation is drain, stop, and volume-preserving restart. A
	// partially migrated container is not drifting, and following that advice
	// deletes and recreates a container whose named volume is still attached.
	if strings.Contains(err.Error(), "DRAINING") ||
		strings.Contains(err.Error(), "drifted") {
		t.Fatalf("a partial migration was reported as drift: %v", err)
	}
	for _, args := range fake.args {
		if len(args) != 0 &&
			(args[0] == "delete" || args[0] == "run" || args[0] == "kill") {
			t.Fatalf("a partially migrated PostgreSQL container was mutated: %#v", fake.args)
		}
	}
}

func TestConflictErrorsNeverEchoLabelValues(t *testing.T) {
	// A label value is accident- or attacker-supplied text that reaches operator
	// output and the durable lifecycle failure record, which only redacts
	// credentials it already knows about.
	secret := "/Users/operator/.config/agentops/auth.json"
	for name, err := range map[string]error{
		"ownership": lifecycle.RequireOwned("container agentops-runner", map[string]string{
			lifecycle.LegacyManagedLabelKey:  "v1",
			lifecycle.CurrentManagedLabelKey: secret,
		}),
		"role": lifecycle.RequireRole("agentops-runner", "runner", map[string]string{
			lifecycle.LegacyRoleLabelKey:  "runner",
			lifecycle.CurrentRoleLabelKey: secret,
		}),
		"specification digest": lifecycle.RequireSpecDigest(
			"agentops-runner",
			strings.Repeat("a", 64),
			map[string]string{
				lifecycle.LegacySpecLabelKey:  strings.Repeat("a", 64),
				lifecycle.CurrentSpecLabelKey: secret,
			},
		),
	} {
		if err == nil {
			t.Fatalf("%s conflict was accepted", name)
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("%s conflict leaked a label value: %v", name, err)
		}
		if !errors.Is(err, lifecycle.ErrConflictingLabels) {
			t.Fatalf("%s conflict is not marked as a partial migration: %v", name, err)
		}
		if !strings.Contains(err.Error(), "conflicting") {
			t.Fatalf("%s conflict does not name the problem: %v", name, err)
		}
	}
}

func TestReconciliationReadsRoleFromTheCurrentNamespaceAlone(t *testing.T) {
	// Phase 3 leaves containers carrying only `com.mrbaron3.servo.*`. The
	// reconciliation table has to exercise that shape, not a legacy role label
	// riding along with a current ownership marker.
	labels := withRole(
		map[string]string{lifecycle.CurrentManagedLabelKey: "v1"},
		"runner",
	)
	if _, legacy := labels[lifecycle.LegacyRoleLabelKey]; legacy {
		t.Fatalf("the current-only case still carries a legacy role label: %v", labels)
	}
	role, agreement := lifecycle.ReadRoleLabel(labels)
	if agreement != lifecycle.LabelCurrentOnly || role != "runner" {
		t.Fatalf("current-only role = %q, %q", role, agreement)
	}
	actual := legacyOwnedActual("agentops-runner", "runner")
	actual.Configuration.Labels = labels
	if err := validateManagedActual(
		actual, "agentops-runner", "runner", "runner:test",
		[]string{"agentops-internal"}, false,
	); err != nil {
		t.Fatalf("a current-only container was rejected: %v", err)
	}
}

func TestDrainStillStopsOwnedWorkersWhenAnotherWorkerIsConflicting(t *testing.T) {
	// Fail-closed means not touching what this binary cannot own. It must not
	// mean abandoning a worker it provably does own: DRAINING is already
	// committed, so an unsignalled runner becomes unstoppable through agentopsctl.
	cfg := testManagerConfig()
	conflicting, err := json.Marshal(map[string]string{
		lifecycle.LegacyManagedLabelKey:  "v1",
		lifecycle.CurrentManagedLabelKey: "v2",
	})
	if err != nil {
		t.Fatal(err)
	}
	owned, err := json.Marshal(withRole(
		map[string]string{lifecycle.LegacyManagedLabelKey: "v1"},
		"runner",
	))
	if err != nil {
		t.Fatal(err)
	}
	listing := `[{"id":"` + cfg.PostgresContainer + `","configuration":{"labels":` +
		`{"` + lifecycle.LegacyManagedLabelKey + `":"v1"}},"status":{"state":"running",` +
		`"networks":[{"network":"agentops-internal","ipv4Address":"192.0.2.10/24"}]}},` +
		`{"id":"` + cfg.TriageContainer + `","configuration":{"labels":` +
		string(conflicting) + `},"status":{"state":"running"}},` +
		`{"id":"` + cfg.RunnerContainer + `","configuration":{"labels":` +
		string(owned) + `},"status":{"state":"running"}}]`
	fake := &managerRuntimeRunner{results: []lifecycle.CommandResult{
		{Status: 0, Stdout: "container 1.1.0"},
		{Status: 0},
		{Status: 0, Stdout: listing},
		{Status: 0, Stdout: `{"state":{"mode":"ACTIVE","generation":1,` +
			`"updatedAt":"2026-08-10T00:00:00Z"},` +
			`"databaseTime":"2026-08-10T00:00:00Z"}`},
		{Status: 0, Stdout: listing},
		{Status: 0, Stdout: `{"state":{"mode":"DRAINING","generation":2,` +
			`"drainDeadlineAt":"2026-08-10T00:10:00Z",` +
			`"updatedAt":"2026-08-10T00:00:01Z"}}`},
		{Status: 0, Stdout: listing}, // triage lookup: conflicting
		{Status: 0, Stdout: listing}, // runner lookup: owned
		{Status: 0},                  // runner TERM
	}}
	subject := newManager(cfg, lifecycle.NewAppleRuntimeForTest(fake))

	err = subject.Drain(context.Background(), time.Minute, "drain-request-002")
	if !errors.Is(err, lifecycle.ErrConflictingLabels) {
		t.Fatalf("drain did not report the conflicting worker: %v", err)
	}
	signalled := map[string]bool{}
	for _, args := range fake.args {
		if len(args) != 0 && args[0] == "kill" {
			signalled[args[len(args)-1]] = true
		}
	}
	if signalled[cfg.TriageContainer] {
		t.Fatalf("a conflicting worker was signalled: %#v", fake.args)
	}
	if !signalled[cfg.RunnerContainer] {
		t.Fatalf("an owned worker was left running by drain: %#v", fake.args)
	}
}

func TestRoleMismatchDoesNotBlameOwnership(t *testing.T) {
	// Ownership is proven separately at every caller, so naming it in a role
	// failure sends the operator to a boundary already known to be sound.
	err := lifecycle.RequireRole("container agentops-control", "control", map[string]string{
		lifecycle.LegacyRoleLabelKey:  "runner",
		lifecycle.CurrentRoleLabelKey: "runner",
	})
	if err == nil {
		t.Fatal("a foreign role was accepted")
	}
	if strings.Contains(err.Error(), "ownership") {
		t.Fatalf("a role mismatch blamed ownership: %v", err)
	}
	if !strings.Contains(err.Error(), "role label does not match") {
		t.Fatalf("a role mismatch does not name the role: %v", err)
	}
}
