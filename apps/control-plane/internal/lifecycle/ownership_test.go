package lifecycle

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The cases below pin the ownership contract that Issue #123 migrates from
// `com.mrbaron3.workflow.*` to `com.mrbaron3.servo.*`.
//
// Phase 3A stops writing the legacy namespace: a resource this binary creates
// now carries `com.mrbaron3.servo.*` alone. The reader is deliberately left
// dual, which is what keeps the change reversible. Rolling back to the Phase 1
// or Phase 2 binary is safe because both read either namespace and therefore
// still discover a current-only resource; rolling back past Phase 1, to a
// binary that reads the legacy keys only, is not, and that is the boundary this
// phase knowingly crosses once its sweep has moved every managed resource
// forward.

const legacyManagedLabel = "com.mrbaron3.workflow.agentopsctl"

func TestRunArgsWriteOnlyTheCurrentNamespace(t *testing.T) {
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
		"--label com.mrbaron3.servo.agentopsctl=v1",
		"--label com.mrbaron3.servo.role=runner",
		"--label com.mrbaron3.servo.spec-sha256=" + strings.Repeat("a", 64),
	} {
		if !strings.Contains(rendered, expected) {
			t.Fatalf("current label %q is absent from %s", expected, rendered)
		}
	}
	// The legacy namespace must not appear at all. Checking the prefix rather
	// than the three exact keys means a fourth ownership label added later
	// cannot reintroduce the namespace unnoticed.
	if strings.Contains(rendered, LegacyLabelNamespace) {
		t.Fatalf(
			"Phase 3A still writes the legacy namespace: %s", rendered,
		)
	}
}

func TestEnsureNetworkAndVolumeWriteOnlyTheCurrentNamespaceOnCreate(t *testing.T) {
	for name, ensure := range map[string]func(*AppleRuntime) error{
		"network": func(runtime *AppleRuntime) error {
			return runtime.EnsureNetwork(
				context.Background(), "agentops-internal",
			)
		},
		"volume": func(runtime *AppleRuntime) error {
			return runtime.EnsureVolume(
				context.Background(), "agentops-postgres-data",
			)
		},
	} {
		t.Run(name, func(t *testing.T) {
			runner := &fakeRuntimeRunner{
				results: []CommandResult{{Status: 0, Stdout: `[]`}},
			}
			if err := ensure(NewAppleRuntimeForTest(runner)); err != nil {
				t.Fatal(err)
			}
			if len(runner.args) != 2 {
				t.Fatalf("unexpected %s commands: %#v", name, runner.args)
			}
			rendered := strings.Join(runner.args[1], " ")
			if !strings.Contains(
				rendered, "--label com.mrbaron3.servo.agentopsctl=v1",
			) {
				t.Fatalf(
					"%s create lost the current ownership label: %s",
					name, rendered,
				)
			}
			if strings.Contains(rendered, LegacyLabelNamespace) {
				t.Fatalf(
					"%s create still writes the legacy namespace: %s",
					name, rendered,
				)
			}
		})
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

// The remaining cases are the Phase 1 acceptance surface: every classification
// Issue #123 enumerates is explicit, conflicts fail closed instead of reading
// as unowned, and a newly created resource is discoverable through either
// namespace.

func TestClassifyOwnershipCoversEveryMigrationState(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		labels   map[string]string
		expected OwnershipClass
		owned    bool
	}{
		{
			name:     "missing-label",
			labels:   map[string]string{"com.example.other": "v1"},
			expected: OwnershipMissingLabel,
		},
		{
			name:     "nil labels are missing, not owned",
			labels:   nil,
			expected: OwnershipMissingLabel,
		},
		{
			name:     "old-only",
			labels:   map[string]string{LegacyManagedLabelKey: "v1"},
			expected: OwnershipLegacyOnly,
			owned:    true,
		},
		{
			name:     "new-only",
			labels:   map[string]string{CurrentManagedLabelKey: "v1"},
			expected: OwnershipCurrentOnly,
			owned:    true,
		},
		{
			name: "dual-equal",
			labels: map[string]string{
				LegacyManagedLabelKey:  "v1",
				CurrentManagedLabelKey: "v1",
			},
			expected: OwnershipDual,
			owned:    true,
		},
		{
			name: "dual-conflicting values",
			labels: map[string]string{
				LegacyManagedLabelKey:  "v1",
				CurrentManagedLabelKey: "v2",
			},
			expected: OwnershipConflicting,
		},
		{
			name: "half-written pair is conflicting, not owned",
			labels: map[string]string{
				LegacyManagedLabelKey:  "v1",
				CurrentManagedLabelKey: "",
			},
			expected: OwnershipConflicting,
		},
		{
			name: "conflicting stays conflicting when neither side is managed",
			labels: map[string]string{
				LegacyManagedLabelKey:  "v2",
				CurrentManagedLabelKey: "v3",
			},
			expected: OwnershipConflicting,
		},
		{
			name:     "unmanaged legacy value",
			labels:   map[string]string{LegacyManagedLabelKey: "v2"},
			expected: OwnershipUnmanaged,
		},
		{
			name:     "unmanaged current value",
			labels:   map[string]string{CurrentManagedLabelKey: "v2"},
			expected: OwnershipUnmanaged,
		},
		{
			name: "unmanaged agreement across both namespaces",
			labels: map[string]string{
				LegacyManagedLabelKey:  "v2",
				CurrentManagedLabelKey: "v2",
			},
			expected: OwnershipUnmanaged,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			class := ClassifyOwnership(testCase.labels)
			if class != testCase.expected {
				t.Fatalf("ClassifyOwnership() = %q, want %q", class, testCase.expected)
			}
			if class.Owned() != testCase.owned {
				t.Fatalf("%q.Owned() = %v, want %v", class, class.Owned(), testCase.owned)
			}
		})
	}
}

