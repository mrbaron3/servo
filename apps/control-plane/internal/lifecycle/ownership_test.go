package lifecycle

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The cases below pin the ownership contract at the end of Issue #123's
// migration from `com.mrbaron3.workflow.*` to `com.mrbaron3.servo.*`.
//
// Phase 3A stopped writing the legacy namespace. Phase 3B stops reading it, so
// the current namespace is now the sole evidence of ownership. Two consequences
// are asserted throughout: a resource carrying only the legacy namespace is
// unreadable and therefore never adopted, mutated, or deleted; and a resource
// whose current labels are incomplete fails closed rather than reading as one
// this binary does not own.
//
// The legacy strings below are spelled literally on purpose. They are no longer
// constants anywhere in the production tree, and a test that referenced a
// production constant for them could not detect the constant coming back.
const (
	legacyLabelNamespace  = "com.mrbaron3.workflow"
	legacyManagedLabelKey = legacyLabelNamespace + ".agentopsctl"
	legacyRoleLabelKey    = legacyLabelNamespace + ".role"
	legacySpecLabelKey    = legacyLabelNamespace + ".spec-sha256"
)

// labelValueSentinel is a label value chosen so that finding it in an error
// message can only mean the message echoed the value itself. It shares no
// substring with the subjects or key names these cases use.
const labelValueSentinel = "zzz-label-value-must-not-leak-zzz"

// TestNoProductionCodeReferencesTheLegacyNamespace is the structural half of
// Phase 3B. Every other case here proves a behaviour; this one proves the
// namespace is not reachable at all, so a future edit cannot reintroduce a read
// through a constant no behavioural test happens to cover.
//
// It scans every tracked file that is not obviously binary rather than an
// allowlist of extensions. An allowlist missed exactly the file most able to
// write a container label: `deploy/Containerfile` has no extension and carries a
// real LABEL directive, and so do `githooks/*` and the `.toml`/`.md` files
// outside `docs/`.
//
// In Go it checks each string literal AND the concatenation of every literal in
// the file, so a key split across `"com.mrbaron3." + "workflow.agentopsctl"` is
// caught too. Go comments are exempt: this phase is a compatibility boundary,
// and a boundary nobody may describe is one the next reader has to rediscover.
//
// Three directories are excluded by name and each for its own reason:
// `evidence/` is the migration's own audit trail, `docs/` is where the
// compatibility history is deliberately written down, and `_test.go` files pin
// the behaviour of resources that still carry the retired namespace.
func TestNoProductionCodeReferencesTheLegacyNamespace(t *testing.T) {
	root := repositoryRootForTest(t)
	tracked, err := exec.Command(
		"git", "-C", root, "ls-files", "-z",
	).Output()
	if err != nil {
		t.Fatalf("list tracked files: %v", err)
	}
	skipDirectories := map[string]struct{}{
		"evidence": {}, "docs": {},
	}
	fileSet := token.NewFileSet()
	scannedGo, scannedText := 0, 0
	for _, relative := range strings.Split(string(tracked), "\x00") {
		if relative == "" || strings.HasSuffix(relative, "_test.go") {
			continue
		}
		if _, skip := skipDirectories[strings.SplitN(relative, "/", 2)[0]]; skip {
			continue
		}
		path := filepath.Join(root, relative)
		source, err := os.ReadFile(path)
		if err != nil {
			// A tracked path that is not a readable regular file (a submodule,
			// a symlink to nowhere) is not source this test can speak about.
			continue
		}
		// "Not obviously binary" rather than an extension allowlist: a NUL byte
		// is the same signal git itself uses.
		if bytes.IndexByte(source, 0) >= 0 {
			continue
		}
		if filepath.Ext(path) == ".go" {
			parsed, err := parser.ParseFile(fileSet, path, source, 0)
			if err != nil {
				t.Fatalf("%s: %v", relative, err)
			}
			scannedGo++
			var joined strings.Builder
			ast.Inspect(parsed, func(node ast.Node) bool {
				literal, ok := node.(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					return true
				}
				if unquoted, err := strconv.Unquote(literal.Value); err == nil {
					joined.WriteString(unquoted)
				} else {
					joined.WriteString(literal.Value)
				}
				if strings.Contains(literal.Value, legacyLabelNamespace) {
					t.Errorf(
						"%s:%d has the string literal %s, which names the "+
							"retired namespace",
						relative, fileSet.Position(literal.Pos()).Line,
						literal.Value,
					)
				}
				return true
			})
			// A key assembled from adjacent literals is still a reference.
			if strings.Contains(joined.String(), legacyLabelNamespace) {
				t.Errorf(
					"%s builds the retired namespace out of separate string "+
						"literals", relative,
				)
			}
			continue
		}
		scannedText++
		if strings.Contains(string(source), legacyLabelNamespace) {
			t.Errorf(
				"%s references the retired namespace %q",
				relative, legacyLabelNamespace,
			)
		}
	}
	// A scan that silently stopped reaching the tree would make this test pass
	// for the worst possible reason.
	if scannedGo == 0 || scannedText == 0 {
		t.Fatalf(
			"scanned %d Go and %d text files; the scan is not reaching the tree",
			scannedGo, scannedText,
		)
	}
}

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
	// Checking the prefix rather than the three exact keys means a fourth
	// ownership label added later cannot reintroduce the namespace unnoticed.
	if strings.Contains(rendered, legacyLabelNamespace) {
		t.Fatalf("the writer still emits the legacy namespace: %s", rendered)
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
			if strings.Contains(rendered, legacyLabelNamespace) {
				t.Fatalf(
					"%s create still writes the legacy namespace: %s",
					name, rendered,
				)
			}
		})
	}
}

