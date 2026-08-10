package lifecycle

import (
	"encoding/json"
	"fmt"
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

func currentLabels(role, spec string) string {
	return `{
		"com.mrbaron3.servo.agentopsctl": "v1",
		"com.mrbaron3.servo.role": "` + role + `",
		"com.mrbaron3.servo.spec-sha256": "` + spec + `"
	}`
}

const fixtureSpecDigest = "05876d07396f2dbc15ab09108cdd4e69aa6d98cc4411c51289b1eba98eff7c8c"

// The inventory must reach exactly one verdict per ownership class. Its counts
// are what an operator reads to decide whether a host is in the shape the epic
// expects, so a class that silently collapsed into another would let that
// judgement be made on a lie.
func TestInventoryReachesOneVerdictPerOwnershipClass(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		labels      string
		ownership   OwnershipClass
		disposition MigrationDisposition
	}{
		{
			name:        "current-only is the post-Phase-3 shape",
			labels:      currentLabels("runner", fixtureSpecDigest),
			ownership:   OwnershipOwned,
			disposition: MigrationSkipped,
		},
		{
			name:        "dual is owned through its current labels",
			labels:      dualLabels("runner", fixtureSpecDigest),
			ownership:   OwnershipOwned,
			disposition: MigrationSkipped,
		},
		{
			// The inversion Phase 3B performs. A legacy-only container is no
			// longer a migration target, because it is no longer ours to migrate.
			name:        "legacy-only is not ours",
			labels:      legacyOnlyLabels("runner", fixtureSpecDigest),
			ownership:   OwnershipMissingLabel,
			disposition: MigrationSkipped,
		},
		{
			// Evaluated from the current label alone: the obsolete legacy v1 does
			// not make this container ours.
			name: "legacy disagrees and current is unmanaged",
			labels: `{
				"com.mrbaron3.workflow.agentopsctl": "v1",
				"com.mrbaron3.servo.agentopsctl": "v2"
			}`,
			ownership:   OwnershipUnmanaged,
			disposition: MigrationSkipped,
		},
		{
			name:        "a blank current marker fails closed",
			labels:      `{"com.mrbaron3.servo.agentopsctl": ""}`,
			ownership:   OwnershipMalformed,
			disposition: MigrationMalformed,
		},
		{
			name:        "a current role without a marker fails closed",
			labels:      `{"com.mrbaron3.servo.role": "runner"}`,
			ownership:   OwnershipMalformed,
			disposition: MigrationMalformed,
		},
		{
			name:        "unmanaged belongs to another deployment",
			labels:      `{"com.mrbaron3.servo.agentopsctl": "someone-else"}`,
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

// A malformed container must never be reported as unowned, because "unowned" is
// what tells an operator a name belongs to somebody else.
func TestInventoryNeverDemotesMalformedToUnowned(t *testing.T) {
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
	if record.Disposition != MigrationMalformed {
		t.Fatalf("a half-written ownership marker became %q", record.Disposition)
	}
	if record.Ownership.Owned() {
		t.Fatal("a malformed container must not report as owned")
	}
	if record.Ownership == OwnershipUnmanaged ||
		record.Ownership == OwnershipMissingLabel {
		t.Fatalf("a malformed container was demoted to %q", record.Ownership)
	}
}

// Evidence is durable and reviewed by people who are not the operator that took
// it. It records what the volume is called, never where the host keeps it, and
// never an environment value.
func TestInventoryRecordsVolumeIdentityWithoutHostPathsOrSecrets(t *testing.T) {
	record := InventoryContainer(containerFixture(
		t,
		"agentops-runner",
		"stopped",
		currentLabels("runner", fixtureSpecDigest),
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

// The ownership label subset published in evidence must not carry the retired
// namespace. It is derived from the keys this binary reads, so a legacy key
// reaching it would mean the reader had not actually been narrowed.
func TestInventoryPublishesNoLegacyOwnershipLabels(t *testing.T) {
	record := InventoryContainer(containerFixture(
		t, "agentops-runner", "stopped",
		dualLabels("runner", fixtureSpecDigest), "",
	))
	for key := range record.OwnershipLabels {
		if strings.HasPrefix(key, legacyLabelNamespace) {
			t.Fatalf("the inventory published %s: %v", key, record.OwnershipLabels)
		}
	}
	// The marker is published as a bounded status rather than as its literal
	// value: see boundedOwnershipValue.
	if record.OwnershipLabels[CurrentManagedLabelKey] != ManagedLabelToken {
		t.Fatalf("the inventory lost the current labels: %v", record.OwnershipLabels)
	}
}

// An ownership label is not trusted input: anyone who can create a container can
// put this namespace's keys on it with any text they like, and that text reaches
// operator terminals and durable evidence. These cases pin that no resource can
// choose what an audit trail says.
func TestInventoryNeverPublishesUntrustedLabelText(t *testing.T) {
	const secret = "/Users/operator/.config/agentops/auth.json?token=SUPERSECRET"
	for name, labels := range map[string]string{
		"foreign marker": `{"com.mrbaron3.servo.agentopsctl": "` + secret + `"}`,
		"foreign role": `{"com.mrbaron3.servo.agentopsctl": "v1",
			"com.mrbaron3.servo.role": "` + secret + `"}`,
		"foreign digest": `{"com.mrbaron3.servo.agentopsctl": "v1",
			"com.mrbaron3.servo.spec-sha256": "` + secret + `"}`,
		"role that is prose": `{"com.mrbaron3.servo.agentopsctl": "v1",
			"com.mrbaron3.servo.role": "runner OR 1=1; DROP TABLE"}`,
		"digest of the wrong shape": `{"com.mrbaron3.servo.agentopsctl": "v1",
			"com.mrbaron3.servo.spec-sha256": "NOT-A-DIGEST"}`,
	} {
		t.Run(name, func(t *testing.T) {
			record := InventoryContainer(containerFixture(
				t, "agentops-runner", "stopped", labels, "",
			))
			// The JSON form is what reaches durable evidence.
			encoded, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			// The %v form is what reaches an operator's terminal.
			for channel, rendered := range map[string]string{
				"evidence": string(encoded),
				"console":  fmt.Sprintf("%v", record),
			} {
				if strings.Contains(rendered, secret) ||
					strings.Contains(rendered, "DROP TABLE") ||
					strings.Contains(rendered, "NOT-A-DIGEST") {
					t.Fatalf(
						"%s reproduced an untrusted label value:\n%s",
						channel, rendered,
					)
				}
			}
			if !strings.Contains(string(encoded), UnrecognizedLabelToken) &&
				!strings.Contains(string(encoded), PresentLabelToken) {
				t.Fatalf(
					"the untrusted value was dropped without a marker:\n%s",
					encoded,
				)
			}
		})
	}
}

// Nothing observed is published, not even a well-formed value: the tokens carry
// the distinctions the diagnostics are used for and no bits the resource chose.
func TestInventoryPublishesTokensRatherThanObservedValues(t *testing.T) {
	record := InventoryContainer(containerFixture(
		t, "agentops-runner", "stopped",
		currentLabels("github-broker", fixtureSpecDigest), "",
	))
	labels := record.OwnershipLabels
	if labels[CurrentManagedLabelKey] != ManagedLabelToken {
		t.Fatalf("the marker published as %q", labels[CurrentManagedLabelKey])
	}
	if labels[CurrentRoleLabelKey] != PresentLabelToken {
		t.Fatalf("the role published as %q", labels[CurrentRoleLabelKey])
	}
	if labels[CurrentSpecLabelKey] != DigestShapedLabelToken {
		t.Fatalf("the digest published as %q", labels[CurrentSpecLabelKey])
	}
	// Even a value this binary itself wrote must not appear.
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	for _, observed := range []string{"github-broker", fixtureSpecDigest} {
		if strings.Contains(string(encoded), observed) {
			t.Fatalf("an observed value reached the record: %s", encoded)
		}
	}
	// The presence fields carry what the fail-closed rules turn on.
	if record.RolePresence != LabelPresent || record.SpecPresence != LabelPresent {
		t.Fatalf("presence = %q/%q", record.RolePresence, record.SpecPresence)
	}
}

// Absent and blank must stay distinguishable: the fail-closed rules turn on
// exactly that difference, so an absent label may not render as "blank".
func TestInventoryDistinguishesAbsentFromBlankInPublishedValues(t *testing.T) {
	absent := InventoryContainer(containerFixture(
		t, "agentops-runner", "stopped",
		`{"com.mrbaron3.servo.agentopsctl": "v1"}`, "",
	))
	if absent.RolePresence != LabelAbsent {
		t.Fatalf("an absent role published as %q", absent.RolePresence)
	}
	if _, published := absent.OwnershipLabels[CurrentRoleLabelKey]; published {
		t.Fatalf("an absent role reached the published labels: %v", absent.OwnershipLabels)
	}

	blank := InventoryContainer(containerFixture(
		t, "agentops-runner", "stopped",
		`{"com.mrbaron3.servo.agentopsctl": "v1", "com.mrbaron3.servo.role": ""}`, "",
	))
	if blank.RolePresence != LabelBlank {
		t.Fatalf("a blank role published as %q", blank.RolePresence)
	}
	if blank.OwnershipLabels[CurrentRoleLabelKey] != BlankLabelToken {
		t.Fatalf("a blank role published as %q", blank.OwnershipLabels[CurrentRoleLabelKey])
	}
	// A blank ancillary label now outranks the marker: the resource is malformed.
	if blank.Ownership != OwnershipMalformed {
		t.Fatalf("a blank role classified as %q", blank.Ownership)
	}
}

// The audit has to separate its outcomes, and its totals have to agree with its
// records or the counts are read off a lie.
func TestMigrationAuditTotalsAgreeWithRecords(t *testing.T) {
	audit := BuildMigrationAudit("post-migration", []ContainerActual{
		containerFixture(t, "agentops-runner", "stopped",
			currentLabels("runner", fixtureSpecDigest), ""),
		containerFixture(t, "agentops-control", "running",
			dualLabels("control", fixtureSpecDigest), ""),
		containerFixture(t, "foreign", "stopped", `{}`, ""),
		// Legacy-only is now indistinguishable from unlabelled: both are skipped
		// because neither is ours.
		containerFixture(t, "predates-the-migration", "stopped",
			legacyOnlyLabels("runner", fixtureSpecDigest), ""),
		containerFixture(t, "half", "stopped", `{
			"com.mrbaron3.servo.agentopsctl": ""
		}`, ""),
	})
	if audit.Phase != "post-migration" {
		t.Fatalf("phase = %q", audit.Phase)
	}
	if len(audit.Records) != 5 {
		t.Fatalf("records = %d, want 5", len(audit.Records))
	}
	want := map[MigrationDisposition]int{
		MigrationSkipped:   4,
		MigrationMalformed: 1,
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
	// The Phase 2 sweep's verdicts are no longer declared as constants, so they
	// are spelled here as the wire values committed evidence carries. Nothing may
	// produce them.
	for _, unreachable := range []MigrationDisposition{
		MigrationBlocked, "pending", "migrated",
	} {
		if audit.Totals[unreachable] != 0 {
			t.Fatalf("unexpected %s total: %#v", unreachable, audit.Totals)
		}
	}
	if !audit.HasMalformed() {
		t.Fatal("an audit containing a malformed container must report it")
	}
}

// A blank role or specification digest on an otherwise owned container is as
// incomplete as a blank marker, and reporting it as skipped would hide a
// resource no destructive path may touch.
func TestInventoryTreatsABlankRoleOrSpecAsMalformed(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		labels string
	}{
		{
			name: "role",
			labels: `{
				"com.mrbaron3.servo.agentopsctl": "v1",
				"com.mrbaron3.servo.role": ""
			}`,
		},
		{
			name: "specification digest",
			labels: `{
				"com.mrbaron3.servo.agentopsctl": "v1",
				"com.mrbaron3.servo.spec-sha256": "   "
			}`,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			record := InventoryContainer(containerFixture(
				t, "agentops-runner", "stopped", testCase.labels, "",
			))
			if record.Disposition != MigrationMalformed {
				t.Fatalf(
					"a blank %s reported as %q, want malformed",
					testCase.name, record.Disposition,
				)
			}
			if record.Ownership != OwnershipMalformed {
				t.Fatalf(
					"a blank %s classified as %q, want malformed",
					testCase.name, record.Ownership,
				)
			}
			audit := BuildMigrationAudit("t", []ContainerActual{
				containerFixture(
					t, "agentops-runner", "stopped", testCase.labels, "",
				),
			})
			if !audit.HasMalformed() {
				t.Fatal("the audit did not surface the incomplete labels")
			}
		})
	}
}

// An obsolete legacy role that disagrees with the current one is not a conflict
// any more: only the current value is read, so the container is finished.
func TestInventorySkipsAContainerWhoseObsoleteLegacyLabelsDisagree(t *testing.T) {
	record := InventoryContainer(containerFixture(t, "agentops-runner",
		"stopped", `{
			"com.mrbaron3.workflow.agentopsctl": "v1",
			"com.mrbaron3.servo.agentopsctl": "v1",
			"com.mrbaron3.workflow.role": "triage",
			"com.mrbaron3.servo.role": "runner",
			"com.mrbaron3.workflow.spec-sha256": "deadbeef",
			"com.mrbaron3.servo.spec-sha256": "`+fixtureSpecDigest+`"
		}`, ""))
	if record.Disposition != MigrationSkipped {
		t.Fatalf(
			"a container with obsolete legacy labels reported %q",
			record.Disposition,
		)
	}
	if record.RolePresence != LabelPresent || record.SpecPresence != LabelPresent {
		t.Fatalf("presence = %q, %q", record.RolePresence, record.SpecPresence)
	}
}

// A container labelled entirely in the current namespace is finished.
func TestInventorySkipsAFullyLabelledContainer(t *testing.T) {
	record := InventoryContainer(containerFixture(
		t, "agentops-runner", "stopped",
		currentLabels("runner", fixtureSpecDigest), "",
	))
	if record.Disposition != MigrationSkipped {
		t.Fatalf("a finished container reported %q", record.Disposition)
	}
}
