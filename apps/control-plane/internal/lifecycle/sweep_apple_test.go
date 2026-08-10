package lifecycle

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// This file grounds Phase 2 on a real Apple Container host. The headless suite
// proves the state machine's ordering, but only the real runtime can prove the
// three things the epic actually rests on:
//
//   - a named volume is attached to exactly one virtual machine at a time, so a
//     replacement cannot attach it until the previous container is really gone;
//   - the data inside that volume survives the container being deleted and
//     recreated, which is the whole reason Phase 2 never deletes a volume;
//   - the migrated container is still discoverable by the pre-migration binary,
//     which reads the legacy namespace only.
//
// Run with:
//
//	AGENTOPS_TEST_APPLE_CONTAINER=1 \
//	AGENTOPS_TEST_APPLE_IMAGE=<local image with /bin/sh and /bin/sleep> \
//	go test ./apps/control-plane/internal/lifecycle/ -run AppleContainerSweep -v -count=1
//
// Every resource carries a prefix unique to this process. The sweep is scoped
// with Only to the probe container, so a live managed topology on the same host
// is never a target even though it is old-only and therefore pending.

func appleSweepProbe(
	t *testing.T,
	runtime *AppleRuntime,
	prefix, image string,
) (name, volume, network string) {
	t.Helper()
	ctx := context.Background()
	raw := func(args ...string) CommandResult {
		return runtime.runner.Run(ctx, args)
	}

	network = prefix + "-internal"
	if err := runtime.EnsureNetwork(ctx, network); err != nil {
		t.Fatalf("create probe network: %v", err)
	}
	t.Cleanup(func() { raw("network", "delete", network) })

	// The probe volume is seeded legacy-only with the raw CLI, which is exactly
	// what a pre-migration host looks like. EnsureVolume would dual-write it.
	volume = prefix + "-data"
	if result := raw(
		"volume", "create", "--label", LegacyManagedLabelKey+"=v1", volume,
	); result.Status != 0 {
		t.Fatalf("seed probe volume: %s", result.Stderr)
	}
	t.Cleanup(func() { raw("volume", "delete", volume) })

	name = prefix + "-legacy"
	if result := raw(
		"run", "--detach", "--name", name,
		"--label", LegacyManagedLabelKey+"=v1",
		"--label", LegacyRoleLabelKey+"=runner",
		"--label", LegacySpecLabelKey+"="+strings.Repeat("b", 64),
		"--network", network,
		"--volume", volume+":/data",
		"--entrypoint", "/bin/sleep", image, "900",
	); result.Status != 0 {
		t.Fatalf("seed probe container: %s", result.Stderr)
	}
	t.Cleanup(func() {
		raw("stop", name)
		raw("delete", "--force", name)
	})
	return name, volume, network
}

func appleSweepMustContainer(
	t *testing.T,
	runtime *AppleRuntime,
	name string,
) ContainerActual {
	t.Helper()
	actual, err := runtime.Container(context.Background(), name)
	if err != nil {
		t.Fatalf("list containers: %v", err)
	}
	if actual == nil {
		t.Fatalf("container %s is absent from the real runtime", name)
	}
	return *actual
}

func appleSweepWriteSentinel(
	t *testing.T,
	runtime *AppleRuntime,
	name, value string,
) {
	t.Helper()
	result := runtime.Exec(
		context.Background(), name,
		"/bin/sh", "-c", "echo "+value+" > /data/sentinel",
	)
	if result.Status != 0 {
		t.Fatalf("write sentinel into the probe volume: %s", result.Stderr)
	}
}

func appleSweepReadSentinel(
	t *testing.T,
	runtime *AppleRuntime,
	name string,
) string {
	t.Helper()
	result := runtime.Exec(
		context.Background(), name, "/bin/sh", "-c", "cat /data/sentinel",
	)
	if result.Status != 0 {
		t.Fatalf("read sentinel from the probe volume: %s", result.Stderr)
	}
	return strings.TrimSpace(result.Stdout)
}

