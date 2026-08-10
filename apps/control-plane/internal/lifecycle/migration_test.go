package lifecycle

import (
	"encoding/json"
	"strings"
	"testing"
)

// containerFixture renders one `container list --format json` record. Tests
// decode real listing shapes rather than composing the nested anonymous structs
// by hand, so a JSON tag that stops matching Apple Container fails a test here
// instead of silently emptying a migration decision.
func containerFixture(t *testing.T, id, state, labels, extra string) ContainerActual {
	t.Helper()
	raw := `{
		"id": "` + id + `",
		"configuration": {
			"id": "` + id + `",
			"labels": ` + labels + `,
			"readOnly": true,
			"useInit": true,
			"capDrop": ["ALL"],
			"capAdd": [],
			"image": {
				"reference": "agentops-runner:dev",
				"descriptor": {"digest": "sha256:` + strings.Repeat("a", 64) + `"}
			},
			"initProcess": {
				"executable": "node",
				"arguments": ["dist/src/runner/cli.js"],
				"workingDirectory": "/app",
				"environment": ["PATH=/usr/bin", "AGENTOPS_DATABASE_URL=postgres://u:secret@db/x"],
				"user": {"raw": {"userString": "agentops"}}
			},
			"publishedPorts": [],
			"publishedSockets": [],
			"resources": {"cpus": 4, "memoryInBytes": 1073741824, "cpuOverhead": 1},
			"mounts": [
				{"destination": "/tmp", "source": "tmpfs", "type": {"tmpfs": {}}},
				{
					"destination": "/workspace",
					"source": "/Users/operator/Library/volumes/agentops-runner-workspace/volume.img",
					"type": {"volume": {"name": "agentops-runner-workspace", "format": "ext4"}}
				}
			],
			"networks": [{"network": "agentops-internal"}]
			` + extra + `
		},
		"status": {"state": "` + state + `"}
	}`
	var actual ContainerActual
	if err := json.Unmarshal([]byte(raw), &actual); err != nil {
		t.Fatalf("decode container fixture: %v", err)
	}
	return actual
}

func legacyOnlyLabels(role, spec string) string {
	return `{
		"com.mrbaron3.workflow.agentopsctl": "v1",
		"com.mrbaron3.workflow.role": "` + role + `",
		"com.mrbaron3.workflow.spec-sha256": "` + spec + `"
	}`
}

func dualLabels(role, spec string) string {
	return `{
		"com.mrbaron3.workflow.agentopsctl": "v1",
		"com.mrbaron3.workflow.role": "` + role + `",
		"com.mrbaron3.workflow.spec-sha256": "` + spec + `",
		"com.mrbaron3.servo.agentopsctl": "v1",
		"com.mrbaron3.servo.role": "` + role + `",
		"com.mrbaron3.servo.spec-sha256": "` + spec + `"
	}`
}

const fixtureSpecDigest = "05876d07396f2dbc15ab09108cdd4e69aa6d98cc4411c51289b1eba98eff7c8c"