func TestRequireOwnedReportsConflictSeparatelyFromForeignOwnership(t *testing.T) {
	conflicting := map[string]string{
		LegacyManagedLabelKey:  "v1",
		CurrentManagedLabelKey: "v2",
	}
	err := RequireOwned("container agentops-runner", conflicting)
	if err == nil {
		t.Fatal("a conflicting container was accepted as owned")
	}
	if strings.Contains(err.Error(), "is not owned by agentopsctl") {
		t.Fatalf("a partially migrated container was reported as unowned: %v", err)
	}
	for _, expected := range []string{
		"conflicting ownership labels",
		LegacyManagedLabelKey,
		CurrentManagedLabelKey,
		"partial container label migration",
	} {
		if !strings.Contains(err.Error(), expected) {
			t.Fatalf("conflict error omits %q: %v", expected, err)
		}
	}

	foreign := RequireOwned(
		"container agentops-runner",
		map[string]string{LegacyManagedLabelKey: "v2"},
	)
	if foreign == nil ||
		!strings.Contains(foreign.Error(), "is not owned by agentopsctl") {
		t.Fatalf("a foreign container was not reported as unowned: %v", foreign)
	}
}

func TestReadDualLabelResolvesRoleAndSpecPairs(t *testing.T) {
	digest := strings.Repeat("a", 64)
	for _, testCase := range []struct {
		name          string
		labels        map[string]string
		role          string
		roleAgreement LabelAgreement
		specDigest    string
		specAgreement LabelAgreement
	}{
		{
			name:          "absent",
			labels:        map[string]string{},
			roleAgreement: LabelAbsent,
			specAgreement: LabelAbsent,
		},
		{
			name: "old-only",
			labels: map[string]string{
				LegacyRoleLabelKey: "runner",
				LegacySpecLabelKey: digest,
			},
			role:          "runner",
			roleAgreement: LabelLegacyOnly,
			specDigest:    digest,
			specAgreement: LabelLegacyOnly,
		},
		{
			name: "new-only",
			labels: map[string]string{
				CurrentRoleLabelKey: "runner",
				CurrentSpecLabelKey: digest,
			},
			role:          "runner",
			roleAgreement: LabelCurrentOnly,
			specDigest:    digest,
			specAgreement: LabelCurrentOnly,
		},
		{
			name: "dual-equal",
			labels: map[string]string{
				LegacyRoleLabelKey:  "runner",
				CurrentRoleLabelKey: "runner",
				LegacySpecLabelKey:  digest,
				CurrentSpecLabelKey: digest,
			},
			role:          "runner",
			roleAgreement: LabelDual,
			specDigest:    digest,
			specAgreement: LabelDual,
		},
		{
			name: "dual-conflicting",
			labels: map[string]string{
				LegacyRoleLabelKey:  "runner",
				CurrentRoleLabelKey: "triage",
				LegacySpecLabelKey:  digest,
				CurrentSpecLabelKey: strings.Repeat("b", 64),
			},
			roleAgreement: LabelConflicting,
			specAgreement: LabelConflicting,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			role, roleAgreement := ReadRoleLabel(testCase.labels)
			if role != testCase.role || roleAgreement != testCase.roleAgreement {
				t.Fatalf(
					"ReadRoleLabel() = %q, %q; want %q, %q",
					role, roleAgreement, testCase.role, testCase.roleAgreement,
				)
			}
			sealed, specAgreement := ReadSpecLabel(testCase.labels)
			if sealed != testCase.specDigest ||
				specAgreement != testCase.specAgreement {
				t.Fatalf(
					"ReadSpecLabel() = %q, %q; want %q, %q",
					sealed, specAgreement, testCase.specDigest, testCase.specAgreement,
				)
			}
			if roleAgreement.Agreed() != (testCase.roleAgreement != LabelAbsent &&
				testCase.roleAgreement != LabelConflicting) {
				t.Fatalf("%q.Agreed() is inconsistent", roleAgreement)
			}
		})
	}
}

