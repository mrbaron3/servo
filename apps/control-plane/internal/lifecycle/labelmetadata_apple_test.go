package lifecycle

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This file grounds what Phase 3B keeps of Phase 3A on a real Apple Container
// host. Phase 3A's forward stages are gone; the recovery path that restores a
// document to the exact bytes a Phase 3A run recorded is not, and only the real
// runtime can prove the two things it rests on:
//
//   - an offline edit is what the runtime reports after it starts again, which
//     is the only definition of "the label changed" that matters. Reading back
//     the file this process just wrote would prove the writer agreed with
//     itself;
//   - the data inside a named volume is untouched by that edit, which is why a
//     volume can be relabelled at all where Phase 2 could only recreate it.
//
// It also grounds the one-way boundary itself. After a rollback restores
// pre-Phase-3A labels, the resource is genuinely invisible to this binary on a
// real host — not merely classified differently by a fake. That is the fact the
// runbook asks an operator to accept before rolling back, so it is worth
// proving on metal rather than asserting in prose.
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
// exactly the kind of resource this recovery path is for.

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

// writeVolumeSentinel puts a known string inside the named volume. The reason a
// volume can be relabelled at all is that nothing recreates one, and this is
// what proves the data was left alone across the edit.
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

// TestAppleContainerMetadataRollbackRestoresExactlyAndEndsTheReadPath drives the
// retained recovery path end to end against a real host.
//
// The fixture is a Phase 3A `retire` run reconstructed rather than performed:
// this binary can no longer produce one, so the test builds the record such a
// run would have left — a backup holding the pre-migration bytes, and a plan
// naming the document, its digests, and the complete label maps on either side.
// That is exactly the input an operator's retained backup root contains, and
// driving the recovery path from a hand-built record is the only way left to
// exercise it.
func TestAppleContainerMetadataRollbackRestoresExactlyAndEndsTheReadPath(t *testing.T) {
	runtime, prefix := appleContainerRuntimeForPhase(t, "labelp3b")
	image := strings.TrimSpace(os.Getenv(appleContainerImageEnv))
	ctx := context.Background()
	raw := func(args ...string) CommandResult {
		return runtime.runner.Run(ctx, args)
	}

	// A current-only volume, which is what every managed resource looks like
	// after Phase 3A's sweep.
	volume := prefix + "-data"
	if result := raw(
		"volume", "create", "--label", CurrentManagedLabelKey+"=v1", volume,
	); result.Status != 0 {
		t.Fatalf("seed probe volume: %s", result.Stderr)
	}
	t.Cleanup(func() { raw("volume", "delete", volume) })
	writeVolumeSentinel(t, runtime, volume, image, "p3b-rollback")

	host, err := runtime.ResolveMetadataHost(ctx)
	if err != nil {
		t.Fatalf("resolve metadata host: %v", err)
	}
	document := filepath.Join(host.AppRoot, "volumes", volume, "entity.json")
	backupRoot := filepath.Join(t.TempDir(), "private-backups")

	// The labels a pre-Phase-3A host carried. Restoring these is the whole point
	// of the retained path, and they are deliberately in the namespace this
	// binary no longer reads.
	beforeLabels := map[string]string{
		legacyManagedLabelKey: "v1",
		"com.example.foreign": "keep-me",
	}

	stopSystemForTest(t, runtime)
	if err := runtime.RequireServicesStopped(ctx); err != nil {
		t.Fatalf("services are not stopped: %v", err)
	}

	state, err := inspectMetadataFile(metadataFileRef{
		Path: document, LabelPath: []string{"labels"},
	})
	if err != nil {
		t.Fatalf("inspect the runtime's own volume document: %v", err)
	}
	afterLabels := state.Labels
	if afterLabels[CurrentManagedLabelKey] != "v1" {
		t.Fatalf("the seeded volume document is not current-only: %v", afterLabels)
	}
	// The backup a Phase 3A run would have taken: the same document carrying the
	// pre-migration labels.
	backupBytes, err := rewriteLabels(state.Bytes, []string{"labels"}, beforeLabels)
	if err != nil {
		t.Fatalf("build the pre-migration document: %v", err)
	}
	backupPath := filepath.Join(backupRoot, "volume", volume, "entity.json")
	if err := writeBackup(backupPath, backupBytes); err != nil {
		t.Fatalf("write the reconstructed backup: %v", err)
	}

	plan := &RollbackPlan{
		// A stage name this binary no longer defines, carried verbatim.
		Stage: "retire",
		Applied: []*MetadataApplication{{
			Kind:        MetadataKindVolume,
			ID:          volume,
			Stage:       "retire",
			ClassBefore: "legacy-only",
			ClassAfter:  "current-only",
			ChangedKeys: []string{legacyManagedLabelKey, CurrentManagedLabelKey},
			Files: []*metadataFileApplication{{
				LabelPath:    []string{"labels"},
				Document:     filepath.Join("volumes", volume, "entity.json"),
				Backup:       filepath.Join("volume", volume, "entity.json"),
				BeforeSHA256: digestOf(backupBytes),
				AfterSHA256:  state.SHA256,
				BackupSHA256: digestOf(backupBytes),
				Mode:         state.Mode.Perm(),
			}},
		}},
		Locations: [][]RollbackLocation{{
			{Path: document, BackupPath: backupPath},
		}},
		Labels: []RollbackLabels{{Before: beforeLabels, After: afterLabels}},
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	// Going through the parser rather than using the struct directly grounds the
	// round trip an operator's on-disk plan actually takes.
	parsed, err := ParseRollbackPlan(encoded)
	if err != nil {
		t.Fatalf("parse the reconstructed plan: %v", err)
	}
	// The plan carries absolute paths read out of a file, so it is bound to the
	// host it will be applied to before anything is written. This grounds the
	// binding's VERDICTS against a real appRoot; the ordering claim — that a
	// refusal precedes StopSystem — is a property of the command and is pinned
	// by TestRollbackNeverStopsTheRuntimeWhenThePlanDoesNotBind, because this
	// suite has already stopped the runtime by the time it gets here.
	if err := parsed.BindToHost(host); err != nil {
		t.Fatalf("bind the reconstructed plan to this host: %v", err)
	}
	// A plan naming a document this host does not keep must be refused even
	// though every digest in it is correct.
	forged, err := ParseRollbackPlan(encoded)
	if err != nil {
		t.Fatal(err)
	}
	forged.Applied[0].ID = "vol-that-does-not-exist"
	if err := forged.BindToHost(host); err == nil {
		t.Fatal("a plan naming another resource was bound to this host")
	}
	if err := RollbackMetadataSweep(parsed); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if outcome := parsed.Applied[0].Files[0].RestoredAs; outcome != RestoreExactBytes {
		t.Fatalf("restore outcome = %q, want %q", outcome, RestoreExactBytes)
	}

	startSystemForTest(t, runtime)

	// What the runtime reports is the only definition of "the label changed"
	// that counts.
	restored, present := appleLabels(t, runtime, "volume", volume)
	if !present {
		t.Fatal("the volume disappeared across the rollback")
	}
	if restored[legacyManagedLabelKey] != "v1" {
		t.Fatalf("the runtime does not report the restored labels: %v", restored)
	}
	if _, stillCurrent := restored[CurrentManagedLabelKey]; stillCurrent {
		t.Fatalf("the current namespace survived the rollback: %v", restored)
	}
	// A foreign label must survive a rollback untouched. The plan carries the
	// complete label maps precisely so recovery cannot drop somebody else's key.
	if restored["com.example.foreign"] != "keep-me" {
		t.Fatalf("the rollback dropped a foreign label: %v", restored)
	}

	// The one-way boundary, on metal: this binary can no longer see what it just
	// restored, and refuses it rather than reusing the name.
	if class := ClassifyOwnership(restored); class != OwnershipMissingLabel {
		t.Fatalf(
			"a rolled-back volume still classifies as %q on a real host: %v",
			class, restored,
		)
	}
	if err := runtime.EnsureVolume(ctx, volume); err == nil {
		t.Fatal("a rolled-back volume was adopted by the Phase 3B reader")
	}
	if _, stillThere := appleLabels(t, runtime, "volume", volume); !stillThere {
		t.Fatal("a rolled-back volume was removed by the Phase 3B reader")
	}

	// And the data the whole exercise exists to protect is still there.
	if sentinel := readVolumeSentinel(
		t, runtime, volume, image,
	); sentinel != "p3b-rollback" {
		t.Fatalf("volume sentinel = %q after the rollback", sentinel)
	}
}

// TestAppleContainerMetadataRollbackRefusesWhileTheRuntimeIsUp proves the
// stopped-state gate is real rather than advisory. It is the guard that keeps a
// recovery from editing a document the runtime is holding its own view of.
func TestAppleContainerMetadataRollbackRefusesWhileTheRuntimeIsUp(t *testing.T) {
	runtime, _ := appleContainerRuntimeForPhase(t, "labelp3b")
	if err := runtime.RequireServicesStopped(context.Background()); err == nil {
		t.Fatal("the stopped-state proof passed while Apple Container was running")
	}
}