// The sweep must reach exactly one verdict per ownership class. Phase 3 keys its
// entry gate off these counts, so a class that silently collapses into another
// would let the epic advance on an unproven inventory.
func TestInventoryReachesOneVerdictPerOwnershipClass(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		labels      string
		ownership   OwnershipClass
		disposition MigrationDisposition
	}{
		{
			name:        "legacy-only is the migration target",
			labels:      legacyOnlyLabels("runner", fixtureSpecDigest),
			ownership:   OwnershipLegacyOnly,
			disposition: MigrationPending,
		},
		{
			name:        "dual is already migrated",
			labels:      dualLabels("runner", fixtureSpecDigest),
			ownership:   OwnershipDual,
			disposition: MigrationSkipped,
		},
		{
			name:        "current-only is the post-Phase-3 shape",
			labels:      `{"com.mrbaron3.servo.agentopsctl": "v1"}`,
			ownership:   OwnershipCurrentOnly,
			disposition: MigrationSkipped,
		},
		{
			name: "conflicting stops the sweep",
			labels: `{
				"com.mrbaron3.workflow.agentopsctl": "v1",
				"com.mrbaron3.servo.agentopsctl": "v2"
			}`,
			ownership:   OwnershipConflicting,
			disposition: MigrationConflicting,
		},
		{
			name:        "unmanaged belongs to another deployment",
			labels:      `{"com.mrbaron3.workflow.agentopsctl": "someone-else"}`,
			ownership:   OwnershipUnmanaged,
			disposition: MigrationSkipped,
		},
		{
			name:        "missing-label is never claimed",
			labels:      `{}`,
			ownership:   OwnershipMissingLabel,
			disposition: MigrationSkipped,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			record := InventoryContainer(
				containerFixture(t, "agentops-runner", "stopped", testCase.labels, ""),
			)
			if record.Ownership != testCase.ownership {
				t.Fatalf(
					"ownership = %q, want %q",
					record.Ownership,
					testCase.ownership,
				)
			}
			if record.Disposition != testCase.disposition {
				t.Fatalf(
					"disposition = %q, want %q",
					record.Disposition,
					testCase.disposition,
				)
			}
			if record.Reason == "" {
				t.Fatal("every verdict must carry an operator-readable reason")
			}
		})
	}
}

// A conflicting container must never be reported as unowned, because "unowned"
// is what the sweep uses to decide a name belongs to somebody else.
func TestInventoryNeverDemotesConflictToUnowned(t *testing.T) {
	record := InventoryContainer(containerFixture(
		t,
		"agentops-postgres",
		"running",
		`{
			"com.mrbaron3.workflow.agentopsctl": "v1",
			"com.mrbaron3.servo.agentopsctl": ""
		}`,
		"",
	))
	if record.Disposition != MigrationConflicting {
		t.Fatalf("half-written ownership pair became %q", record.Disposition)
	}
	if record.Ownership.Owned() {
		t.Fatal("a conflicting container must not report as owned")
	}
}

// Evidence is durable and reviewed by people who are not the operator that ran
// the sweep. It records what the volume is called, never where the host keeps
// it, and never an environment value.
func TestInventoryRecordsVolumeIdentityWithoutHostPathsOrSecrets(t *testing.T) {
	record := InventoryContainer(containerFixture(
		t,
		"agentops-runner",
		"stopped",
		legacyOnlyLabels("runner", fixtureSpecDigest),
		"",
	))
	if len(record.NamedVolumes) != 1 {
		t.Fatalf("named volumes = %#v, want exactly one", record.NamedVolumes)
	}
	attachment := record.NamedVolumes[0]
	if attachment.Name != "agentops-runner-workspace" ||
		attachment.Destination != "/workspace" {
		t.Fatalf("named volume attachment = %#v", attachment)
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	rendered := string(encoded)
	for _, forbidden := range []string{
		"/Users/operator",
		"volume.img",
		"secret",
		"postgres://",
	} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("evidence leaked %q: %s", forbidden, rendered)
		}
	}
}

// The audit has to separate the four outcomes Issue #123 names, and its totals
// have to agree with its records or the Phase 3 gate is read off a lie.
func TestMigrationAuditTotalsAgreeWithRecords(t *testing.T) {
	audit := BuildMigrationAudit("pre-migration", []ContainerActual{
		containerFixture(t, "agentops-runner", "stopped",
			legacyOnlyLabels("runner", fixtureSpecDigest), ""),
		containerFixture(t, "agentops-control", "running",
			dualLabels("control", fixtureSpecDigest), ""),
		containerFixture(t, "foreign", "stopped", `{}`, ""),
		containerFixture(t, "half", "stopped", `{
			"com.mrbaron3.workflow.agentopsctl": "v1",
			"com.mrbaron3.servo.agentopsctl": "v2"
		}`, ""),
	})
	if audit.Phase != "pre-migration" {
		t.Fatalf("phase = %q", audit.Phase)
	}
	if len(audit.Records) != 4 {
		t.Fatalf("records = %d, want 4", len(audit.Records))
	}
	want := map[MigrationDisposition]int{
		MigrationPending:     1,
		MigrationSkipped:     2,
		MigrationConflicting: 1,
	}
	for disposition, count := range want {
		if audit.Totals[disposition] != count {
			t.Fatalf(
				"total[%s] = %d, want %d (totals=%#v)",
				disposition,
				audit.Totals[disposition],
				count,
				audit.Totals,
			)
		}
	}
	if audit.Totals[MigrationBlocked] != 0 {
		t.Fatalf("unexpected blocked total: %#v", audit.Totals)
	}
	if !audit.HasConflicts() {
		t.Fatal("an audit containing a conflict must report it")
	}
}

