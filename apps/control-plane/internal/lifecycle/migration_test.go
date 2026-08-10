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
	spec, err := RebuildMigratedSpec(actual, fixtureImage())
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
		{
			// CapDropAll is one bit; a specific drop would come back as none.
			name:  "specific dropped capability cannot be restated",
			extra: `,"capDrop": ["CAP_NET_RAW"]`,
		},
		{
			name: "published protocol other than tcp cannot be restated",
			extra: `,"publishedPorts": [{
				"hostAddress": "127.0.0.1", "hostPort": 8080,
				"containerPort": 8080, "proto": "udp"
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
			if _, err := RebuildMigratedSpec(actual, fixtureImage()); err == nil {
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

// A mount the specification cannot restate must block the container. A bind
// mount reaches the host filesystem, and dropping one silently is the worst
// outcome this code could produce.
func TestRebuildRefusesMountShapesItCannotRestate(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mounts string
	}{
		{
			name: "host bind mount",
			mounts: `[{
				"destination": "/host", "source": "/Users/operator/data",
				"type": {"bind": {}}
			}]`,
		},
		{
			name: "mount option beyond read-only",
			mounts: `[{
				"destination": "/workspace", "source": "/redacted",
				"options": ["rw", "nosuid"],
				"type": {"volume": {"name": "agentops-runner-workspace"}}
			}]`,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			actual := containerFixtureWithMounts(
				t, "agentops-runner", "stopped",
				legacyOnlyLabels("runner", fixtureSpecDigest),
				testCase.mounts, "",
			)
			if _, err := RebuildMigratedSpec(actual, fixtureImage()); err == nil {
				t.Fatal("an inexpressible mount was accepted for rebuild")
			}
			if record := InventoryContainer(actual); record.Disposition !=
				MigrationBlocked {
				t.Fatalf("disposition = %q, want blocked", record.Disposition)
			}
		})
	}
}

// The writer emits only the six managed keys, so a container carrying anything
// else would come back without it — and the operator would find out after the
// original had already been deleted.
func TestRebuildRefusesAContainerCarryingUnmanagedLabels(t *testing.T) {
	actual := containerFixture(t, "agentops-runner", "stopped", `{
		"com.mrbaron3.workflow.agentopsctl": "v1",
		"com.mrbaron3.workflow.role": "runner",
		"com.example.team": "platform"
	}`, "")
	if _, err := RebuildMigratedSpec(actual, fixtureImage()); err == nil {
		t.Fatal("a container with an unreproducible label was accepted")
	}
	if record := InventoryContainer(actual); record.Disposition !=
		MigrationBlocked {
		t.Fatalf("disposition = %q, want blocked", record.Disposition)
	}
}

// Only settled states can be reproduced deliberately; a transitional one would
// otherwise be silently treated as "stopped".
func TestRebuildRefusesATransitionalLifecycleState(t *testing.T) {
	actual := containerFixture(
		t, "agentops-runner", "stopping",
		legacyOnlyLabels("runner", fixtureSpecDigest), "",
	)
	if _, err := RebuildMigratedSpec(actual, fixtureImage()); err == nil {
		t.Fatal("a container in a transitional state was accepted")
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

// The equivalence gate is the last defence against a dropped mount, so it has
// to compare the whole mount list rather than only the shapes the rebuild can
// produce.
func TestEquivalenceDetectsADroppedMount(t *testing.T) {
	before := containerFixture(
		t, "agentops-runner", "stopped",
		legacyOnlyLabels("runner", fixtureSpecDigest), "",
	)
	after := containerFixtureWithMounts(
		t, "agentops-runner", "stopped",
		dualLabels("runner", fixtureSpecDigest),
		`[{"destination": "/tmp", "source": "tmpfs", "type": {"tmpfs": {}}}]`,
		"",
	)
	if err := VerifyMigrationEquivalence(before, after); err == nil {
		t.Fatal("a replacement that lost its named volume was accepted")
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

// An operator-supplied variable with an empty value must survive. The image
// does not declare it, and a one-value map lookup would treat it as a default.
func TestRebuildKeepsAnEmptyValuedEnvironmentVariable(t *testing.T) {
	actual := containerFixture(
		t, "agentops-runner", "stopped",
		legacyOnlyLabels("runner", fixtureSpecDigest),
		`,"initProcess": {
			"executable": "/bin/run",
			"environment": ["PATH=/usr/bin", "FEATURE_FLAG="],
			"user": {"raw": {"userString": "agentops"}}
		}`,
	)
	spec, err := RebuildMigratedSpec(actual, ImageConfiguration{
		Environment: []string{"PATH=/usr/bin"},
		Entrypoint:  []string{"/bin/run"},
		WorkingDir:  "",
		User:        "agentops",
	})
	if err != nil {
		t.Fatal(err)
	}
	value, present := spec.Environment["FEATURE_FLAG"]
	if !present || value != "" {
		t.Fatalf(
			"empty-valued variable was dropped: %#v", spec.Environment,
		)
	}
	if _, inherited := spec.Environment["PATH"]; inherited {
		t.Fatal("an image default was carried onto the replacement")
	}
}

// A read-only tmpfs cannot be restated: the specification carries a tmpfs as a
// bare destination, so it would silently come back writable.
func TestRebuildRefusesAReadOnlyTmpfs(t *testing.T) {
	actual := containerFixtureWithMounts(
		t, "agentops-runner", "stopped",
		legacyOnlyLabels("runner", fixtureSpecDigest),
		`[{"destination": "/tmp", "source": "tmpfs", "options": ["ro"],
		   "type": {"tmpfs": {}}}]`, "",
	)
	if _, err := RebuildMigratedSpec(actual, fixtureImage()); err == nil {
		t.Fatal("a read-only tmpfs was accepted for rebuild")
	}
}

// Network attachment options are not restatable, so the equivalence gate is the
// only thing that can notice a replacement landing on different ones.
func TestEquivalenceDetectsNetworkAttachmentOptionDrift(t *testing.T) {
	before := containerFixture(
		t, "agentops-runner", "stopped",
		legacyOnlyLabels("runner", fixtureSpecDigest),
		`,"networks": [{"network": "agentops-internal",
			"options": {"hostname": "agentops-runner", "mtu": 1280}}]`,
	)
	after := containerFixture(
		t, "agentops-runner", "stopped",
		dualLabels("runner", fixtureSpecDigest),
		`,"networks": [{"network": "agentops-internal",
			"options": {"hostname": "agentops-runner", "mtu": 1500}}]`,
	)
	if err := VerifyMigrationEquivalence(before, after); err == nil {
		t.Fatal("a replacement on a different MTU was accepted")
	}
}

// Resources and working directory used to be compared only after the delete.
// Apple Container accepts --cpus, --memory, and --workdir, so they are restated
// instead of hoping the runtime defaults the same way twice.
func TestRebuiltSpecRestatesResourcesAndWorkingDirectory(t *testing.T) {
	actual := containerFixture(
		t, "agentops-runner", "stopped",
		legacyOnlyLabels("runner", fixtureSpecDigest), "",
	)
	spec, err := RebuildMigratedSpec(actual, fixtureImage())
	if err != nil {
		t.Fatal(err)
	}
	if spec.CPUs != 4 || spec.MemoryMiB != 1024 {
		t.Fatalf("resources were not restated: %#v", spec)
	}
	// The fixture's working directory IS the image default, so it is inherited
	// rather than restated.
	if spec.WorkingDir != "" {
		t.Fatalf("an image default was restated unnecessarily: %q",
			spec.WorkingDir)
	}
	args, _, err := containerArgs("create", spec)
	if err != nil {
		t.Fatal(err)
	}
	rendered := strings.Join(args, " ")
	if !strings.Contains(rendered, "--cpus 4") ||
		!strings.Contains(rendered, "--memory 1024MiB") {
		t.Fatalf("resource flags are absent: %s", rendered)
	}
}

// A working directory that is not the image's default has to be restated.
func TestRebuiltSpecRestatesAnOverriddenWorkingDirectory(t *testing.T) {
	actual := containerFixture(
		t, "agentops-runner", "stopped",
		legacyOnlyLabels("runner", fixtureSpecDigest),
		`,"initProcess": {
			"executable": "node", "arguments": ["dist/src/runner/cli.js"],
			"workingDirectory": "/elsewhere",
			"environment": ["PATH=/usr/bin"],
			"user": {"raw": {"userString": "agentops"}}
		}`,
	)
	spec, err := RebuildMigratedSpec(actual, fixtureImage())
	if err != nil {
		t.Fatal(err)
	}
	if spec.WorkingDir != "/elsewhere" {
		t.Fatalf("an overridden working directory was dropped: %#v", spec)
	}
}

// A process that differs from the image default and is not absolute cannot be
// restated through --entrypoint, so it has to block before the delete.
func TestRebuildRefusesARelativeOverriddenProcess(t *testing.T) {
	actual := containerFixture(
		t, "agentops-runner", "stopped",
		legacyOnlyLabels("runner", fixtureSpecDigest),
		`,"initProcess": {
			"executable": "node", "arguments": ["dist/src/other/cli.js"],
			"workingDirectory": "/app",
			"environment": ["PATH=/usr/bin"],
			"user": {"raw": {"userString": "agentops"}}
		}`,
	)
	if _, err := RebuildMigratedSpec(actual, fixtureImage()); err == nil {
		t.Fatal("a relative overridden process was accepted")
	}
}

// A network attachment carrying something the specification cannot restate has
// to stop the migration before the delete, not be compared after it.
func TestRebuildRefusesInexpressibleNetworkAttachmentOptions(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		options string
	}{
		{name: "foreign option", options: `{"mac": "02:00:00:00:00:01"}`},
		{name: "overridden hostname", options: `{"hostname": "somebody-else"}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			actual := containerFixture(
				t, "agentops-runner", "stopped",
				legacyOnlyLabels("runner", fixtureSpecDigest),
				`,"networks": [{"network": "agentops-internal",
					"options": `+testCase.options+`}]`,
			)
			if _, err := RebuildMigratedSpec(
				actual, fixtureImage(),
			); err == nil {
				t.Fatal("an inexpressible network attachment was accepted")
			}
			if record := InventoryContainer(actual); record.Disposition !=
				MigrationBlocked {
				t.Fatalf("disposition = %q, want blocked", record.Disposition)
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
	if _, err := RebuildMigratedSpec(actual, fixtureImage()); err == nil {
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

// The ownership marker alone is not the contract. A replacement carrying both
// ownership labels while keeping its role or specification digest in the legacy
// namespace only is exactly the state Phase 3 deletes — and exactly what this
// migration exists to remove — so the gate must reject it.
func TestEquivalenceRequiresEveryCarriedPairToBecomeDual(t *testing.T) {
	before := containerFixture(
		t, "agentops-runner", "stopped",
		legacyOnlyLabels("runner", fixtureSpecDigest), "",
	)
	for _, testCase := range []struct {
		name   string
		labels string
	}{
		{
			name: "role left in the legacy namespace",
			labels: `{
				"com.mrbaron3.workflow.agentopsctl": "v1",
				"com.mrbaron3.servo.agentopsctl": "v1",
				"com.mrbaron3.workflow.role": "runner",
				"com.mrbaron3.workflow.spec-sha256": "` + fixtureSpecDigest + `",
				"com.mrbaron3.servo.spec-sha256": "` + fixtureSpecDigest + `"
			}`,
		},
		{
			name: "specification digest left in the legacy namespace",
			labels: `{
				"com.mrbaron3.workflow.agentopsctl": "v1",
				"com.mrbaron3.servo.agentopsctl": "v1",
				"com.mrbaron3.workflow.role": "runner",
				"com.mrbaron3.servo.role": "runner",
				"com.mrbaron3.workflow.spec-sha256": "` + fixtureSpecDigest + `"
			}`,
		},
		{
			name: "current role written with a different value",
			labels: `{
				"com.mrbaron3.workflow.agentopsctl": "v1",
				"com.mrbaron3.servo.agentopsctl": "v1",
				"com.mrbaron3.workflow.role": "runner",
				"com.mrbaron3.servo.role": "triage",
				"com.mrbaron3.workflow.spec-sha256": "` + fixtureSpecDigest + `",
				"com.mrbaron3.servo.spec-sha256": "` + fixtureSpecDigest + `"
			}`,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			after := containerFixture(
				t, "agentops-runner", "stopped", testCase.labels, "",
			)
			if err := VerifyMigrationEquivalence(before, after); err == nil {
				t.Fatal("a half-migrated replacement passed the gate")
			}
		})
	}
	// The complete shape still passes.
	complete := containerFixture(
		t, "agentops-runner", "stopped",
		dualLabels("runner", fixtureSpecDigest), "",
	)
	if err := VerifyMigrationEquivalence(before, complete); err != nil {
		t.Fatalf("a fully migrated replacement was rejected: %v", err)
	}
}

// A container that never carried a specification digest is not required to
// gain one.
func TestEquivalenceDoesNotDemandAPairTheOriginalNeverCarried(t *testing.T) {
	before := containerFixture(t, "agentops-runner", "stopped", `{
		"com.mrbaron3.workflow.agentopsctl": "v1",
		"com.mrbaron3.workflow.role": "runner"
	}`, "")
	after := containerFixture(t, "agentops-runner", "stopped", `{
		"com.mrbaron3.workflow.agentopsctl": "v1",
		"com.mrbaron3.servo.agentopsctl": "v1",
		"com.mrbaron3.workflow.role": "runner",
		"com.mrbaron3.servo.role": "runner"
	}`, "")
	if err := VerifyMigrationEquivalence(before, after); err != nil {
		t.Fatalf("an absent pair was treated as required: %v", err)
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
