package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// This file grounds the Phase 1 label contract on a real Apple Container host.
// Headless fakes prove the classification logic but cannot prove that Apple
// Container itself stores and returns two ownership namespaces on the same
// resource — and if it dropped one, a rollback to the pre-migration binary
// would silently orphan every container this binary created, while the named
// volume stays exclusively attached to it.
//
// Run with:
//
//	AGENTOPS_TEST_APPLE_CONTAINER=1 \
//	AGENTOPS_TEST_APPLE_IMAGE=<local image reference> \
//	go test ./apps/control-plane/internal/lifecycle/ -run AppleContainer -v
//
// The test only ever touches resources whose names carry its own unique
// prefix. It never lists-and-deletes by label, so a real managed topology on
// the same host is untouched.

const (
	appleContainerTestEnv  = "AGENTOPS_TEST_APPLE_CONTAINER"
	appleContainerImageEnv = "AGENTOPS_TEST_APPLE_IMAGE"
)

func appleContainerRuntime(t *testing.T) (*AppleRuntime, string) {
	t.Helper()
	return appleContainerRuntimeForPhase(t, "labelp1")
}

// appleContainerRuntimeForPhase gates the grounded boundary and hands back a
// prefix unique to this process and phase. Every resource a grounded test
// creates carries that prefix, which is how the suite can run on a host with a
// live managed topology without ever selecting one of its resources.
func appleContainerRuntimeForPhase(
	t *testing.T,
	phase string,
) (*AppleRuntime, string) {
	t.Helper()
	if os.Getenv(appleContainerTestEnv) != "1" {
		t.Skipf("%s is not set", appleContainerTestEnv)
	}
	// Once the grounded boundary is opted into, every missing prerequisite is a
	// failure rather than a skip. A skipped case still exits 0, so a gate that
	// runs this suite would otherwise read "no proof" as "proved".
	if strings.TrimSpace(os.Getenv(appleContainerImageEnv)) == "" {
		t.Fatalf(
			"%s=1 requires %s: the live-container round trip is the proof the "+
				"rollback contract rests on",
			appleContainerTestEnv,
			appleContainerImageEnv,
		)
	}
	runtime := NewAppleRuntime()
	capability := runtime.Capability(context.Background())
	if !capability.Available || !capability.ServiceRunning {
		// Fail rather than skip, for the same reason.
		t.Fatalf(
			"%s=1 but Apple Container is unusable: %#v",
			appleContainerTestEnv,
			capability,
		)
	}
	prefix := "agentops-" + phase + "-" + strconv.Itoa(os.Getpid()) +
		"-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	return runtime, prefix
}

// appleLabels reads the labels a real Apple Container host recorded for one
// named resource. `kind` is "volume" or "network".
func appleLabels(
	t *testing.T,
	runtime *AppleRuntime,
	kind, name string,
) (map[string]string, bool) {
	t.Helper()
	result := runtime.runner.Run(
		context.Background(),
		[]string{kind, "list", "--format", "json"},
	)
	if result.Status != 0 {
		t.Fatalf("%s list failed: %s", kind, result.Stderr)
	}
	var resources []Resource
	if err := json.Unmarshal([]byte(result.Stdout), &resources); err != nil {
		t.Fatalf("parse %s list: %v", kind, err)
	}
	for _, resource := range resources {
		if resource.ID == name || resource.Configuration.Name == name {
			return resource.Configuration.Labels, true
		}
	}
	return nil, false
}

func appleContainerLabels(
	t *testing.T,
	runtime *AppleRuntime,
	name string,
) (map[string]string, bool) {
	t.Helper()
	actual, err := runtime.Container(context.Background(), name)
	if err != nil {
		t.Fatalf("list containers: %v", err)
	}
	if actual == nil {
		return nil, false
	}
	return actual.Configuration.Labels, true
}

// assertRollbackReaderStillFinds proves the binary this phase can be rolled
// back to still discovers the resource.
//
// Through Phase 2 that binary was the pre-migration one, which reads the legacy
// keys alone, so the assertion was "the legacy label is present". Phase 3A stops
// writing that namespace, and the rollback target becomes the Phase 1 or Phase 2
// binary — both of which read either namespace. The property that has to hold is
// therefore the one those readers actually evaluate: the dual reader must
// classify the resource as owned. Keeping the old assertion would pin a promise
// this phase deliberately gave up, and would fail on every resource the current
// writer creates.
func assertRollbackReaderStillFinds(t *testing.T, subject string, labels map[string]string) {
	t.Helper()
	if class := ClassifyOwnership(labels); !class.Owned() {
		t.Fatalf(
			"%s is invisible to the Phase 1/Phase 2 reader (%s): %v",
			subject, class, labels,
		)
	}
	if err := RequireOwned(subject, labels); err != nil {
		t.Fatalf("%s is not owned by the rollback reader: %v", subject, err)
	}
}

