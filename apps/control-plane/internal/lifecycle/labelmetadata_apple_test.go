package lifecycle

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This file grounds Phase 3A on a real Apple Container host. The headless tests
// prove the document rewriter and the staging rules; only the real runtime can
// prove the three things this phase actually rests on:
//
//   - Apple Container has no route that changes an existing resource's labels,
//     so the migration has to edit its metadata while it is stopped;
//   - an offline edit is what the runtime reports after it starts again, which
//     is the only definition of "the label changed" that matters;
//   - the data inside a named volume is untouched by that edit, which is why
//     this phase can migrate a volume at all where Phase 2 could not.
//
// Run with:
//
//	AGENTOPS_TEST_APPLE_CONTAINER=1 \
//	AGENTOPS_TEST_APPLE_IMAGE=<local image with /bin/sh> \
//	go test ./apps/control-plane/internal/lifecycle/ -run AppleContainerMetadata -v -count=1
//
// Every resource carries a prefix unique to this process, every target is named
// by exact identity, and the suite never selects by label. A live managed
// topology on the same host is therefore never a target even though it is
// exactly the kind of resource this migration is for.

// stopSystemForTest stops the runtime and proves it, restoring it on cleanup.
// Bringing the host back up is registered before the stop so a failure inside
// the stopped window cannot leave the operator's machine without a runtime.
func stopSystemForTest(t *testing.T, runtime *AppleRuntime) {
	t.Helper()
	ctx := context.Background()
	t.Cleanup(func() {
		if err := runtime.StartSystem(ctx); err != nil {
			t.Errorf("restart Apple Container: %v", err)
		}
		waitForRuntime(t, runtime)
	})
	if err := runtime.StopSystem(ctx); err != nil {
		t.Fatalf("stop Apple Container: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err := runtime.RequireServicesStopped(ctx); err == nil {
			return
		} else if time.Now().After(deadline) {
			t.Fatalf("Apple Container did not stop: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func startSystemForTest(t *testing.T, runtime *AppleRuntime) {
	t.Helper()
	if err := runtime.StartSystem(context.Background()); err != nil {
		t.Fatalf("start Apple Container: %v", err)
	}
	waitForRuntime(t, runtime)
}

func waitForRuntime(t *testing.T, runtime *AppleRuntime) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if capability := runtime.Capability(ctx); capability.ServiceRunning {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("Apple Container did not come back up")
}

// appleMetadataProbes creates one stopped container, one named volume carrying
// a sentinel, and one network, all seeded legacy-only with the raw CLI so they
// look exactly like a pre-migration host.
func appleMetadataProbes(
	t *testing.T,
	runtime *AppleRuntime,
	prefix, image string,
) (container, volume, network string) {
	t.Helper()
	ctx := context.Background()
	raw := func(args ...string) CommandResult {
		return runtime.runner.Run(ctx, args)
	}
	volume = prefix + "-data"
	if result := raw(
		"volume", "create", "--label", LegacyManagedLabelKey+"=v1", volume,
	); result.Status != 0 {
		t.Fatalf("seed probe volume: %s", result.Stderr)
	}
	t.Cleanup(func() { raw("volume", "delete", volume) })

	network = prefix + "-internal"
	if result := raw(
		"network", "create", "--label", LegacyManagedLabelKey+"=v1", network,
	); result.Status != 0 {
		t.Fatalf("seed probe network: %s", result.Stderr)
	}
	t.Cleanup(func() { raw("network", "delete", network) })

	container = prefix + "-ctr"
	if result := raw(
		"create", "--name", container,
		"--label", LegacyManagedLabelKey+"=v1",
		"--label", LegacyRoleLabelKey+"=runner",
		"--volume", volume+":/data",
		"--entrypoint", "/bin/sh",
		image, "-c", "sleep 3600",
	); result.Status != 0 {
		t.Fatalf("seed probe container: %s", result.Stderr)
	}
	t.Cleanup(func() { raw("delete", container) })
	return container, volume, network
}

// writeVolumeSentinel puts a known string inside the named volume. The whole
// reason Phase 3A can migrate a volume where Phase 2 could not is that it never
// recreates one, and this is what proves the data was left alone.
func writeVolumeSentinel(
	t *testing.T, runtime *AppleRuntime, volume, image, value string,
) {
	t.Helper()
	result := runtime.runner.Run(context.Background(), []string{
		"run", "--rm", "--entrypoint", "/bin/sh",
		"--volume", volume + ":/data", image,
		"-c", "printf %s " + value + " > /data/servo-sentinel",
	})
	if result.Status != 0 {
		t.Fatalf("write volume sentinel: %s", result.Stderr)
	}
}

func readVolumeSentinel(
	t *testing.T, runtime *AppleRuntime, volume, image string,
) string {
	t.Helper()
	result := runtime.runner.Run(context.Background(), []string{
		"run", "--rm", "--entrypoint", "/bin/sh",
		"--volume", volume + ":/data", image,
		"-c", "cat /data/servo-sentinel",
	})
	if result.Status != 0 {
		t.Fatalf("read volume sentinel: %s", result.Stderr)
	}
	return strings.TrimSpace(result.Stdout)
}

func TestAppleContainerMetadataStagesMigrateAndRollBack(t *testing.T) {
	runtime, prefix := appleContainerRuntimeForPhase(t, "labelp3a")
	image := strings.TrimSpace(os.Getenv(appleContainerImageEnv))
	ctx := context.Background()
	container, volume, network := appleMetadataProbes(t, runtime, prefix, image)

	host, err := runtime.ResolveMetadataHost(ctx)
	if err != nil {
		t.Fatalf("resolve host: %v", err)
	}
	// The sentinel proves the volume's data is untouched by a metadata edit,
	// which is the claim that lets this phase migrate a volume where Phase 2
	// could not.
	const sentinel = "servo-p3a-sentinel"
	writeVolumeSentinel(t, runtime, volume, image, sentinel)

	// The image file is measured across the sweep window only. Running a
	// container against the volume writes to it legitimately, so a comparison
	// that spanned one would be measuring the container, not the migration.
	imagePath := filepath.Join(host.AppRoot, "volumes", volume, "volume.img")
	beforeImage, err := os.Stat(imagePath)
	if err != nil {
		t.Fatalf("probe volume image: %v", err)
	}

	targets := []MetadataTargetRef{
		{Kind: MetadataKindContainer, ID: container},
		{Kind: MetadataKindVolume, ID: volume},
		{Kind: MetadataKindNetwork, ID: network},
	}
	backupRoot := filepath.Join(t.TempDir(), "backups")

	// --- prepare -------------------------------------------------------
	stopSystemForTest(t, runtime)
	prepared, err := ApplyMetadataSweep(
		ctx, runtime, host, MetadataStagePrepare, targets, backupRoot,
	)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if len(prepared.Applied) != 3 {
		t.Fatalf("expected three resources prepared, got %d", len(prepared.Applied))
	}
	startSystemForTest(t, runtime)
	if err := VerifyMetadataSweep(ctx, runtime, prepared); err != nil {
		t.Fatalf("runtime does not report the prepared labels: %v", err)
	}
	inventory, err := TakeOwnershipInventory(ctx, runtime)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range targets {
		if class := classOf(t, inventory, target); class != OwnershipDual {
			t.Fatalf("%s is %q after prepare, want dual", target, class)
		}
	}

	// --- retire --------------------------------------------------------
	stopSystemForTest(t, runtime)
	retireBackups := filepath.Join(t.TempDir(), "retire-backups")
	retired, err := ApplyMetadataSweep(
		ctx, runtime, host, MetadataStageRetire, targets, retireBackups,
	)
	if err != nil {
		t.Fatalf("retire: %v", err)
	}
	startSystemForTest(t, runtime)
	if err := VerifyMetadataSweep(ctx, runtime, retired); err != nil {
		t.Fatalf("runtime does not report the retired labels: %v", err)
	}
	inventory, err = TakeOwnershipInventory(ctx, runtime)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range targets {
		if class := classOf(t, inventory, target); class != OwnershipCurrentOnly {
			t.Fatalf("%s is %q after retire, want current-only", target, class)
		}
	}
	// Zero residue: no document may still mention the legacy namespace.
	for _, target := range targets {
		requireNoLegacyResidue(t, host.AppRoot, target)
	}

	// --- the volume image was not written by the migration ---------------
	// Nothing has run a container since beforeImage was taken, so any change
	// here would have been made by the sweep itself.
	sweptImage, err := os.Stat(imagePath)
	if err != nil {
		t.Fatalf("probe volume image vanished: %v", err)
	}
	if sweptImage.Size() != beforeImage.Size() ||
		!sweptImage.ModTime().Equal(beforeImage.ModTime()) {
		t.Fatalf(
			"the migration wrote to the volume image (%d@%v -> %d@%v)",
			beforeImage.Size(), beforeImage.ModTime(),
			sweptImage.Size(), sweptImage.ModTime(),
		)
	}
	if got := readVolumeSentinel(t, runtime, volume, image); got != sentinel {
		t.Fatalf("volume sentinel reads %q after the sweep, want %q", got, sentinel)
	}

	// --- the probe container still starts and stops ---------------------
	if result := runtime.runner.Run(
		ctx, []string{"start", container},
	); result.Status != 0 {
		t.Fatalf("probe container will not start after the sweep: %s", result.Stderr)
	}
	if result := runtime.runner.Run(
		ctx, []string{"stop", container},
	); result.Status != 0 {
		t.Logf("probe container stop reported: %s", result.Stderr)
	}

	// --- rollback to dual, then reapply --------------------------------
	stopSystemForTest(t, runtime)
	if err := RollbackMetadataSweep(retired); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	startSystemForTest(t, runtime)
	inventory, err = TakeOwnershipInventory(ctx, runtime)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range targets {
		if class := classOf(t, inventory, target); class != OwnershipDual {
			t.Fatalf("%s is %q after rollback, want dual again", target, class)
		}
	}
	stopSystemForTest(t, runtime)
	reapplyBackups := filepath.Join(t.TempDir(), "reapply-backups")
	reapplied, err := ApplyMetadataSweep(
		ctx, runtime, host, MetadataStageRetire, targets, reapplyBackups,
	)
	if err != nil {
		t.Fatalf("reapply retire: %v", err)
	}
	startSystemForTest(t, runtime)
	if err := VerifyMetadataSweep(ctx, runtime, reapplied); err != nil {
		t.Fatalf("reapplied sweep is not reported by the runtime: %v", err)
	}

	// The sentinel is still readable after everything above, which is the
	// end-to-end statement this phase makes about named volumes.
	if got := readVolumeSentinel(t, runtime, volume, image); got != sentinel {
		t.Fatalf("volume sentinel reads %q, want %q", got, sentinel)
	}
	if afterImage, err := os.Stat(imagePath); err != nil {
		t.Fatalf("probe volume image vanished: %v", err)
	} else if afterImage.Size() != beforeImage.Size() {
		t.Fatalf(
			"volume image size changed from %d to %d",
			beforeImage.Size(), afterImage.Size(),
		)
	}
}

// TestAppleContainerMetadataRefusesWhileTheRuntimeIsUp proves the stopped-state
// gate is real rather than advisory.
func TestAppleContainerMetadataRefusesWhileTheRuntimeIsUp(t *testing.T) {
	runtime, prefix := appleContainerRuntimeForPhase(t, "labelp3aguard")
	image := strings.TrimSpace(os.Getenv(appleContainerImageEnv))
	ctx := context.Background()
	_, volume, _ := appleMetadataProbes(t, runtime, prefix, image)
	host, err := runtime.ResolveMetadataHost(ctx)
	if err != nil {
		t.Fatalf("resolve host: %v", err)
	}
	if _, err := ApplyMetadataSweep(
		ctx, runtime, host, MetadataStagePrepare,
		[]MetadataTargetRef{{Kind: MetadataKindVolume, ID: volume}},
		filepath.Join(t.TempDir(), "backups"),
	); err == nil {
		t.Fatal("expected the sweep to refuse a running runtime")
	}
}

// TestAppleContainerMetadataRefusesRetireBeforePrepare proves the staging order
// is enforced against a real host rather than only in the planner's unit tests.
func TestAppleContainerMetadataRefusesRetireBeforePrepare(t *testing.T) {
	runtime, prefix := appleContainerRuntimeForPhase(t, "labelp3aorder")
	image := strings.TrimSpace(os.Getenv(appleContainerImageEnv))
	ctx := context.Background()
	_, volume, _ := appleMetadataProbes(t, runtime, prefix, image)
	host, err := runtime.ResolveMetadataHost(ctx)
	if err != nil {
		t.Fatalf("resolve host: %v", err)
	}
	stopSystemForTest(t, runtime)
	if _, err := ApplyMetadataSweep(
		ctx, runtime, host, MetadataStageRetire,
		[]MetadataTargetRef{{Kind: MetadataKindVolume, ID: volume}},
		filepath.Join(t.TempDir(), "backups"),
	); err == nil {
		t.Fatal("expected retire on a legacy-only resource to be refused")
	}
}

func classOf(
	t *testing.T,
	inventory *OwnershipInventory,
	target MetadataTargetRef,
) OwnershipClass {
	t.Helper()
	for _, record := range inventory.Records {
		if record.Kind == target.Kind && record.ID == target.ID {
			return record.Class
		}
	}
	t.Fatalf("%s is missing from the inventory", target)
	return ""
}

func requireNoLegacyResidue(
	t *testing.T,
	appRoot string,
	target MetadataTargetRef,
) {
	t.Helper()
	layout, err := layoutFor(target.Kind)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(appRoot, layout.directory, target.ID)
	for name := range layout.documents {
		path := filepath.Join(directory, name)
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if strings.Contains(string(data), LegacyLabelNamespace) {
			t.Fatalf(
				"%s still carries the legacy namespace after retire: %s",
				path, data,
			)
		}
	}
}
