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

// The cases below pin how the lifecycle owner reads container ownership after
// Issue #123's migration completes. Phase 3B narrows the reader to
// `com.mrbaron3.servo.*`, so a container created by a pre-migration binary is no
// longer recognised — and the property that matters at every one of these call
// sites is what happens next: reconciliation refuses it rather than reusing its
// name, signalling it, or deleting it.
//
// The legacy strings are spelled literally because they are no longer constants
// anywhere in the production tree.
const (
	legacyLabelNamespace  = "com.mrbaron3.workflow"
	legacyManagedLabelKey = legacyLabelNamespace + ".agentopsctl"
	legacyRoleLabelKey    = legacyLabelNamespace + ".role"
	legacySpecLabelKey    = legacyLabelNamespace + ".spec-sha256"
)

func ownedActual(name, role string) *lifecycle.ContainerActual {
	actual := &lifecycle.ContainerActual{}
	actual.ID = name
	actual.Status.State = "running"
	actual.Configuration.Labels = map[string]string{
		lifecycle.CurrentManagedLabelKey: "v1",
		lifecycle.CurrentRoleLabelKey:    role,
	}
	actual.Configuration.Networks = []lifecycle.ContainerNetworkAttachment{
		{Network: "agentops-internal"},
	}
	actual.Configuration.Image.Reference = "runner:test"
	return actual
}

func TestValidateManagedActualAcceptsOwnershipAndRejectsForeignOrRoleDrift(t *testing.T) {
	actual := ownedActual("agentops-runner", "runner")
	if err := validateManagedActual(
		actual, "agentops-runner", "runner", "runner:test",
		[]string{"agentops-internal"}, false,
	); err != nil {
		t.Fatalf("an owned container was rejected: %v", err)
	}

	foreign := ownedActual("agentops-runner", "runner")
	delete(foreign.Configuration.Labels, lifecycle.CurrentManagedLabelKey)
	if err := validateManagedActual(
		foreign, "agentops-runner", "runner", "runner:test",
		[]string{"agentops-internal"}, false,
	); err == nil {
		t.Fatal("an unowned container name was accepted")
	}

	misrouted := ownedActual("agentops-runner", "triage")
	if err := validateManagedActual(
		misrouted, "agentops-runner", "runner", "runner:test",
		[]string{"agentops-internal"}, false,
	); err == nil {
		t.Fatal("a container with a foreign role label was accepted")
	}
}

// TestValidateManagedActualRefusesALegacyOnlyContainer is the reconciliation
// side of the Phase 3B inversion. Such a container was accepted unchanged
// through Phase 3A; it is now simply not ours.
func TestValidateManagedActualRefusesALegacyOnlyContainer(t *testing.T) {
	actual := ownedActual("agentops-runner", "runner")
	actual.Configuration.Labels = map[string]string{
		legacyManagedLabelKey: "v1",
		legacyRoleLabelKey:    "runner",
	}
	err := validateManagedActual(
		actual, "agentops-runner", "runner", "runner:test",
		[]string{"agentops-internal"}, false,
	)
	if err == nil {
		t.Fatal("a legacy-only container was accepted by reconciliation")
	}
	if !strings.Contains(err.Error(), "is not owned by agentopsctl") {
		t.Fatalf("a legacy-only container was not reported as unowned: %v", err)
	}
}

func TestManagedDashboardControlRequiresOwnershipAndControlRole(t *testing.T) {
	cfg := testManagerConfig()
	actual := ownedActual(cfg.ControlContainer, "control")
	actual.Configuration.PublishedPorts = []map[string]any{{
		"hostAddress":   "127.0.0.1",
		"hostPort":      float64(cfg.ControlHostPort),
		"containerPort": float64(8080),
		"count":         float64(1),
		"proto":         "tcp",
	}}
	if err := managedDashboardControl(actual, cfg); err != nil {
		t.Fatalf("an owned control container was not recognised: %v", err)
	}

	unowned := *actual
	unowned.Configuration.Labels = map[string]string{
		legacyRoleLabelKey: "control",
	}
	if err := managedDashboardControl(&unowned, cfg); err == nil {
		t.Fatal("an unowned control container name was recognised")
	}
}