func TestAppleContainerDualWritesAndClassifiesOwnershipLabels(t *testing.T) {
	runtime, prefix := appleContainerRuntime(t)
	ctx := context.Background()
	raw := func(args ...string) CommandResult {
		return runtime.runner.Run(ctx, args)
	}

	network := prefix + "-internal"
	if err := runtime.EnsureNetwork(ctx, network); err != nil {
		t.Fatalf("create network: %v", err)
	}
	t.Cleanup(func() { raw("network", "delete", network) })

	labels, present := appleLabels(t, runtime, "network", network)
	if !present {
		t.Fatal("the created network is absent from the real runtime")
	}
	// Phase 3A writes the current namespace alone.
	if class := ClassifyOwnership(labels); class != OwnershipCurrentOnly {
		t.Fatalf("real network classifies as %q: %v", class, labels)
	}
	assertRollbackReaderStillFinds(t, "network "+network, labels)

	// Re-running the writer must accept the resource it already owns instead of
	// recreating it; recreation is what destroys an attached named volume.
	if err := runtime.EnsureNetwork(ctx, network); err != nil {
		t.Fatalf("re-running the writer rejected its own network: %v", err)
	}

	volume := prefix + "-data"
	if err := runtime.EnsureVolume(ctx, volume); err != nil {
		t.Fatalf("create volume: %v", err)
	}
	t.Cleanup(func() { raw("volume", "delete", volume) })

	labels, present = appleLabels(t, runtime, "volume", volume)
	if !present {
		t.Fatal("the created volume is absent from the real runtime")
	}
	if class := ClassifyOwnership(labels); class != OwnershipCurrentOnly {
		t.Fatalf("real volume classifies as %q: %v", class, labels)
	}
	assertRollbackReaderStillFinds(t, "volume "+volume, labels)
	if err := runtime.EnsureVolume(ctx, volume); err != nil {
		t.Fatalf("re-running the writer rejected its own volume: %v", err)
	}
}

func TestAppleContainerReadsEveryOwnershipMigrationStateFromRealVolumes(t *testing.T) {
	runtime, prefix := appleContainerRuntime(t)
	ctx := context.Background()
	raw := func(args ...string) CommandResult {
		return runtime.runner.Run(ctx, args)
	}

	for _, testCase := range []struct {
		name     string
		labels   []string
		accepted bool
		conflict bool
	}{
		{
			name:     "old-only",
			labels:   []string{LegacyManagedLabelKey + "=v1"},
			accepted: true,
		},
		{
			name:     "new-only",
			labels:   []string{CurrentManagedLabelKey + "=v1"},
			accepted: true,
		},
		{
			name: "dual-equal",
			labels: []string{
				LegacyManagedLabelKey + "=v1",
				CurrentManagedLabelKey + "=v1",
			},
			accepted: true,
		},
		{
			name: "dual-conflicting",
			labels: []string{
				LegacyManagedLabelKey + "=v1",
				CurrentManagedLabelKey + "=v2",
			},
			conflict: true,
		},
		{
			name:   "unmanaged",
			labels: []string{LegacyManagedLabelKey + "=v2"},
		},
		{
			name:   "missing-label",
			labels: []string{"com.example.other=v1"},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			name := fmt.Sprintf("%s-%s", prefix, testCase.name)
			args := []string{"volume", "create"}
			for _, label := range testCase.labels {
				args = append(args, "--label", label)
			}
			if result := raw(append(args, name)...); result.Status != 0 {
				t.Fatalf("seed volume: %s", result.Stderr)
			}
			t.Cleanup(func() { raw("volume", "delete", name) })

			seeded, present := appleLabels(t, runtime, "volume", name)
			if !present {
				t.Fatal("the seeded volume is absent from the real runtime")
			}
			err := runtime.EnsureVolume(ctx, name)
			if testCase.accepted {
				if err != nil {
					t.Fatalf("a real owned volume was rejected: %v (%v)", err, seeded)
				}
				return
			}
			assertOwnershipRejection(t, err, testCase.conflict)
		})
	}
}