func TestAppleContainerSweepMigratesLegacyOnlyContainerAndPreservesVolumeData(
	t *testing.T,
) {
	runtime, prefix := appleContainerRuntimeForPhase(t, "labelp2")
	image := strings.TrimSpace(os.Getenv(appleContainerImageEnv))
	ctx := context.Background()

	name, volume, _ := appleSweepProbe(t, runtime, prefix, image)
	sentinel := "sentinel-" + prefix
	appleSweepWriteSentinel(t, runtime, name, sentinel)

	before := appleSweepMustContainer(t, runtime, name)
	if record := InventoryContainer(before); record.Ownership !=
		OwnershipLegacyOnly || record.Disposition != MigrationPending {
		t.Fatalf("probe is not a legacy-only migration target: %#v", record)
	}
	if before.Status.State != "running" {
		t.Fatalf("probe did not start: state=%s", before.Status.State)
	}
	attachments := NamedVolumeAttachments(before)
	if len(attachments) != 1 || attachments[0].Name != volume {
		t.Fatalf("probe volume is not attached: %#v", attachments)
	}

	// Record what every other container on this host looks like, so the scoped
	// sweep can be proven not to have touched the live managed topology.
	untouched := appleSweepOwnershipByID(t, runtime, name)

	sweeper := NewLabelSweeper(runtime)
	sweeper.Only = []string{name}
	report, err := sweeper.Apply(ctx)
	if err != nil {
		t.Fatalf("grounded sweep failed: %v (steps=%+v)", err, report.Steps)
	}

	// Drain and recreate actually happened, in order, on the real runtime.
	var stages []SweepStage
	for _, step := range report.Steps {
		if step.Outcome != "ok" {
			t.Fatalf("grounded sweep step failed: %#v", step)
		}
		stages = append(stages, step.Stage)
	}
	wantStages := []SweepStage{
		StageReinspect, StageStop, StageDelete,
		StageVolumeRelease, StageRecreate, StageVerify,
	}
	if len(stages) != len(wantStages) {
		t.Fatalf("unexpected grounded stages: %v", stages)
	}
	for index, stage := range wantStages {
		if stages[index] != stage {
			t.Fatalf("stage %d = %q, want %q", index, stages[index], stage)
		}
	}

	// The replacement carries both namespaces on a real host, and the
	// pre-migration reader still finds it.
	after := appleSweepMustContainer(t, runtime, name)
	if class := ClassifyOwnership(after.Configuration.Labels); class !=
		OwnershipDual {
		t.Fatalf(
			"migrated container classifies as %q: %v",
			class, after.Configuration.Labels,
		)
	}
	assertLegacyReaderStillFinds(t, "container "+name, after.Configuration.Labels)
	if after.Status.State != "running" {
		t.Fatalf("a running container was not replaced running: %s",
			after.Status.State)
	}

	// The same named volume was re-attached to the replacement, and the volume
	// itself was never deleted.
	replacementAttachments := NamedVolumeAttachments(after)
	if len(replacementAttachments) != 1 ||
		replacementAttachments[0].Name != volume ||
		replacementAttachments[0].Destination != "/data" {
		t.Fatalf("replacement lost its named volume: %#v",
			replacementAttachments)
	}
	if _, present := appleLabels(t, runtime, "volume", volume); !present {
		t.Fatal("the named volume was destroyed by the migration")
	}
	for _, record := range report.Volumes {
		if record.Name == volume && !record.PresentAfter {
			t.Fatal("preservation evidence says the probe volume is gone")
		}
	}

	// The data inside the volume outlived the container replacement. This is
	// the claim that makes deleting a container acceptable at all.
	if got := appleSweepReadSentinel(t, runtime, name); got != sentinel {
		t.Fatalf("volume data did not survive migration: %q != %q",
			got, sentinel)
	}

	// Restart reconciliation: the replacement stops and starts again, keeps
	// both namespaces, and keeps its volume and its data.
	if err := runtime.Stop(ctx, name, 10); err != nil {
		t.Fatalf("stop the replacement: %v", err)
	}
	if err := runtime.Start(ctx, name); err != nil {
		t.Fatalf("restart the replacement: %v", err)
	}
	running, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	if err := runtime.WaitState(
		running, name, "running", 500*time.Millisecond,
	); err != nil {
		t.Fatalf("the replacement did not come back: %v", err)
	}
	restarted := appleSweepMustContainer(t, runtime, name)
	if class := ClassifyOwnership(restarted.Configuration.Labels); class !=
		OwnershipDual {
		t.Fatalf("restart lost the dual labels: %q", class)
	}
	assertLegacyReaderStillFinds(
		t, "restarted container "+name, restarted.Configuration.Labels,
	)
	if got := appleSweepReadSentinel(t, runtime, name); got != sentinel {
		t.Fatalf("volume data did not survive restart: %q", got)
	}

	// A second sweep is a no-op: the container is dual now, so it is skipped.
	repeat, err := NewLabelSweeper(runtime).Plan(ctx, "post-migration")
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range repeat.Records {
		if record.ID != name {
			continue
		}
		if record.Disposition != MigrationSkipped {
			t.Fatalf("a migrated container is still a target: %#v", record)
		}
	}

	// Nothing outside the probe changed class, including the live managed
	// topology this host is running.
	for id, class := range appleSweepOwnershipByID(t, runtime, name) {
		if untouched[id] != class {
			t.Fatalf(
				"the scoped sweep changed unrelated container %s: %q -> %q",
				id, untouched[id], class,
			)
		}
	}
}