func TestRunArgsWriteEveryOwnershipLabelInTheCurrentNamespace(t *testing.T) {
	digest := strings.Repeat("a", 64)
	args, _, err := buildContainerArgs(ContainerSpec{
		Name: "agentops-runner", Role: "runner", Image: "runner:test",
		Networks: []string{"agentops-internal"}, Detach: true,
		SpecDigest: digest,
	})
	if err != nil {
		t.Fatal(err)
	}
	rendered := strings.Join(args, " ")
	for _, expected := range []string{
		"--label " + CurrentManagedLabelKey + "=v1",
		"--label " + CurrentRoleLabelKey + "=runner",
		"--label " + CurrentSpecLabelKey + "=" + digest,
	} {
		if !strings.Contains(rendered, expected) {
			t.Fatalf("label %q is absent from %s", expected, rendered)
		}
	}
	for _, forbidden := range []string{
		LegacyManagedLabelKey, LegacyRoleLabelKey, LegacySpecLabelKey,
	} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("legacy label %q is still written: %s", forbidden, rendered)
		}
	}
	// A container created here classifies as current-only, which every reader
	// in this phase still treats as owned. That equivalence is the entire
	// reason the writer may move before the reader.
	labels := labelsFromArgs(args)
	if class := ClassifyOwnership(labels); class != OwnershipCurrentOnly {
		t.Fatalf("newly created container classifies as %q", class)
	}
	if err := RequireManaged("container", labels); err != nil {
		t.Fatalf("a container this binary just created is not managed: %v", err)
	}
	if err := RequireRole("container", "runner", labels); err != nil {
		t.Fatalf("role label unreadable after the writer change: %v", err)
	}
	if err := RequireSpecDigest("container", digest, labels); err != nil {
		t.Fatalf("spec label unreadable after the writer change: %v", err)
	}
}