// The rebuilt specification is the only thing standing between a label
// migration and an accidental redeployment: every observed field has to survive
// it unchanged, and the two label namespaces have to appear on the replacement.
func TestRebuiltSpecPreservesObservedConfigurationAndDualWrites(t *testing.T) {
	actual := containerFixture(
		t,
		"agentops-runner",
		"stopped",
		legacyOnlyLabels("runner", fixtureSpecDigest),
		"",
	)
	spec, err := RebuildMigratedSpec(actual)
	if err != nil {
		t.Fatalf("rebuild rejected a faithful container: %v", err)
	}
	if spec.Name != "agentops-runner" || spec.Role != "runner" ||
		spec.Image != "agentops-runner:dev" ||
		spec.SpecDigest != fixtureSpecDigest {
		t.Fatalf("identity drifted: %#v", spec)
	}
	if !spec.ReadOnly || !spec.CapDropAll || !spec.Init ||
		spec.User != "agentops" {
		t.Fatalf("hardening drifted: %#v", spec)
	}
	if len(spec.Networks) != 1 || spec.Networks[0] != "agentops-internal" {
		t.Fatalf("networks drifted: %#v", spec.Networks)
	}
	if len(spec.Tmpfs) != 1 || spec.Tmpfs[0] != "/tmp" {
		t.Fatalf("tmpfs drifted: %#v", spec.Tmpfs)
	}
	if len(spec.Mounts) != 1 ||
		spec.Mounts[0].Volume != "agentops-runner-workspace" ||
		spec.Mounts[0].Target != "/workspace" {
		t.Fatalf("named mounts drifted: %#v", spec.Mounts)
	}
	if spec.Environment["AGENTOPS_DATABASE_URL"] !=
		"postgres://u:secret@db/x" {
		t.Fatalf("environment was not carried onto the replacement")
	}
	// The replacement must be discoverable by the pre-migration binary, which
	// reads the legacy namespace only, and by the current one.
	args, _, err := buildContainerArgs(spec)
	if err != nil {
		t.Fatalf("rebuilt spec is not runnable: %v", err)
	}
	rendered := strings.Join(args, " ")
	for _, want := range []string{
		"--label com.mrbaron3.workflow.agentopsctl=v1",
		"--label com.mrbaron3.servo.agentopsctl=v1",
		"--label com.mrbaron3.workflow.role=runner",
		"--label com.mrbaron3.servo.role=runner",
		"--label com.mrbaron3.workflow.spec-sha256=" + fixtureSpecDigest,
		"--label com.mrbaron3.servo.spec-sha256=" + fixtureSpecDigest,
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("replacement argv is missing %q: %s", want, rendered)
		}
	}
	// Secret values reach the child process environment, never argv.
	if strings.Contains(rendered, "postgres://u:secret@db/x") {
		t.Fatalf("credential leaked into argv: %s", rendered)
	}
}