func TestValidateSpecActualReadsTheCurrentSpecDigestLabel(t *testing.T) {
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

	if err := validateSpecActual(newActual(map[string]string{
		lifecycle.CurrentSpecLabelKey: digest,
	}), spec); err != nil {
		t.Fatalf("a sealed container was rejected: %v", err)
	}
	// An obsolete legacy digest riding along is ignored, not consulted.
	if err := validateSpecActual(newActual(map[string]string{
		legacySpecLabelKey:            strings.Repeat("b", 64),
		lifecycle.CurrentSpecLabelKey: digest,
	}), spec); err != nil {
		t.Fatalf("an obsolete legacy digest blocked the current one: %v", err)
	}
	// A legacy-only digest is unreadable, so the container reads as drifted.
	if err := validateSpecActual(newActual(map[string]string{
		legacySpecLabelKey: digest,
	}), spec); err == nil {
		t.Fatal("a legacy-only specification digest was read")
	}
	if err := validateSpecActual(newActual(map[string]string{}), spec); err == nil {
		t.Fatal("a container without a specification digest label was accepted")
	}

	// A blank current digest is a half-written label, not drift, and the two have
	// opposite remediations.
	blank := validateSpecActual(newActual(map[string]string{
		lifecycle.CurrentSpecLabelKey: "",
	}), spec)
	if !errors.Is(blank, lifecycle.ErrMalformedOwnershipLabels) {
		t.Fatalf("a blank specification digest did not fail closed: %v", blank)
	}
	if strings.Contains(blank.Error(), "drifted") {
		t.Fatalf("a half-written label was reported as drift: %v", blank)
	}

	drifted := validateSpecActual(newActual(map[string]string{
		lifecycle.CurrentSpecLabelKey: strings.Repeat("b", 64),
	}), spec)
	if drifted == nil || !strings.Contains(drifted.Error(), "drifted") {
		t.Fatalf("genuine specification drift was not reported: %v", drifted)
	}
}

// ownershipLabelCases enumerates the states an ownership marker can be in on a
// live container, as the Phase 3B reader sees them.
var ownershipLabelCases = []struct {
	name      string
	labels    map[string]string
	owned     bool
	malformed bool
}{
	{
		name:   "current-only",
		labels: map[string]string{lifecycle.CurrentManagedLabelKey: "v1"},
		owned:  true,
	},
	{
		name: "dual-equal",
		labels: map[string]string{
			legacyManagedLabelKey:            "v1",
			lifecycle.CurrentManagedLabelKey: "v1",
		},
		owned: true,
	},
	{
		// Was owned through Phase 3A; is not ours now.
		name:   "legacy-only",
		labels: map[string]string{legacyManagedLabelKey: "v1"},
	},
	{
		// Read from the current key alone: the obsolete legacy v1 does not make
		// this container ours.
		name: "legacy disagrees and current is unmanaged",
		labels: map[string]string{
			legacyManagedLabelKey:            "v1",
			lifecycle.CurrentManagedLabelKey: "v2",
		},
	},
	{
		name:      "blank current marker",
		labels:    map[string]string{lifecycle.CurrentManagedLabelKey: ""},
		malformed: true,
	},
	{
		name:   "unmanaged",
		labels: map[string]string{lifecycle.CurrentManagedLabelKey: "v2"},
	},
	{
		name:   "missing-label",
		labels: map[string]string{},
	},
}

// assertOwnershipRejection checks that a rejection names the right reason: an
// incompletely labelled container must never be reported as one this binary does
// not own, because "unowned" is what tells an operator a name is somebody
// else's.
func assertOwnershipRejection(t *testing.T, err error, malformed bool) {
	t.Helper()
	if err == nil {
		t.Fatal("a container this binary does not own was accepted")
	}
	if malformed {
		if !errors.Is(err, lifecycle.ErrMalformedOwnershipLabels) {
			t.Fatalf(
				"an incompletely labelled container was not reported as such: %v",
				err,
			)
		}
		if strings.Contains(err.Error(), "is not owned by agentopsctl") {
			t.Fatalf(
				"an incompletely labelled container was reported as unowned: %v",
				err,
			)
		}
		return
	}
	if !strings.Contains(err.Error(), "is not owned by agentopsctl") {
		t.Fatalf("a foreign container was not reported as unowned: %v", err)
	}
}