// TestEnsureVolumeRefusesALegacyOnlyVolumeWithoutTouchingIt is the inversion
// Phase 3B performs, and the single most consequential one in the change. Before
// this phase a legacy-only volume read as owned and was accepted. It is now
// unreadable, and the property that matters is what happens next: the volume is
// refused, and no create, delete, or any other mutation is issued against the
// name. Apple Container attaches a named volume exclusively, so a volume this
// binary cannot see must be left exactly where it is.
func TestEnsureVolumeRefusesALegacyOnlyVolumeWithoutTouchingIt(t *testing.T) {
	legacy := &fakeRuntimeRunner{results: []CommandResult{{
		Status: 0,
		Stdout: `[{"id":"agentops-postgres-data","configuration":{"labels":` +
			`{"` + legacyManagedLabelKey + `":"v1"}}}]`,
	}}}
	err := NewAppleRuntimeForTest(legacy).EnsureVolume(
		context.Background(),
		"agentops-postgres-data",
	)
	if err == nil {
		t.Fatal("a legacy-only volume was adopted after the reader was narrowed")
	}
	if !strings.Contains(err.Error(), "is not owned by agentopsctl") {
		t.Fatalf("a legacy-only volume was not reported as unowned: %v", err)
	}
	// One call: the listing. Nothing was created and nothing was removed.
	if len(legacy.args) != 1 {
		t.Fatalf("a legacy-only volume reached a mutation: %#v", legacy.args)
	}
}

func TestDeleteRefusesALegacyOnlyContainer(t *testing.T) {
	fake := &fakeRuntimeRunner{results: []CommandResult{{
		Status: 0,
		Stdout: `[{"id":"agentops-runner","configuration":{"labels":` +
			`{"` + legacyManagedLabelKey + `":"v1"}},` +
			`"status":{"state":"stopped"}}]`,
	}}}
	if err := NewAppleRuntimeForTest(fake).Delete(
		context.Background(),
		"agentops-runner",
	); err == nil {
		t.Fatal("a legacy-only container was deleted")
	}
	for _, args := range fake.args {
		if len(args) != 0 && args[0] == "delete" {
			t.Fatalf("a legacy-only container reached delete: %#v", fake.args)
		}
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
			`{"` + CurrentManagedLabelKey + `":"v1"}},` +
			`"status":{"state":"stopped"}}]`,
	}}}
	if err := NewAppleRuntimeForTest(owned).Delete(
		context.Background(),
		"agentops-runner",
	); err != nil {
		t.Fatalf("an owned container was not deleted: %v", err)
	}
	if len(owned.args) != 2 || owned.args[1][0] != "delete" {
		t.Fatalf("delete was not issued: %#v", owned.args)
	}
}

