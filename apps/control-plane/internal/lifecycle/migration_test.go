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
// defaultFixtureMounts is one tmpfs and one named volume, the shape every
// managed container in this topology has.
const defaultFixtureMounts = `[
		{"destination": "/tmp", "source": "tmpfs", "type": {"tmpfs": {}}},
		{
			"destination": "/workspace",
			"source": "/Users/operator/Library/volumes/agentops-runner-workspace/volume.img",
			"type": {"volume": {"name": "agentops-runner-workspace", "format": "ext4"}}
		}
	]`

func containerFixture(t *testing.T, id, state, labels, extra string) ContainerActual {
	t.Helper()
	return containerFixtureWithMounts(
		t, id, state, labels, defaultFixtureMounts, extra,
	)
}

// containerFixtureWithMounts takes the mount list explicitly rather than
// letting a caller override it through `extra`. Repeating a key in the JSON
// does not replace the earlier value for a map field: Go's decoder merges into
// the map it already populated, so a second "mounts" array would produce a
// mount whose type was {"tmpfs":{},"volume":{...}} — a shape the real runtime
// never emits, and one that quietly satisfies checks it should fail.
func containerFixtureWithMounts(
	t *testing.T,
	id, state, labels, mounts, extra string,
) ContainerActual {
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
			"mounts": ` + mounts + `,
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

// fixtureImage is what the fixture's image declares for itself. The fixture's
// process, working directory, and PATH are all image defaults, which is what
// lets the rebuild inherit them rather than restate them.
func fixtureImage() ImageConfiguration {
	return ImageConfiguration{
		Environment: []string{"PATH=/usr/bin"},
		Entrypoint:  []string{"node", "dist/src/runner/cli.js"},
		WorkingDir:  "/app",
		User:        "agentops",
	}
}

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

// A role or specification pair that disagrees is just as partially migrated as
// a disagreeing ownership marker. Reporting it as skipped would let the Phase 3
// gate read zero conflicts with one still on the host.
func TestInventoryTreatsRoleAndSpecDisagreementAsConflicting(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		labels string
	}{
		{
			name: "role",
			labels: `{
				"com.mrbaron3.workflow.agentopsctl": "v1",
				"com.mrbaron3.servo.agentopsctl": "v1",
				"com.mrbaron3.workflow.role": "runner",
				"com.mrbaron3.servo.role": "triage"
			}`,
		},
		{
			name: "specification digest",
			labels: `{
				"com.mrbaron3.workflow.agentopsctl": "v1",
				"com.mrbaron3.servo.agentopsctl": "v1",
				"com.mrbaron3.workflow.spec-sha256": "` + fixtureSpecDigest + `",
				"com.mrbaron3.servo.spec-sha256": "deadbeef"
			}`,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			record := InventoryContainer(containerFixture(
				t, "agentops-runner", "stopped", testCase.labels, "",
			))
			if record.Disposition != MigrationConflicting {
				t.Fatalf(
					"%s disagreement reported as %q, want conflicting",
					testCase.name, record.Disposition,
				)
			}
			audit := BuildMigrationAudit("t", []ContainerActual{
				containerFixture(
					t, "agentops-runner", "stopped", testCase.labels, "",
				),
			})
			if !audit.HasConflicts() {
				t.Fatal("the audit did not surface the partial migration")
			}
		})
	}
}

// Ownership can be dual while the role or specification digest is still written
// in the legacy namespace only. Phase 3 deletes that namespace, so such a
// container is not finished and must not be reported as skipped.
func TestInventoryTreatsAOneSidedRoleOrSpecPairAsStillPending(t *testing.T) {
	record := InventoryContainer(containerFixture(t, "agentops-runner",
		"stopped", `{
			"com.mrbaron3.workflow.agentopsctl": "v1",
			"com.mrbaron3.servo.agentopsctl": "v1",
			"com.mrbaron3.workflow.role": "runner",
			"com.mrbaron3.workflow.spec-sha256": "`+fixtureSpecDigest+`"
		}`, ""))
	if record.Disposition != MigrationPending {
		t.Fatalf(
			"a container whose role is still legacy-only reported %q",
			record.Disposition,
		)
	}
}

// A container with no legacy-only pair left is finished.
func TestInventorySkipsAFullyDualLabelledContainer(t *testing.T) {
	record := InventoryContainer(containerFixture(
		t, "agentops-runner", "stopped",
		dualLabels("runner", fixtureSpecDigest), "",
	))
	if record.Disposition != MigrationSkipped {
		t.Fatalf("a finished container reported %q", record.Disposition)
	}
}
