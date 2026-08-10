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

// This file grounds the ownership contract on a real Apple Container host.
// Headless fakes prove the classification logic but cannot prove what Apple
// Container itself stores and returns for a label — and the whole contract is a
// statement about labels the runtime hands back, not about labels this process
// believes it wrote.
//
// Phase 3B narrows the reader to `com.mrbaron3.servo.*`, which makes one case
// here load-bearing in a way it was not before: a resource carrying only the
// retired namespace is now refused, and the grounded assertion is that it is
// still THERE afterwards. Apple Container attaches a named volume exclusively,
// so a volume this binary can no longer see must be left exactly where it is
// rather than treated as a free name.
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
	return appleContainerRuntimeForPhase(t, "labelp3b")
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

// assertReaderOwns proves this binary's own reader classifies a resource it
// just created as owned.
//
// Through Phase 3A this helper asserted a rollback property: that the binary
// this phase could be rolled back to would still discover the resource. Phase 3B
// gives that up deliberately. Rolling back now means restoring the retained
// private Phase 3A backups AND running a pre-Phase-3B binary, so there is no
// longer a reader-compatibility promise for this suite to pin — only the
// requirement that the current reader and the current writer agree.
func assertReaderOwns(t *testing.T, subject string, labels map[string]string) {
	t.Helper()
	if class := ClassifyOwnership(labels); class != OwnershipOwned {
		t.Fatalf("%s classifies as %q rather than owned: %v", subject, class, labels)
	}
	if err := RequireOwned(subject, labels); err != nil {
		t.Fatalf("%s is not owned by its own writer's reader: %v", subject, err)
	}
	// The retired namespace must be absent, not merely unused: a stray legacy key
	// would mean the writer had not actually stopped emitting it.
	for key := range labels {
		if strings.HasPrefix(key, legacyLabelNamespace) {
			t.Fatalf("%s still carries %s: %v", subject, key, labels)
		}
	}
}

func TestAppleContainerWritesAndClassifiesCurrentOwnershipLabels(t *testing.T) {
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
	// The writer emits the current namespace alone, and the reader accepts it.
	if class := ClassifyOwnership(labels); class != OwnershipOwned {
		t.Fatalf("real network classifies as %q: %v", class, labels)
	}
	assertReaderOwns(t, "network "+network, labels)

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
	if class := ClassifyOwnership(labels); class != OwnershipOwned {
		t.Fatalf("real volume classifies as %q: %v", class, labels)
	}
	assertReaderOwns(t, "volume "+volume, labels)
	if err := runtime.EnsureVolume(ctx, volume); err != nil {
		t.Fatalf("re-running the writer rejected its own volume: %v", err)
	}
}

func TestAppleContainerReadsEveryOwnershipStateFromRealVolumes(t *testing.T) {
	runtime, prefix := appleContainerRuntime(t)
	ctx := context.Background()
	raw := func(args ...string) CommandResult {
		return runtime.runner.Run(ctx, args)
	}

	for _, testCase := range []struct {
		name      string
		labels    []string
		accepted  bool
		malformed bool
	}{
		{
			name:     "current-only",
			labels:   []string{CurrentManagedLabelKey + "=v1"},
			accepted: true,
		},
		{
			name: "dual-equal",
			labels: []string{
				legacyManagedLabelKey + "=v1",
				CurrentManagedLabelKey + "=v1",
			},
			accepted: true,
		},
		{
			// The inversion Phase 3B performs, on a real host: this volume was
			// owned before the reader was narrowed and is not owned now.
			name:   "legacy-only",
			labels: []string{legacyManagedLabelKey + "=v1"},
		},
		{
			// Evaluated from the current label alone. The obsolete legacy v1 does
			// not rescue a current value this binary does not manage.
			name: "legacy disagrees and current is unmanaged",
			labels: []string{
				legacyManagedLabelKey + "=v1",
				CurrentManagedLabelKey + "=v2",
			},
		},
		{
			name:   "unmanaged",
			labels: []string{CurrentManagedLabelKey + "=v2"},
		},
		{
			name:   "missing-label",
			labels: []string{"com.example.other=v1"},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			name := fmt.Sprintf("%s-%s", prefix, strings.ReplaceAll(testCase.name, " ", "-"))
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
			assertOwnershipRejection(t, err, testCase.malformed)
			// The refusal must leave the volume alone. This is the property that
			// matters most for the legacy-only case: Apple Container attaches a
			// named volume exclusively, so a volume this binary can no longer read
			// must survive being refused rather than have its name reused.
			after, stillThere := appleLabels(t, runtime, "volume", name)
			if !stillThere {
				t.Fatalf("a refused volume was removed from the real runtime")
			}
			if len(after) != len(seeded) {
				t.Fatalf(
					"a refused volume's labels changed: %v -> %v", seeded, after,
				)
			}
		})
	}
}

func TestAppleContainerCurrentLabelsSurviveOnALiveContainer(t *testing.T) {
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
	assertReaderOwns(t, "container "+name, labels)
	role, presence := ReadRoleLabel(labels)
	if presence != LabelPresent || role != "runner" {
		t.Fatalf("real container role = %q, %q: %v", role, presence, labels)
	}
	sealed, presence := ReadSpecLabel(labels)
	if presence != LabelPresent || sealed != digest {
		t.Fatalf("real container digest = %q, %q: %v", sealed, presence, labels)
	}
	if err := RequireManaged("container "+name, labels); err != nil {
		t.Fatalf("the reader does not manage this container: %v", err)
	}
	if err := RequireRole("container "+name, "runner", labels); err != nil {
		t.Fatalf("the reader cannot resolve the role: %v", err)
	}
	if err := RequireSpecDigest("container "+name, digest, labels); err != nil {
		t.Fatalf("the reader cannot resolve the specification digest: %v", err)
	}

	// A container carrying only the retired namespace is what a pre-Phase-3A
	// binary would have created. It must be refused AND left running: the
	// destructive paths are exactly where an unreadable resource must not be
	// mistaken for a free name.
	legacyOnly := prefix + "-legacy-only"
	if result := raw(
		"run", "--detach", "--name", legacyOnly,
		"--label", legacyManagedLabelKey+"=v1",
		"--label", legacyRoleLabelKey+"=runner",
		"--network", network,
		"--entrypoint", "/bin/sleep", image, "120",
	); result.Status != 0 {
		t.Fatalf("seed legacy-only container: %s", result.Stderr)
	}
	t.Cleanup(func() {
		raw("stop", legacyOnly)
		raw("delete", "--force", legacyOnly)
	})

	seeded, present := appleContainerLabels(t, runtime, legacyOnly)
	if !present {
		t.Fatal("the seeded legacy-only container is absent from the real runtime")
	}
	if class := ClassifyOwnership(seeded); class != OwnershipMissingLabel {
		t.Fatalf("a real legacy-only container classifies as %q: %v", class, seeded)
	}
	assertOwnershipRejection(t, runtime.Delete(ctx, legacyOnly), false)
	if _, stillThere := appleContainerLabels(
		t, runtime, legacyOnly,
	); !stillThere {
		t.Fatal("a legacy-only container was deleted from the real runtime")
	}

	if err := runtime.Stop(ctx, name, 5); err != nil {
		t.Fatalf("stop managed container: %v", err)
	}
	if err := runtime.Delete(ctx, name); err != nil {
		t.Fatalf("an owned container could not be deleted: %v", err)
	}
	if _, present := appleContainerLabels(t, runtime, name); present {
		t.Fatal("the owned container survived its own delete")
	}
}