func TestClassifyOwnershipReadsTheCurrentNamespaceAlone(t *testing.T) {
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
			name:     "current-only",
			labels:   map[string]string{CurrentManagedLabelKey: "v1"},
			expected: OwnershipOwned,
			owned:    true,
		},
		{
			// The whole point of Phase 3B: a resource that used to read as owned
			// through the legacy namespace is now simply not ours.
			name:     "legacy-only is no longer owned",
			labels:   map[string]string{legacyManagedLabelKey: "v1"},
			expected: OwnershipMissingLabel,
		},
		{
			name: "legacy-only role and digest are not ours either",
			labels: map[string]string{
				legacyManagedLabelKey: "v1",
				legacyRoleLabelKey:    "runner",
				legacySpecLabelKey:    strings.Repeat("a", 64),
			},
			expected: OwnershipMissingLabel,
		},
		{
			name: "dual-equal reads as owned through the current key",
			labels: map[string]string{
				legacyManagedLabelKey:  "v1",
				CurrentManagedLabelKey: "v1",
			},
			expected: OwnershipOwned,
			owned:    true,
		},
		{
			// A dual resource is evaluated from its current labels. The obsolete
			// legacy value is not consulted even when it disagrees, which is what
			// makes the current namespace authoritative rather than merely
			// preferred.
			name: "current value wins when the obsolete legacy value disagrees",
			labels: map[string]string{
				legacyManagedLabelKey:  "v1",
				CurrentManagedLabelKey: "v2",
			},
			expected: OwnershipUnmanaged,
		},
		{
			name: "current value wins in the other direction too",
			labels: map[string]string{
				legacyManagedLabelKey:  "v2",
				CurrentManagedLabelKey: "v1",
			},
			expected: OwnershipOwned,
			owned:    true,
		},
		{
			// The old contract called this "conflicting". With one namespace it
			// is a half-written marker, and it must still fail closed rather than
			// fall through to "somebody else's name".
			name:     "blank current marker is malformed",
			labels:   map[string]string{CurrentManagedLabelKey: ""},
			expected: OwnershipMalformed,
		},
		{
			name:     "whitespace-only current marker is malformed",
			labels:   map[string]string{CurrentManagedLabelKey: "   "},
			expected: OwnershipMalformed,
		},
		{
			name: "blank current marker beside a legacy one is still malformed",
			labels: map[string]string{
				legacyManagedLabelKey:  "v1",
				CurrentManagedLabelKey: "",
			},
			expected: OwnershipMalformed,
		},
		{
			// A partially labelled resource of ours. Reporting it as
			// missing-label would make it indistinguishable from a name nobody
			// owns, which is the reading that lets the name be reused.
			name:     "current role without a marker is malformed",
			labels:   map[string]string{CurrentRoleLabelKey: "runner"},
			expected: OwnershipMalformed,
		},
		{
			name: "current digest without a marker is malformed",
			labels: map[string]string{
				CurrentSpecLabelKey: strings.Repeat("a", 64),
			},
			expected: OwnershipMalformed,
		},
		{
			name:     "unmanaged current value",
			labels:   map[string]string{CurrentManagedLabelKey: "v2"},
			expected: OwnershipUnmanaged,
		},
		{
			// An unknown marker used to win outright, which reported a
			// half-written resource as somebody else's name and dropped it out
			// of the pre-stop gate. The blank label decides first now.
			name: "unknown marker beside a blank role is malformed",
			labels: map[string]string{
				CurrentManagedLabelKey: "v2",
				CurrentRoleLabelKey:    "",
			},
			expected: OwnershipMalformed,
		},
		{
			name: "unknown marker beside a blank digest is malformed",
			labels: map[string]string{
				CurrentManagedLabelKey: "v2",
				CurrentSpecLabelKey:    "   ",
			},
			expected: OwnershipMalformed,
		},
		{
			name: "managed marker beside a blank role is malformed",
			labels: map[string]string{
				CurrentManagedLabelKey: "v1",
				CurrentRoleLabelKey:    "",
			},
			expected: OwnershipMalformed,
		},
		{
			// A legacy marker cannot rescue a blank current label either.
			name: "legacy marker beside a blank current role is malformed",
			labels: map[string]string{
				legacyManagedLabelKey: "v1",
				CurrentRoleLabelKey:   "",
			},
			expected: OwnershipMalformed,
		},
		{
			// A legacy marker alongside a foreign-looking current one is read
			// entirely from the current key.
			name: "unmanaged agreement across both namespaces",
			labels: map[string]string{
				legacyManagedLabelKey:  "v2",
				CurrentManagedLabelKey: "v2",
			},
			expected: OwnershipUnmanaged,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			class := ClassifyOwnership(testCase.labels)
			if class != testCase.expected {
				t.Fatalf(
					"ClassifyOwnership() = %q, want %q",
					class, testCase.expected,
				)
			}
			if class.Owned() != testCase.owned {
				t.Fatalf(
					"%q.Owned() = %v, want %v",
					class, class.Owned(), testCase.owned,
				)
			}
		})
	}
}