// Anything the rebuilt specification cannot express has to block the container
// rather than migrate it approximately. A replacement that silently drops a
// published socket or an added capability is a downgrade, not a migration.
func TestRebuildRefusesConfigurationItCannotExpress(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		extra string
	}{
		{
			name:  "published socket has no specification field",
			extra: `,"publishedSockets": [{"hostPath": "/var/run/x.sock"}]`,
		},
		{
			name:  "added capability is restricted to volume initialization",
			extra: `,"capAdd": ["CAP_NET_ADMIN"]`,
		},
		{
			name: "non-loopback publication must never be recreated",
			extra: `,"publishedPorts": [{
				"hostAddress": "0.0.0.0", "hostPort": 8080,
				"containerPort": 8080, "proto": "tcp"
			}]`,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			actual := containerFixture(
				t,
				"agentops-runner",
				"stopped",
				legacyOnlyLabels("runner", fixtureSpecDigest),
				testCase.extra,
			)
			if _, err := RebuildMigratedSpec(actual); err == nil {
				t.Fatal("an inexpressible container was accepted for rebuild")
			}
			record := InventoryContainer(actual)
			if record.Disposition != MigrationBlocked {
				t.Fatalf(
					"inexpressible container disposition = %q, want blocked",
					record.Disposition,
				)
			}
		})
	}
}

// A container whose role or specification namespaces disagree is partially
// migrated. Rebuilding it would pick one side of a disagreement it cannot
// adjudicate.
func TestRebuildRefusesPartiallyMigratedLabelPairs(t *testing.T) {
	actual := containerFixture(t, "agentops-runner", "stopped", `{
		"com.mrbaron3.workflow.agentopsctl": "v1",
		"com.mrbaron3.workflow.role": "runner",
		"com.mrbaron3.servo.role": "triage"
	}`, "")
	if _, err := RebuildMigratedSpec(actual); err == nil {
		t.Fatal("a conflicting role pair was rebuilt")
	}
}

// After the replacement exists, the only difference from the original may be
// the ownership namespaces. Everything else is drift and has to be caught while
// the operator is still standing in front of the migration.
func TestEquivalenceAcceptsLabelGainAndRejectsEveryOtherDrift(t *testing.T) {
	before := containerFixture(
		t,
		"agentops-runner",
		"stopped",
		legacyOnlyLabels("runner", fixtureSpecDigest),
		"",
	)
	after := containerFixture(
		t,
		"agentops-runner",
		"stopped",
		dualLabels("runner", fixtureSpecDigest),
		"",
	)
	if err := VerifyMigrationEquivalence(before, after); err != nil {
		t.Fatalf("gaining the current namespace was rejected: %v", err)
	}
	for _, testCase := range []struct {
		name  string
		extra string
	}{
		{name: "image", extra: `,"image": {"reference": "other:dev"}`},
		{name: "read-only", extra: `,"readOnly": false`},
		{name: "capability drop", extra: `,"capDrop": []`},
		{name: "init", extra: `,"useInit": false`},
		{name: "resources", extra: `,"resources": {"cpus": 8}`},
		{
			name:  "entrypoint",
			extra: `,"initProcess": {"executable": "sh", "user": {}}`,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			drifted := containerFixture(
				t,
				"agentops-runner",
				"stopped",
				dualLabels("runner", fixtureSpecDigest),
				testCase.extra,
			)
			if err := VerifyMigrationEquivalence(before, drifted); err == nil {
				t.Fatalf("%s drift survived the equivalence gate", testCase.name)
			}
		})
	}
}

// Losing the legacy namespace is the Phase 3 shape. Reaching it during Phase 2
// would strand the container for the pre-migration binary, which is the exact
// rollback the epic promises to keep available.
func TestEquivalenceRejectsLosingTheLegacyNamespace(t *testing.T) {
	before := containerFixture(
		t,
		"agentops-runner",
		"stopped",
		legacyOnlyLabels("runner", fixtureSpecDigest),
		"",
	)
	after := containerFixture(
		t,
		"agentops-runner",
		"stopped",
		`{"com.mrbaron3.servo.agentopsctl": "v1",
		  "com.mrbaron3.servo.role": "runner",
		  "com.mrbaron3.servo.spec-sha256": "`+fixtureSpecDigest+`"}`,
		"",
	)
	if err := VerifyMigrationEquivalence(before, after); err == nil {
		t.Fatal("Phase 2 accepted a Phase 3 label shape")
	}
}