// labelsFromArgs recovers the label map a runtime would record from `--label`
// arguments, so writer and reader are asserted against one another.
func labelsFromArgs(args []string) map[string]string {
	labels := make(map[string]string)
	for index := 0; index < len(args)-1; index++ {
		if args[index] != "--label" {
			continue
		}
		key, value, _ := strings.Cut(args[index+1], "=")
		labels[key] = value
	}
	return labels
}

func TestEnsureNetworkAndVolumeCreateCurrentOnlyAndReadEitherNamespace(t *testing.T) {
	network := &fakeRuntimeRunner{results: []CommandResult{{Status: 0, Stdout: `[]`}}}
	if err := NewAppleRuntimeForTest(network).EnsureNetwork(
		context.Background(),
		"agentops-internal",
	); err != nil {
		t.Fatal(err)
	}
	if len(network.args) != 2 {
		t.Fatalf("unexpected network commands: %#v", network.args)
	}
	if class := ClassifyOwnership(
		labelsFromArgs(network.args[1]),
	); class != OwnershipCurrentOnly {
		t.Fatalf("created network classifies as %q", class)
	}
	if network.args[1][len(network.args[1])-1] != "agentops-internal" {
		t.Fatalf("network name is no longer the final argument: %#v", network.args[1])
	}

	volume := &fakeRuntimeRunner{results: []CommandResult{{Status: 0, Stdout: `[]`}}}
	if err := NewAppleRuntimeForTest(volume).EnsureVolume(
		context.Background(),
		"agentops-postgres-data",
	); err != nil {
		t.Fatal(err)
	}
	if len(volume.args) != 2 {
		t.Fatalf("unexpected volume commands: %#v", volume.args)
	}
	if class := ClassifyOwnership(
		labelsFromArgs(volume.args[1]),
	); class != OwnershipCurrentOnly {
		t.Fatalf("created volume classifies as %q", class)
	}
	if volume.args[1][len(volume.args[1])-1] != "agentops-postgres-data" {
		t.Fatalf("volume name is no longer the final argument: %#v", volume.args[1])
	}
}

// TestReadersStillAcceptEveryLegacyShape is the other half of Phase 3A. The
// writer moved; the reader must not, because the host is full of resources the
// sweep has not reached yet and because a Phase 2 binary has to remain a valid
// rollback target.
func TestReadersStillAcceptEveryLegacyShape(t *testing.T) {
	digest := strings.Repeat("b", 64)
	for name, testCase := range map[string]struct {
		labels map[string]string
		class  OwnershipClass
	}{
		"legacy-only": {
			labels: map[string]string{
				LegacyManagedLabelKey: "v1",
				LegacyRoleLabelKey:    "runner",
				LegacySpecLabelKey:    digest,
			},
			class: OwnershipLegacyOnly,
		},
		"dual": {
			labels: map[string]string{
				LegacyManagedLabelKey:  "v1",
				CurrentManagedLabelKey: "v1",
				LegacyRoleLabelKey:     "runner",
				CurrentRoleLabelKey:    "runner",
				LegacySpecLabelKey:     digest,
				CurrentSpecLabelKey:    digest,
			},
			class: OwnershipDual,
		},
		"current-only": {
			labels: map[string]string{
				CurrentManagedLabelKey: "v1",
				CurrentRoleLabelKey:    "runner",
				CurrentSpecLabelKey:    digest,
			},
			class: OwnershipCurrentOnly,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if class := ClassifyOwnership(testCase.labels); class != testCase.class {
				t.Fatalf("classified %q, want %q", class, testCase.class)
			}
			if err := RequireManaged("resource", testCase.labels); err != nil {
				t.Fatalf("reader stopped owning a %s resource: %v", name, err)
			}
			if err := RequireRole(
				"resource", "runner", testCase.labels,
			); err != nil {
				t.Fatalf("role unreadable for %s: %v", name, err)
			}
			if err := RequireSpecDigest(
				"resource", digest, testCase.labels,
			); err != nil {
				t.Fatalf("spec digest unreadable for %s: %v", name, err)
			}
		})
	}
}