func TestRequireOwnedReportsMalformedSeparatelyFromForeignOwnership(t *testing.T) {
	for name, labels := range map[string]map[string]string{
		// Every fixture carries the sentinel on a label the classifier is handed,
		// so the leak assertion below has something it could actually find. A
		// fixture without it would make that assertion unfalsifiable.
		"blank marker": {
			CurrentManagedLabelKey: "",
			CurrentRoleLabelKey:    labelValueSentinel,
		},
		"marker-less role":   {CurrentRoleLabelKey: labelValueSentinel},
		"marker-less digest": {CurrentSpecLabelKey: labelValueSentinel},
		"blank beside a legacy": {
			legacyManagedLabelKey:  labelValueSentinel,
			CurrentManagedLabelKey: "",
			CurrentSpecLabelKey:    labelValueSentinel,
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := RequireOwned("container agentops-postgres", labels)
			if err == nil {
				t.Fatal("an incompletely labelled container was accepted as owned")
			}
			if !errors.Is(err, ErrMalformedOwnershipLabels) {
				t.Fatalf("not reported as a fail-closed condition: %v", err)
			}
			if strings.Contains(err.Error(), "is not owned by agentopsctl") {
				t.Fatalf(
					"an incompletely labelled container was reported as unowned: %v",
					err,
				)
			}
			if !strings.Contains(err.Error(), CurrentManagedLabelKey) {
				t.Fatalf("the error does not name the key at fault: %v", err)
			}
			// A label value is accident- or attacker-supplied text that reaches
			// operator output and the durable lifecycle failure record, so the
			// message names keys and never values. The guard below proves the
			// fixture could detect a leak before asserting that there is none:
			// without it this assertion cannot fail, whatever the message says.
			if !fixtureCarriesSentinel(labels) {
				t.Fatalf("fixture %q cannot detect a leak: %v", name, labels)
			}
			if strings.Contains(err.Error(), labelValueSentinel) {
				t.Fatalf("the error echoes a label value: %v", err)
			}
		})
	}

	for name, labels := range map[string]map[string]string{
		"foreign current value": {CurrentManagedLabelKey: "v2"},
		"legacy-only":           {legacyManagedLabelKey: "v1"},
		"no labels at all":      {},
	} {
		t.Run(name, func(t *testing.T) {
			err := RequireOwned("container agentops-runner", labels)
			if err == nil ||
				!strings.Contains(err.Error(), "is not owned by agentopsctl") {
				t.Fatalf("not reported as unowned: %v", err)
			}
			if errors.Is(err, ErrMalformedOwnershipLabels) {
				t.Fatalf("an unowned resource was reported as malformed: %v", err)
			}
		})
	}
}