// withRole adds the role label to whichever namespace the ownership marker
// occupies, so each case tests the ownership state it is named for and nothing
// else.
//
// The namespace matters here, which is easy to miss. Attaching a current role
// label to a container whose only marker is legacy would make it malformed —
// this namespace's role written without this namespace's marker is a
// half-labelled resource — and the case would then pass for the wrong reason,
// proving the fail-closed path rather than the not-ours one. A pre-migration
// container carried its role in the retired namespace, so that is what the
// legacy-only fixture gets.
func withRole(labels map[string]string, role string) map[string]string {
	merged := make(map[string]string, len(labels)+1)
	for key, value := range labels {
		merged[key] = value
	}
	if _, current := labels[lifecycle.CurrentManagedLabelKey]; current {
		merged[lifecycle.CurrentRoleLabelKey] = role
		return merged
	}
	if _, legacy := labels[legacyManagedLabelKey]; legacy {
		merged[legacyRoleLabelKey] = role
	}
	return merged
}

func TestValidateManagedActualClassifiesEveryOwnershipState(t *testing.T) {
	for _, testCase := range ownershipLabelCases {
		t.Run(testCase.name, func(t *testing.T) {
			actual := ownedActual("agentops-runner", "runner")
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
			assertOwnershipRejection(t, err, testCase.malformed)
		})
	}
}

func TestValidateManagedActualFailsClosedOnABlankRoleLabel(t *testing.T) {
	actual := ownedActual("agentops-runner", "runner")
	actual.Configuration.Labels = map[string]string{
		lifecycle.CurrentManagedLabelKey: "v1",
		lifecycle.CurrentRoleLabelKey:    "",
	}
	err := validateManagedActual(
		actual, "agentops-runner", "runner", "runner:test",
		[]string{"agentops-internal"}, false,
	)
	if err == nil {
		t.Fatal("a blank role label was accepted")
	}
	if !errors.Is(err, lifecycle.ErrMalformedOwnershipLabels) {
		t.Fatalf("a blank role label did not fail closed: %v", err)
	}

	missing := ownedActual("agentops-runner", "runner")
	missing.Configuration.Labels = map[string]string{
		lifecycle.CurrentManagedLabelKey: "v1",
	}
	if err := validateManagedActual(
		missing, "agentops-runner", "runner", "runner:test",
		[]string{"agentops-internal"}, false,
	); err == nil {
		t.Fatal("a managed container without any role label was accepted")
	}

	// An obsolete legacy role must neither satisfy nor block the current reader.
	obsolete := ownedActual("agentops-runner", "runner")
	obsolete.Configuration.Labels = map[string]string{
		lifecycle.CurrentManagedLabelKey: "v1",
		legacyRoleLabelKey:               "triage",
		lifecycle.CurrentRoleLabelKey:    "runner",
	}
	if err := validateManagedActual(
		obsolete, "agentops-runner", "runner", "runner:test",
		[]string{"agentops-internal"}, false,
	); err != nil {
		t.Fatalf("an obsolete legacy role blocked reconciliation: %v", err)
	}
}

func TestManagedDashboardControlClassifiesEveryOwnershipState(t *testing.T) {
	cfg := testManagerConfig()
	for _, testCase := range ownershipLabelCases {
		t.Run(testCase.name, func(t *testing.T) {
			actual := ownedActual(cfg.ControlContainer, "control")
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
			assertOwnershipRejection(t, err, testCase.malformed)
		})
	}

	blankRole := ownedActual(cfg.ControlContainer, "control")
	blankRole.Configuration.Labels = map[string]string{
		lifecycle.CurrentManagedLabelKey: "v1",
		lifecycle.CurrentRoleLabelKey:    "",
	}
	blankRole.Configuration.PublishedPorts = []map[string]any{{
		"hostAddress":   "127.0.0.1",
		"hostPort":      float64(cfg.ControlHostPort),
		"containerPort": float64(8080),
		"count":         float64(1),
		"proto":         "tcp",
	}}
	err := managedDashboardControl(blankRole, cfg)
	if err == nil {
		t.Fatal("a container with a blank role label was opened as the dashboard")
	}
	// An incompletely labelled control container is reachable and healthy.
	// Reporting it as "not running" would send the operator hunting for a crash.
	if !errors.Is(err, lifecycle.ErrMalformedOwnershipLabels) ||
		strings.Contains(err.Error(), "is not running") {
		t.Fatalf("incomplete labels were diagnosed as a dead Control API: %v", err)
	}
}