// appleSweepOwnershipByID snapshots the ownership class of every container on
// the host except the probe.
func appleSweepOwnershipByID(
	t *testing.T,
	runtime *AppleRuntime,
	exclude string,
) map[string]OwnershipClass {
	t.Helper()
	containers, err := runtime.Containers(context.Background())
	if err != nil {
		t.Fatalf("list containers: %v", err)
	}
	classes := make(map[string]OwnershipClass, len(containers))
	for _, container := range containers {
		if container.ID == exclude {
			continue
		}
		classes[container.ID] = ClassifyOwnership(container.Configuration.Labels)
	}
	return classes
}

// A partially migrated container must stop the sweep on a real host too, and
// must survive the refusal with its volume still attached.
func TestAppleContainerSweepRefusesToMutateWhileAConflictExists(t *testing.T) {
	runtime, prefix := appleContainerRuntimeForPhase(t, "labelp2conflict")
	image := strings.TrimSpace(os.Getenv(appleContainerImageEnv))
	ctx := context.Background()
	raw := func(args ...string) CommandResult {
		return runtime.runner.Run(ctx, args)
	}

	name, volume, network := appleSweepProbe(t, runtime, prefix, image)

	conflicting := prefix + "-conflicting"
	if result := raw(
		"run", "--detach", "--name", conflicting,
		"--label", LegacyManagedLabelKey+"=v1",
		"--label", CurrentManagedLabelKey+"=v2",
		"--network", network,
		"--entrypoint", "/bin/sleep", image, "900",
	); result.Status != 0 {
		t.Fatalf("seed conflicting container: %s", result.Stderr)
	}
	t.Cleanup(func() {
		raw("stop", conflicting)
		raw("delete", "--force", conflicting)
	})

	sweeper := NewLabelSweeper(runtime)
	sweeper.Only = []string{name}
	report, err := sweeper.Apply(ctx)
	if err == nil {
		t.Fatal("the sweep ran on a host carrying a partial migration")
	}
	if report.Applied {
		t.Fatal("report claims the sweep was applied")
	}

	// The probe is untouched: still present, still legacy-only, still holding
	// its volume.
	survived := appleSweepMustContainer(t, runtime, name)
	if class := ClassifyOwnership(survived.Configuration.Labels); class !=
		OwnershipLegacyOnly {
		t.Fatalf("the probe was modified despite the refusal: %q", class)
	}
	attachments := NamedVolumeAttachments(survived)
	if len(attachments) != 1 || attachments[0].Name != volume {
		t.Fatalf("the probe lost its volume during a refusal: %#v", attachments)
	}
	if _, present := appleLabels(t, runtime, "volume", volume); !present {
		t.Fatal("a volume was deleted during a refused sweep")
	}
}