func TestReadOwnershipLabelResolvesRoleAndSpec(t *testing.T) {
	digest := strings.Repeat("a", 64)
	for _, testCase := range []struct {
		name         string
		labels       map[string]string
		role         string
		rolePresence LabelPresence
		specDigest   string
		specPresence LabelPresence
	}{
		{
			name:         "absent",
			labels:       map[string]string{},
			rolePresence: LabelAbsent,
			specPresence: LabelAbsent,
		},
		{
			name: "legacy-only reads as absent",
			labels: map[string]string{
				legacyRoleLabelKey: "runner",
				legacySpecLabelKey: digest,
			},
			rolePresence: LabelAbsent,
			specPresence: LabelAbsent,
		},
		{
			name: "current-only",
			labels: map[string]string{
				CurrentRoleLabelKey: "runner",
				CurrentSpecLabelKey: digest,
			},
			role:         "runner",
			rolePresence: LabelPresent,
			specDigest:   digest,
			specPresence: LabelPresent,
		},
		{
			name: "the current value is taken even when legacy disagrees",
			labels: map[string]string{
				legacyRoleLabelKey:  "triage",
				CurrentRoleLabelKey: "runner",
				legacySpecLabelKey:  strings.Repeat("b", 64),
				CurrentSpecLabelKey: digest,
			},
			role:         "runner",
			rolePresence: LabelPresent,
			specDigest:   digest,
			specPresence: LabelPresent,
		},
		{
			name: "blank current values are half-written, not values",
			labels: map[string]string{
				CurrentRoleLabelKey: "",
				CurrentSpecLabelKey: "  ",
			},
			rolePresence: LabelBlank,
			specPresence: LabelBlank,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			role, rolePresence := ReadRoleLabel(testCase.labels)
			if role != testCase.role || rolePresence != testCase.rolePresence {
				t.Fatalf(
					"ReadRoleLabel() = %q, %q; want %q, %q",
					role, rolePresence, testCase.role, testCase.rolePresence,
				)
			}
			sealed, specPresence := ReadSpecLabel(testCase.labels)
			if sealed != testCase.specDigest ||
				specPresence != testCase.specPresence {
				t.Fatalf(
					"ReadSpecLabel() = %q, %q; want %q, %q",
					sealed, specPresence,
					testCase.specDigest, testCase.specPresence,
				)
			}
			if rolePresence.Readable() != (testCase.rolePresence == LabelPresent) {
				t.Fatalf("%q.Readable() is inconsistent", rolePresence)
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
		legacyManagedLabelKey, legacyRoleLabelKey, legacySpecLabelKey,
	} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("legacy label %q is still written: %s", forbidden, rendered)
		}
	}
	// Writer and reader are asserted against one another: what this binary
	// creates is exactly what it can still read back as fully owned.
	labels := labelsFromArgs(args)
	if class := ClassifyOwnership(labels); class != OwnershipOwned {
		t.Fatalf("newly created container classifies as %q", class)
	}
	if err := RequireManaged("container", labels); err != nil {
		t.Fatalf("a container this binary just created is not managed: %v", err)
	}
	if err := RequireRole("container", "runner", labels); err != nil {
		t.Fatalf("role label unreadable: %v", err)
	}
	if err := RequireSpecDigest("container", digest, labels); err != nil {
		t.Fatalf("spec label unreadable: %v", err)
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

func TestEnsureNetworkAndVolumeCreateOwnedResources(t *testing.T) {
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
	); class != OwnershipOwned {
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
	); class != OwnershipOwned {
		t.Fatalf("created volume classifies as %q", class)
	}
	if volume.args[1][len(volume.args[1])-1] != "agentops-postgres-data" {
		t.Fatalf("volume name is no longer the final argument: %#v", volume.args[1])
	}
}

// TestReadersAcceptOnlyTheCurrentNamespace is the counterpart to the Phase 3A
// test that required the reader to accept every legacy shape. The requirement
// has inverted: only the current namespace proves ownership, and a resource
// carrying a complete legacy triple is refused as thoroughly as an unlabelled
// one.
func TestReadersAcceptOnlyTheCurrentNamespace(t *testing.T) {
	digest := strings.Repeat("b", 64)
	t.Run("current-only", func(t *testing.T) {
		labels := map[string]string{
			CurrentManagedLabelKey: "v1",
			CurrentRoleLabelKey:    "runner",
			CurrentSpecLabelKey:    digest,
		}
		if class := ClassifyOwnership(labels); class != OwnershipOwned {
			t.Fatalf("classified %q, want %q", class, OwnershipOwned)
		}
		if err := RequireManaged("resource", labels); err != nil {
			t.Fatalf("reader stopped owning a current-only resource: %v", err)
		}
		if err := RequireRole("resource", "runner", labels); err != nil {
			t.Fatalf("role unreadable: %v", err)
		}
		if err := RequireSpecDigest("resource", digest, labels); err != nil {
			t.Fatalf("spec digest unreadable: %v", err)
		}
	})
	t.Run("dual", func(t *testing.T) {
		// The legacy half is inert: the resource is owned because of its current
		// labels, and would be owned identically without the legacy ones.
		labels := map[string]string{
			legacyManagedLabelKey:  "v1",
			CurrentManagedLabelKey: "v1",
			legacyRoleLabelKey:     "runner",
			CurrentRoleLabelKey:    "runner",
			legacySpecLabelKey:     digest,
			CurrentSpecLabelKey:    digest,
		}
		if class := ClassifyOwnership(labels); class != OwnershipOwned {
			t.Fatalf("classified %q, want %q", class, OwnershipOwned)
		}
		if err := RequireManaged("resource", labels); err != nil {
			t.Fatalf("a dual resource is not managed: %v", err)
		}
		if err := RequireRole("resource", "runner", labels); err != nil {
			t.Fatalf("role unreadable on a dual resource: %v", err)
		}
	})
	t.Run("legacy-only", func(t *testing.T) {
		labels := map[string]string{
			legacyManagedLabelKey: "v1",
			legacyRoleLabelKey:    "runner",
			legacySpecLabelKey:    digest,
		}
		if class := ClassifyOwnership(labels); class != OwnershipMissingLabel {
			t.Fatalf("classified %q, want %q", class, OwnershipMissingLabel)
		}
		if err := RequireManaged("resource", labels); err == nil {
			t.Fatal("a legacy-only resource passed the destructive gate")
		}
		if err := RequireRole("resource", "runner", labels); err == nil {
			t.Fatal("a legacy-only role label was read")
		}
		if err := RequireSpecDigest("resource", digest, labels); err == nil {
			t.Fatal("a legacy-only specification digest was read")
		}
	})
	t.Run("legacy role disagrees with current", func(t *testing.T) {
		// Only the current value is consulted, so the stale legacy role neither
		// blocks the read nor changes its answer.
		labels := map[string]string{
			CurrentManagedLabelKey: "v1",
			legacyRoleLabelKey:     "triage",
			CurrentRoleLabelKey:    "runner",
		}
		if err := RequireRole("resource", "runner", labels); err != nil {
			t.Fatalf("an obsolete legacy role blocked the current one: %v", err)
		}
		if err := RequireManaged("resource", labels); err != nil {
			t.Fatalf("an obsolete legacy role blocked the destructive gate: %v", err)
		}
	})
}

func TestEnsureVolumeClassifiesEveryStateWithoutRecreating(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		labels    string
		accepted  bool
		malformed bool
	}{
		{
			name:     "current-only",
			labels:   `{"` + CurrentManagedLabelKey + `":"v1"}`,
			accepted: true,
		},
		{
			name: "dual-equal",
			labels: `{"` + legacyManagedLabelKey + `":"v1","` +
				CurrentManagedLabelKey + `":"v1"}`,
			accepted: true,
		},
		{
			// Current is authoritative: the obsolete legacy v1 does not rescue a
			// current value this binary does not manage.
			name: "legacy disagrees and current is unmanaged",
			labels: `{"` + legacyManagedLabelKey + `":"v1","` +
				CurrentManagedLabelKey + `":"v2"}`,
		},
		{name: "legacy-only", labels: `{"` + legacyManagedLabelKey + `":"v1"}`},
		{
			name:      "blank current marker",
			labels:    `{"` + CurrentManagedLabelKey + `":""}`,
			malformed: true,
		},
		{
			name:      "current role without a marker",
			labels:    `{"` + CurrentRoleLabelKey + `":"runner"}`,
			malformed: true,
		},
		{name: "unmanaged", labels: `{"` + CurrentManagedLabelKey + `":"v2"}`},
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
			// Whatever the reason for refusal, the existing volume is never
			// touched. This is the property that keeps PostgreSQL's data safe.
			if len(fake.args) != 1 {
				t.Fatalf("a rejected volume reached a mutation: %#v", fake.args)
			}
			assertOwnershipRejection(t, err, testCase.malformed)
		})
	}
}