func TestEnsureVolumeClassifiesEveryMigrationStateWithoutRecreating(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		labels   string
		accepted bool
		conflict bool
	}{
		{name: "old-only", labels: `{"` + LegacyManagedLabelKey + `":"v1"}`, accepted: true},
		{name: "new-only", labels: `{"` + CurrentManagedLabelKey + `":"v1"}`, accepted: true},
		{
			name: "dual-equal",
			labels: `{"` + LegacyManagedLabelKey + `":"v1","` +
				CurrentManagedLabelKey + `":"v1"}`,
			accepted: true,
		},
		{
			name: "dual-conflicting",
			labels: `{"` + LegacyManagedLabelKey + `":"v1","` +
				CurrentManagedLabelKey + `":"v2"}`,
			conflict: true,
		},
		{name: "unmanaged", labels: `{"` + LegacyManagedLabelKey + `":"v2"}`},
		{name: "missing-label", labels: `{}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fake := &fakeRuntimeRunner{results: []CommandResult{{
				Status: 0,
				Stdout: `[{"id":"agentops-postgres-data","configuration":{"labels":` +
					testCase.labels + `}}]`,
			}}}
			err := NewAppleRuntimeForTest(fake).EnsureVolume(
				context.Background(),
				"agentops-postgres-data",
			)
			if testCase.accepted {
				if err != nil {
					t.Fatalf("an owned volume was rejected: %v", err)
				}
				if len(fake.args) != 1 {
					t.Fatalf("an owned volume was recreated: %#v", fake.args)
				}
				return
			}
			if err == nil {
				t.Fatal("a volume this binary does not own was accepted")
			}
			if len(fake.args) != 1 {
				t.Fatalf("a rejected volume reached a mutation: %#v", fake.args)
			}
			assertOwnershipRejection(t, err, testCase.conflict)
		})
	}
}

func TestDeleteNeverRemovesConflictingOrForeignContainers(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		labels  string
		deleted bool
	}{
		{name: "old-only", labels: `{"` + LegacyManagedLabelKey + `":"v1"}`, deleted: true},
		{name: "new-only", labels: `{"` + CurrentManagedLabelKey + `":"v1"}`, deleted: true},
		{
			name: "dual-equal",
			labels: `{"` + LegacyManagedLabelKey + `":"v1","` +
				CurrentManagedLabelKey + `":"v1"}`,
			deleted: true,
		},
		{
			name: "dual-conflicting",
			labels: `{"` + LegacyManagedLabelKey + `":"v1","` +
				CurrentManagedLabelKey + `":"v2"}`,
		},
		{name: "unmanaged", labels: `{"` + CurrentManagedLabelKey + `":"v2"}`},
		{name: "missing-label", labels: `{}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fake := &fakeRuntimeRunner{results: []CommandResult{{
				Status: 0,
				Stdout: `[{"id":"agentops-runner","configuration":{"labels":` +
					testCase.labels + `},"status":{"state":"stopped"}}]`,
			}}}
			err := NewAppleRuntimeForTest(fake).Delete(
				context.Background(),
				"agentops-runner",
			)
			issuedDelete := false
			for _, args := range fake.args {
				if len(args) != 0 && args[0] == "delete" {
					issuedDelete = true
				}
			}
			if issuedDelete != testCase.deleted {
				t.Fatalf("delete=%v for %q (err=%v)", issuedDelete, testCase.name, err)
			}
			if testCase.deleted != (err == nil) {
				t.Fatalf("unexpected error for %q: %v", testCase.name, err)
			}
		})
	}
}

// assertOwnershipRejection checks that a rejection names the right reason: a
// partially migrated resource must never be reported as one this binary does
// not own, because "unowned" is what later phases read as "somebody else's".
func assertOwnershipRejection(t *testing.T, err error, conflict bool) {
	t.Helper()
	if err == nil {
		t.Fatal("a resource this binary does not own was accepted")
	}
	if conflict {
		if !strings.Contains(err.Error(), "conflicting ownership labels") ||
			!strings.Contains(err.Error(), "partial container label migration") {
			t.Fatalf("a partially migrated resource was not reported as such: %v", err)
		}
		if strings.Contains(err.Error(), "is not owned by agentopsctl") {
			t.Fatalf("a partially migrated resource was reported as unowned: %v", err)
		}
		return
	}
	if !strings.Contains(err.Error(), "is not owned by agentopsctl") {
		t.Fatalf("a foreign resource was not reported as unowned: %v", err)
	}
}

// Ownership is not the only pair that can be half-written. A container whose
// role or specification namespaces disagree is just as partially migrated, and
// the destructive paths are where that has to stop the caller.

func TestRequireManagedRefusesSecondaryLabelConflicts(t *testing.T) {
	owned := map[string]string{
		LegacyManagedLabelKey:  "v1",
		CurrentManagedLabelKey: "v1",
	}
	for name, extra := range map[string]map[string]string{
		"role": {
			LegacyRoleLabelKey:  "runner",
			CurrentRoleLabelKey: "triage",
		},
		"specification digest": {
			LegacySpecLabelKey:  strings.Repeat("a", 64),
			CurrentSpecLabelKey: strings.Repeat("b", 64),
		},
	} {
		labels := map[string]string{}
		for key, value := range owned {
			labels[key] = value
		}
		for key, value := range extra {
			labels[key] = value
		}
		// Ownership alone still reads as owned, which is exactly the trap.
		if err := RequireOwned("container agentops-runner", labels); err != nil {
			t.Fatalf("%s fixture is not ownership-clean: %v", name, err)
		}
		err := RequireManaged("container agentops-runner", labels)
		if err == nil {
			t.Fatalf("a %s conflict passed the destructive gate", name)
		}
		if !errors.Is(err, ErrConflictingLabels) ||
			!strings.Contains(err.Error(), name) {
			t.Fatalf("%s conflict was not reported as a partial migration: %v", name, err)
		}
	}

	// A resource with no secondary pairs at all — every network and volume — is
	// still managed once ownership is proven.
	if err := RequireManaged("volume agentops-postgres-data", owned); err != nil {
		t.Fatalf("an owned volume was refused: %v", err)
	}
}

func TestDeleteRefusesContainersWithConflictingRoleOrSpecLabels(t *testing.T) {
	for name, extra := range map[string]string{
		"role": `"` + LegacyRoleLabelKey + `":"runner","` +
			CurrentRoleLabelKey + `":"triage"`,
		"specification digest": `"` + LegacySpecLabelKey + `":"` +
			strings.Repeat("a", 64) + `","` + CurrentSpecLabelKey + `":"` +
			strings.Repeat("b", 64) + `"`,
	} {
		fake := &fakeRuntimeRunner{results: []CommandResult{{
			Status: 0,
			Stdout: `[{"id":"agentops-runner","configuration":{"labels":{` +
				`"` + LegacyManagedLabelKey + `":"v1","` +
				CurrentManagedLabelKey + `":"v1",` + extra +
				`}},"status":{"state":"stopped"}}]`,
		}}}
		err := NewAppleRuntimeForTest(fake).Delete(
			context.Background(),
			"agentops-runner",
		)
		if !errors.Is(err, ErrConflictingLabels) {
			t.Fatalf("a %s conflict did not fail closed on delete: %v", name, err)
		}
		for _, args := range fake.args {
			if len(args) != 0 && args[0] == "delete" {
				t.Fatalf("a %s-conflicting container was deleted: %#v", name, fake.args)
			}
		}
	}
}