func TestGracefulStopAndRemoveRunnerNeverMutateUnownedContainers(t *testing.T) {
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
				assertOwnershipRejection(t, stopErr, testCase.malformed)
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
			assertOwnershipRejection(t, removeErr, testCase.malformed)
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

func TestEnsurePostgresRefusesUnownedContainersBeforeMutating(t *testing.T) {
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
			assertOwnershipRejection(t, err, testCase.malformed)
			for _, args := range fake.args {
				if len(args) != 0 &&
					(args[0] == "delete" || args[0] == "run" || args[0] == "kill") {
					t.Fatalf("an unowned PostgreSQL container reached a mutation: %#v", fake.args)
				}
			}
		})
	}
}

// The cases below are regressions carried forward from the Phase 1 review. Each
// one pins a way an incompletely labelled container could still have been
// mutated or misdiagnosed, restated against the Phase 3B contract.

func TestDrainNeverSignalsUnownedWorkers(t *testing.T) {
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
				`{"` + lifecycle.CurrentManagedLabelKey + `":"v1"}},"status":{"state":"running",` +
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
			assertOwnershipRejection(t, err, testCase.malformed)
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

func TestEnsurePostgresDoesNotOfferDriftRemediationForIncompleteLabels(t *testing.T) {
	cfg := testManagerConfig()
	image := "sha256:" + strings.Repeat("c", 64)
	labels, err := json.Marshal(map[string]string{
		lifecycle.CurrentManagedLabelKey: "v1",
		lifecycle.CurrentRoleLabelKey:    "postgres",
		// Half-written: the container is owned, but its digest label is blank.
		lifecycle.CurrentSpecLabelKey: "",
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
	if !errors.Is(err, lifecycle.ErrMalformedOwnershipLabels) {
		t.Fatalf("ensurePostgres did not fail closed on incomplete labels: %v", err)
	}
	// The drift remediation is drain, stop, and volume-preserving restart. An
	// incompletely labelled container is not drifting, and following that advice
	// deletes and recreates a container whose named volume is still attached.
	if strings.Contains(err.Error(), "DRAINING") ||
		strings.Contains(err.Error(), "drifted") {
		t.Fatalf("incomplete labels were reported as drift: %v", err)
	}
	for _, args := range fake.args {
		if len(args) != 0 &&
			(args[0] == "delete" || args[0] == "run" || args[0] == "kill") {
			t.Fatalf("an incompletely labelled PostgreSQL container was mutated: %#v", fake.args)
		}
	}
}

func TestFailClosedErrorsNeverEchoLabelValues(t *testing.T) {
	// A label value is accident- or attacker-supplied text that reaches operator
	// output and the durable lifecycle failure record, which only redacts
	// credentials it already knows about. A blank value is what makes each of
	// these fail closed, so the secret rides on a neighbouring label to prove the
	// message never reproduces a map it was handed.
	secret := "/Users/operator/.config/agentops/auth.json"
	for name, err := range map[string]error{
		"ownership": lifecycle.RequireOwned("container agentops-postgres", map[string]string{
			lifecycle.CurrentManagedLabelKey: "",
			lifecycle.CurrentRoleLabelKey:    secret,
		}),
		"role": lifecycle.RequireRole("agentops-postgres", "runner", map[string]string{
			lifecycle.CurrentManagedLabelKey: secret,
			lifecycle.CurrentRoleLabelKey:    "",
		}),
		"specification digest": lifecycle.RequireSpecDigest(
			"agentops-postgres",
			strings.Repeat("a", 64),
			map[string]string{
				lifecycle.CurrentManagedLabelKey: secret,
				lifecycle.CurrentSpecLabelKey:    "",
			},
		),
	} {
		if err == nil {
			t.Fatalf("%s was accepted", name)
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("%s leaked a label value: %v", name, err)
		}
		if !errors.Is(err, lifecycle.ErrMalformedOwnershipLabels) {
			t.Fatalf("%s is not marked as fail-closed: %v", name, err)
		}
		if !strings.Contains(err.Error(), "incomplete") {
			t.Fatalf("%s does not name the problem: %v", name, err)
		}
	}
}

func TestReconciliationReadsRoleFromTheCurrentNamespaceAlone(t *testing.T) {
	labels := withRole(
		map[string]string{lifecycle.CurrentManagedLabelKey: "v1"},
		"runner",
	)
	if _, legacy := labels[legacyRoleLabelKey]; legacy {
		t.Fatalf("the fixture carries a legacy role label: %v", labels)
	}
	role, presence := lifecycle.ReadRoleLabel(labels)
	if presence != lifecycle.LabelPresent || role != "runner" {
		t.Fatalf("current role = %q, %q", role, presence)
	}
	actual := ownedActual("agentops-runner", "runner")
	actual.Configuration.Labels = labels
	if err := validateManagedActual(
		actual, "agentops-runner", "runner", "runner:test",
		[]string{"agentops-internal"}, false,
	); err != nil {
		t.Fatalf("a current-only container was rejected: %v", err)
	}
}

func TestDrainStillStopsOwnedWorkersWhenAnotherWorkerIsUnowned(t *testing.T) {
	// Fail-closed means not touching what this binary cannot own. It must not
	// mean abandoning a worker it provably does own: DRAINING is already
	// committed, so an unsignalled runner becomes unstoppable through agentopsctl.
	cfg := testManagerConfig()
	// A worker that predates the migration: owned by nobody this binary can see.
	unowned, err := json.Marshal(map[string]string{legacyManagedLabelKey: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	owned, err := json.Marshal(withRole(
		map[string]string{lifecycle.CurrentManagedLabelKey: "v1"},
		"runner",
	))
	if err != nil {
		t.Fatal(err)
	}
	listing := `[{"id":"` + cfg.PostgresContainer + `","configuration":{"labels":` +
		`{"` + lifecycle.CurrentManagedLabelKey + `":"v1"}},"status":{"state":"running",` +
		`"networks":[{"network":"agentops-internal","ipv4Address":"192.0.2.10/24"}]}},` +
		`{"id":"` + cfg.TriageContainer + `","configuration":{"labels":` +
		string(unowned) + `},"status":{"state":"running"}},` +
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
		{Status: 0, Stdout: listing}, // triage lookup: unowned
		{Status: 0, Stdout: listing}, // runner lookup: owned
		{Status: 0},                  // runner TERM
	}}
	subject := newManager(cfg, lifecycle.NewAppleRuntimeForTest(fake))

	err = subject.Drain(context.Background(), time.Minute, "drain-request-002")
	if err == nil ||
		!strings.Contains(err.Error(), "is not owned by agentopsctl") {
		t.Fatalf("drain did not report the unowned worker: %v", err)
	}
	signalled := map[string]bool{}
	for _, args := range fake.args {
		if len(args) != 0 && args[0] == "kill" {
			signalled[args[len(args)-1]] = true
		}
	}
	if signalled[cfg.TriageContainer] {
		t.Fatalf("an unowned worker was signalled: %#v", fake.args)
	}
	if !signalled[cfg.RunnerContainer] {
		t.Fatalf("an owned worker was left running by drain: %#v", fake.args)
	}
}

func TestRoleMismatchDoesNotBlameOwnership(t *testing.T) {
	// Ownership is proven separately at every caller, so naming it in a role
	// failure sends the operator to a boundary already known to be sound.
	err := lifecycle.RequireRole("container agentops-control", "control", map[string]string{
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