func TestDeleteNeverRemovesMalformedOrUnownedContainers(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		labels  string
		deleted bool
	}{
		{
			name:    "current-only",
			labels:  `{"` + CurrentManagedLabelKey + `":"v1"}`,
			deleted: true,
		},
		{
			name: "dual-equal",
			labels: `{"` + legacyManagedLabelKey + `":"v1","` +
				CurrentManagedLabelKey + `":"v1"}`,
			deleted: true,
		},
		{name: "legacy-only", labels: `{"` + legacyManagedLabelKey + `":"v1"}`},
		{
			name: "legacy disagrees and current is unmanaged",
			labels: `{"` + legacyManagedLabelKey + `":"v1","` +
				CurrentManagedLabelKey + `":"v2"}`,
		},
		{name: "blank current marker", labels: `{"` + CurrentManagedLabelKey + `":""}`},
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

// assertOwnershipRejection checks that a rejection names the right reason: an
// incompletely labelled resource must never be reported as one this binary does
// not own, because "unowned" is what tells later logic a name is somebody
// else's.
func assertOwnershipRejection(t *testing.T, err error, malformed bool) {
	t.Helper()
	if err == nil {
		t.Fatal("a resource this binary does not own was accepted")
	}
	if malformed {
		if !errors.Is(err, ErrMalformedOwnershipLabels) {
			t.Fatalf(
				"an incompletely labelled resource was not reported as such: %v",
				err,
			)
		}
		if strings.Contains(err.Error(), "is not owned by agentopsctl") {
			t.Fatalf(
				"an incompletely labelled resource was reported as unowned: %v",
				err,
			)
		}
		return
	}
	if !strings.Contains(err.Error(), "is not owned by agentopsctl") {
		t.Fatalf("a foreign resource was not reported as unowned: %v", err)
	}
	if errors.Is(err, ErrMalformedOwnershipLabels) {
		t.Fatalf("an unowned resource was reported as malformed: %v", err)
	}
}

// The ownership marker is not the only label that can be half-written. A
// container whose role or specification label is blank is just as incomplete,
// and the destructive paths are where that has to stop the caller.

func TestRequireManagedRefusesBlankSecondaryLabels(t *testing.T) {
	owned := map[string]string{CurrentManagedLabelKey: "v1"}
	for name, key := range map[string]string{
		"role":                 CurrentRoleLabelKey,
		"specification digest": CurrentSpecLabelKey,
	} {
		t.Run(name, func(t *testing.T) {
			labels := map[string]string{CurrentManagedLabelKey: "v1", key: ""}
			// A blank ancillary label outranks the marker, so ownership itself
			// now fails closed rather than leaving the trap to RequireManaged.
			if err := RequireOwned(
				"container agentops-runner", labels,
			); !errors.Is(err, ErrMalformedOwnershipLabels) {
				t.Fatalf("a blank %s did not fail ownership closed: %v", name, err)
			}
			err := RequireManaged("container agentops-runner", labels)
			if err == nil {
				t.Fatalf("a blank %s label passed the destructive gate", name)
			}
			if !errors.Is(err, ErrMalformedOwnershipLabels) ||
				!strings.Contains(err.Error(), name) {
				t.Fatalf("a blank %s was not reported as incomplete: %v", name, err)
			}
		})
	}

	// A resource with no secondary labels at all — every network and volume — is
	// still managed once ownership is proven. Absent is not blank.
	if err := RequireManaged("volume agentops-postgres-data", owned); err != nil {
		t.Fatalf("an owned volume was refused: %v", err)
	}
}

func TestDeleteRefusesContainersWithBlankRoleOrSpecLabels(t *testing.T) {
	for name, extra := range map[string]string{
		"role":                 `"` + CurrentRoleLabelKey + `":""`,
		"specification digest": `"` + CurrentSpecLabelKey + `":"  "`,
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeRuntimeRunner{results: []CommandResult{{
				Status: 0,
				Stdout: `[{"id":"agentops-runner","configuration":{"labels":{` +
					`"` + CurrentManagedLabelKey + `":"v1",` + extra +
					`}},"status":{"state":"stopped"}}]`,
			}}}
			err := NewAppleRuntimeForTest(fake).Delete(
				context.Background(),
				"agentops-runner",
			)
			if !errors.Is(err, ErrMalformedOwnershipLabels) {
				t.Fatalf("a blank %s did not fail closed on delete: %v", name, err)
			}
			for _, args := range fake.args {
				if len(args) != 0 && args[0] == "delete" {
					t.Fatalf(
						"a container with a blank %s was deleted: %#v",
						name, fake.args,
					)
				}
			}
		})
	}
}

// fixtureCarriesSentinel reports whether a fixture actually contains the value
// the leak assertion looks for. It exists so a fixture that lost the sentinel
// fails loudly rather than making the assertion above vacuously true.
func fixtureCarriesSentinel(labels map[string]string) bool {
	for _, value := range labels {
		if value == labelValueSentinel {
			return true
		}
	}
	return false
}