func TestAppleContainerCurrentLabelsSurviveOnALiveContainerAndRollbackReader(t *testing.T) {
	runtime, prefix := appleContainerRuntime(t)
	image := strings.TrimSpace(os.Getenv(appleContainerImageEnv))
	ctx := context.Background()
	raw := func(args ...string) CommandResult {
		return runtime.runner.Run(ctx, args)
	}

	network := prefix + "-container-internal"
	if err := runtime.EnsureNetwork(ctx, network); err != nil {
		t.Fatalf("create network: %v", err)
	}
	t.Cleanup(func() { raw("network", "delete", network) })

	name := prefix + "-managed"
	digest := strings.Repeat("a", 64)
	if _, err := runtime.RunContainer(ctx, ContainerSpec{
		Name: name, Role: "runner", Image: image,
		Networks: []string{network}, Detach: true,
		SpecDigest: digest,
		Entrypoint: "/bin/sleep", Command: []string{"120"},
	}); err != nil {
		t.Fatalf("run managed container: %v", err)
	}
	t.Cleanup(func() {
		raw("stop", name)
		raw("delete", "--force", name)
	})

	labels, present := appleContainerLabels(t, runtime, name)
	if !present {
		t.Fatal("the created container is absent from the real runtime")
	}
	if class := ClassifyOwnership(labels); class != OwnershipCurrentOnly {
		t.Fatalf("real container classifies as %q: %v", class, labels)
	}
	assertRollbackReaderStillFinds(t, "container "+name, labels)
	role, agreement := ReadRoleLabel(labels)
	if agreement != LabelCurrentOnly || role != "runner" {
		t.Fatalf("real container role = %q, %q: %v", role, agreement, labels)
	}
	sealed, agreement := ReadSpecLabel(labels)
	if agreement != LabelCurrentOnly || sealed != digest {
		t.Fatalf("real container digest = %q, %q: %v", sealed, agreement, labels)
	}
	// Phase 3A's rollback target is the Phase 1 or Phase 2 binary, and what
	// those read is the dual reader in this package. Every pair it resolves on a
	// resource created by the current writer has to come back agreed and with
	// the right value, which is the whole reason the writer was allowed to move
	// before the reader.
	for _, pair := range []struct {
		kind     string
		read     func(map[string]string) (string, LabelAgreement)
		expected string
	}{
		{"role", ReadRoleLabel, "runner"},
		{"specification digest", ReadSpecLabel, digest},
	} {
		value, agreement := pair.read(labels)
		if !agreement.Agreed() || value != pair.expected {
			t.Fatalf(
				"the rollback reader cannot resolve the %s pair: %q, %q in %v",
				pair.kind, value, agreement, labels,
			)
		}
	}
	if err := RequireManaged("container "+name, labels); err != nil {
		t.Fatalf("the rollback reader does not manage this container: %v", err)
	}
	// The legacy namespace must be absent, not merely unused: a stray legacy key
	// here would mean the writer had not actually stopped emitting it.
	for key := range labels {
		if strings.HasPrefix(key, LegacyLabelNamespace) {
			t.Fatalf("the writer still emitted %s: %v", key, labels)
		}
	}

	conflicting := prefix + "-conflicting"
	if result := raw(
		"run", "--detach", "--name", conflicting,
		"--label", LegacyManagedLabelKey+"=v1",
		"--label", CurrentManagedLabelKey+"=v2",
		"--network", network,
		"--entrypoint", "/bin/sleep", image, "120",
	); result.Status != 0 {
		t.Fatalf("seed conflicting container: %s", result.Stderr)
	}
	t.Cleanup(func() {
		raw("stop", conflicting)
		raw("delete", "--force", conflicting)
	})

	err := runtime.Delete(ctx, conflicting)
	assertOwnershipRejection(t, err, true)
	if _, present := appleContainerLabels(t, runtime, conflicting); !present {
		t.Fatal("a partially migrated container was deleted from the real runtime")
	}

	if err := runtime.Stop(ctx, name, 5); err != nil {
		t.Fatalf("stop managed container: %v", err)
	}
	if err := runtime.Delete(ctx, name); err != nil {
		t.Fatalf("a dual-labelled container could not be deleted: %v", err)
	}
	if _, present := appleContainerLabels(t, runtime, name); present {
		t.Fatal("the dual-labelled container survived its own delete")
	}
}
