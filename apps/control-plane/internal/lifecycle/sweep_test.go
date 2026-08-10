package lifecycle

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeSweepRuntime models Apple Container closely enough to exercise the
// ordering guarantees the sweep depends on: a deleted container can linger in
// the listing, and a replacement is only ever synthesized from what the sweep
// actually asked for.
type fakeSweepRuntime struct {
	containers []ContainerActual
	volumes    map[string]bool

	deleteLinger int
	pending      map[string]int
	originals    map[string]ContainerActual

	deleted     []string
	stopped     []string
	createdVerb []string
	createdSpec []ContainerSpec

	deleteErr error
	// synthesize builds the replacement. Tests override it to inject drift.
	synthesize func(ContainerSpec, ContainerActual) ContainerActual
	// deleteVolume simulates a runtime that destroys a volume on delete, which
	// Phase 2 must never do.
	deleteVolume string
}

func newFakeSweepRuntime(containers ...ContainerActual) *fakeSweepRuntime {
	runtime := &fakeSweepRuntime{
		containers: containers,
		volumes:    map[string]bool{},
		pending:    map[string]int{},
		originals:  map[string]ContainerActual{},
	}
	for _, container := range containers {
		for _, attachment := range NamedVolumeAttachments(container) {
			runtime.volumes[attachment.Name] = true
		}
	}
	return runtime
}

func (runtime *fakeSweepRuntime) Containers(
	_ context.Context,
) ([]ContainerActual, error) {
	// A deleted container lingers in the listing for a few polls, which is what
	// makes Apple Container's exclusive volume attachment observable.
	for id, remaining := range runtime.pending {
		if remaining <= 1 {
			delete(runtime.pending, id)
			runtime.remove(id)
			continue
		}
		runtime.pending[id] = remaining - 1
	}
	return append([]ContainerActual(nil), runtime.containers...), nil
}

func (runtime *fakeSweepRuntime) remove(id string) {
	kept := make([]ContainerActual, 0, len(runtime.containers))
	for _, container := range runtime.containers {
		if container.ID != id {
			kept = append(kept, container)
		}
	}
	runtime.containers = kept
}

func (runtime *fakeSweepRuntime) Volumes(
	_ context.Context,
) ([]Resource, error) {
	resources := make([]Resource, 0, len(runtime.volumes))
	for name, present := range runtime.volumes {
		if !present {
			continue
		}
		var resource Resource
		resource.ID = name
		resource.Configuration.Name = name
		resources = append(resources, resource)
	}
	return resources, nil
}

func (runtime *fakeSweepRuntime) Stop(
	_ context.Context,
	name string,
	_ int,
) error {
	runtime.stopped = append(runtime.stopped, name)
	for index := range runtime.containers {
		if runtime.containers[index].ID == name {
			runtime.containers[index].Status.State = "stopped"
		}
	}
	return nil
}

func (runtime *fakeSweepRuntime) Delete(_ context.Context, name string) error {
	if runtime.deleteErr != nil {
		return runtime.deleteErr
	}
	for _, container := range runtime.containers {
		if container.ID == name {
			runtime.originals[name] = container
		}
	}
	runtime.deleted = append(runtime.deleted, name)
	if runtime.deleteLinger > 0 {
		runtime.pending[name] = runtime.deleteLinger
	} else {
		runtime.remove(name)
	}
	if runtime.deleteVolume != "" {
		runtime.volumes[runtime.deleteVolume] = false
	}
	return nil
}

func (runtime *fakeSweepRuntime) CreateContainer(
	ctx context.Context,
	spec ContainerSpec,
) (string, error) {
	return runtime.materialize(ctx, "create", spec, "stopped")
}

func (runtime *fakeSweepRuntime) RunContainer(
	ctx context.Context,
	spec ContainerSpec,
) (string, error) {
	return runtime.materialize(ctx, "run", spec, "running")
}

func (runtime *fakeSweepRuntime) materialize(
	_ context.Context,
	verb string,
	spec ContainerSpec,
	state string,
) (string, error) {
	if _, _, err := containerArgs(verb, spec); err != nil {
		return "", err
	}
	runtime.createdVerb = append(runtime.createdVerb, verb)
	runtime.createdSpec = append(runtime.createdSpec, spec)
	previous := runtime.originals[spec.Name]
	replacement := previous
	if runtime.synthesize != nil {
		replacement = runtime.synthesize(spec, previous)
	} else {
		replacement.Configuration.Labels = dualLabelMap(
			previous.Configuration.Labels,
		)
	}
	replacement.Status.State = state
	runtime.containers = append(runtime.containers, replacement)
	return spec.Name, nil
}

// distinctVolume overrides a fixture's mounts so two containers in one test do
// not accidentally share a named volume.
func distinctVolume(name string) string {
	return `,"mounts": [
		{"destination": "/tmp", "source": "tmpfs", "type": {"tmpfs": {}}},
		{
			"destination": "/workspace",
			"source": "/redacted",
			"type": {"volume": {"name": "` + name + `", "format": "ext4"}}
		}
	]`
}

// dualLabelMap mirrors what the Phase 1 writer puts on a new container.
func dualLabelMap(legacy map[string]string) map[string]string {
	mirrored := make(map[string]string, len(legacy)*2)
	for key, value := range legacy {
		mirrored[key] = value
	}
	for legacyKey, currentKey := range map[string]string{
		LegacyManagedLabelKey: CurrentManagedLabelKey,
		LegacyRoleLabelKey:    CurrentRoleLabelKey,
		LegacySpecLabelKey:    CurrentSpecLabelKey,
	} {
		if value, present := legacy[legacyKey]; present {
			mirrored[currentKey] = value
		}
	}
	return mirrored
}

func testSweeper(runtime SweepRuntime) *LabelSweeper {
	sweeper := NewLabelSweeper(runtime)
	sweeper.ReleasePollInterval = time.Millisecond
	sweeper.ReleaseTimeout = 250 * time.Millisecond
	return sweeper
}

// A partial migration anywhere means ownership is already ambiguous. The sweep
// must not mutate a single container until a human resolves it.
func TestSweepRefusesToMutateWhileAnyContainerConflicts(t *testing.T) {
	runtime := newFakeSweepRuntime(
		containerFixture(t, "agentops-runner", "stopped",
			legacyOnlyLabels("runner", fixtureSpecDigest), ""),
		containerFixture(t, "agentops-triage", "stopped", `{
			"com.mrbaron3.workflow.agentopsctl": "v1",
			"com.mrbaron3.servo.agentopsctl": "v2"
		}`, ""),
	)
	report, err := testSweeper(runtime).Apply(context.Background())
	if err == nil {
		t.Fatal("the sweep ran while a conflicting container existed")
	}
	if report.Applied {
		t.Fatal("report claims the sweep was applied")
	}
	if len(runtime.deleted) != 0 || len(runtime.createdSpec) != 0 ||
		len(runtime.stopped) != 0 {
		t.Fatalf(
			"the sweep mutated something: deleted=%v created=%d stopped=%v",
			runtime.deleted, len(runtime.createdSpec), runtime.stopped,
		)
	}
}

// Relabelling is not a reason to start a topology an operator stopped.
func TestSweepRecreatesAStoppedContainerWithoutStartingIt(t *testing.T) {
	runtime := newFakeSweepRuntime(containerFixture(
		t, "agentops-runner", "stopped",
		legacyOnlyLabels("runner", fixtureSpecDigest), "",
	))
	report, err := testSweeper(runtime).Apply(context.Background())
	if err != nil {
		t.Fatalf("sweep failed: %v (%+v)", err, report.Steps)
	}
	if len(runtime.createdVerb) != 1 || runtime.createdVerb[0] != "create" {
		t.Fatalf("stopped container was recreated with %v", runtime.createdVerb)
	}
	if len(runtime.stopped) != 0 {
		t.Fatalf("an already-stopped container was stopped again: %v",
			runtime.stopped)
	}
	if report.After.Totals[MigrationPending] != 0 ||
		report.After.Totals[MigrationSkipped] != 1 {
		t.Fatalf("post-sweep inventory is not clean: %#v", report.After.Totals)
	}
	// The replacement carries both namespaces, so the pre-migration binary can
	// still discover it.
	args, _, err := containerArgs("create", runtime.createdSpec[0])
	if err != nil {
		t.Fatal(err)
	}
	rendered := strings.Join(args, " ")
	if !strings.Contains(rendered, LegacyManagedLabelKey+"=v1") ||
		!strings.Contains(rendered, CurrentManagedLabelKey+"=v1") {
		t.Fatalf("replacement is not dual-labeled: %s", rendered)
	}
}

// A running container keeps running, and is stopped gracefully first.
func TestSweepPreservesARunningContainersState(t *testing.T) {
	runtime := newFakeSweepRuntime(containerFixture(
		t, "agentops-runner", "running",
		legacyOnlyLabels("runner", fixtureSpecDigest), "",
	))
	if _, err := testSweeper(runtime).Apply(context.Background()); err != nil {
		t.Fatalf("sweep failed: %v", err)
	}
	if len(runtime.stopped) != 1 {
		t.Fatalf("running container was not gracefully stopped: %v",
			runtime.stopped)
	}
	if len(runtime.createdVerb) != 1 || runtime.createdVerb[0] != "run" {
		t.Fatalf("running container was recreated with %v", runtime.createdVerb)
	}
}

// The exclusivity contract: the replacement must not be created until the
// deleted container has actually left the listing.
func TestSweepWaitsForVolumeReleaseBeforeReattaching(t *testing.T) {
	runtime := newFakeSweepRuntime(containerFixture(
		t, "agentops-runner", "stopped",
		legacyOnlyLabels("runner", fixtureSpecDigest), "",
	))
	runtime.deleteLinger = 3
	report, err := testSweeper(runtime).Apply(context.Background())
	if err != nil {
		t.Fatalf("sweep failed: %v (%+v)", err, report.Steps)
	}
	var released, recreated int
	for index, step := range report.Steps {
		switch step.Stage {
		case StageVolumeRelease:
			released = index
		case StageRecreate:
			recreated = index
		}
	}
	if released == 0 || recreated == 0 || released > recreated {
		t.Fatalf("release was not proven before re-attachment: %+v", report.Steps)
	}
}

// If release never happens the sweep must fail rather than attach anyway.
func TestSweepFailsRatherThanReattachWhileVolumeIsHeld(t *testing.T) {
	runtime := newFakeSweepRuntime(containerFixture(
		t, "agentops-runner", "stopped",
		legacyOnlyLabels("runner", fixtureSpecDigest), "",
	))
	// A linger far longer than the timeout models a volume that never frees.
	runtime.deleteLinger = 1_000_000
	report, err := testSweeper(runtime).Apply(context.Background())
	if err == nil {
		t.Fatal("the sweep re-attached a volume that was never released")
	}
	if len(runtime.createdSpec) != 0 {
		t.Fatal("a replacement was created before release was proven")
	}
	if !strings.Contains(report.Halted, "agentops-runner") {
		t.Fatalf("halt reason does not name the container: %q", report.Halted)
	}
}

// Phase 2 never destroys data. If a volume vanishes the sweep says so.
func TestSweepFailsWhenANamedVolumeDisappears(t *testing.T) {
	runtime := newFakeSweepRuntime(containerFixture(
		t, "agentops-runner", "stopped",
		legacyOnlyLabels("runner", fixtureSpecDigest), "",
	))
	runtime.deleteVolume = "agentops-runner-workspace"
	report, err := testSweeper(runtime).Apply(context.Background())
	if err == nil {
		t.Fatal("a destroyed named volume was accepted")
	}
	if len(runtime.createdSpec) != 0 {
		t.Fatal("a replacement was created after its volume disappeared")
	}
	if !strings.Contains(err.Error(), "agentops-runner-workspace") {
		t.Fatalf("failure does not name the lost volume: %v", err)
	}
	for _, record := range report.Volumes {
		if record.Name == "agentops-runner-workspace" && record.PresentAfter {
			t.Fatal("preservation evidence contradicts the failure")
		}
	}
}

// A replacement that is not provably identical stops everything.
func TestSweepHaltsOnReplacementDriftAndLeavesTheRestUntouched(t *testing.T) {
	runtime := newFakeSweepRuntime(
		containerFixture(t, "agentops-runner", "stopped",
			legacyOnlyLabels("runner", fixtureSpecDigest), ""),
		containerFixture(t, "agentops-triage", "stopped",
			legacyOnlyLabels("triage", fixtureSpecDigest),
			distinctVolume("agentops-triage-credentials")),
	)
	runtime.synthesize = func(
		_ ContainerSpec,
		previous ContainerActual,
	) ContainerActual {
		drifted := previous
		drifted.Configuration.Labels = dualLabelMap(previous.Configuration.Labels)
		drifted.Configuration.ReadOnly = false
		return drifted
	}
	report, err := testSweeper(runtime).Apply(context.Background())
	if err == nil {
		t.Fatal("a drifted replacement was accepted")
	}
	// The failing container is deleted and recreated before verification runs;
	// what must not happen is the sweep moving on to the next container.
	if len(runtime.deleted) != 1 || runtime.deleted[0] != "agentops-runner" {
		t.Fatalf("unexpected deletions: %v", runtime.deleted)
	}
	if containsString(runtime.stopped, "agentops-triage") {
		t.Fatalf("the sweep touched the next container: %v", runtime.stopped)
	}
	for _, spec := range runtime.createdSpec {
		if spec.Name == "agentops-triage" {
			t.Fatal("the sweep continued past a failure to the next container")
		}
	}
	if !strings.Contains(report.Halted, "read-only root") {
		t.Fatalf("halt reason does not name the drift: %q", report.Halted)
	}
}

// The plan is a dry run: it must never mutate.
func TestPlanIsReadOnly(t *testing.T) {
	runtime := newFakeSweepRuntime(containerFixture(
		t, "agentops-runner", "stopped",
		legacyOnlyLabels("runner", fixtureSpecDigest), "",
	))
	audit, err := testSweeper(runtime).Plan(context.Background(), "dry-run")
	if err != nil {
		t.Fatal(err)
	}
	if len(audit.MigrationTargets()) != 1 {
		t.Fatalf("dry run did not identify the target: %#v", audit.Totals)
	}
	if len(runtime.deleted) != 0 || len(runtime.createdSpec) != 0 ||
		len(runtime.stopped) != 0 {
		t.Fatal("the dry run mutated the host")
	}
}

// A container that changed between planning and mutation must abort, because
// the next stage deletes it.
func TestSweepAbortsWhenTheTargetChangedSincePlanning(t *testing.T) {
	runtime := newFakeSweepRuntime(containerFixture(
		t, "agentops-runner", "stopped",
		legacyOnlyLabels("runner", fixtureSpecDigest), "",
	))
	sweeper := testSweeper(runtime)
	before, err := sweeper.Plan(context.Background(), "pre")
	if err != nil {
		t.Fatal(err)
	}
	if len(before.MigrationTargets()) != 1 {
		t.Fatal("fixture is not a migration target")
	}
	// Somebody migrated it out from under us.
	runtime.containers[0].Configuration.Labels = dualLabelMap(
		runtime.containers[0].Configuration.Labels,
	)
	report := SweepReport{}
	err = sweeper.migrateOne(
		context.Background(), before.MigrationTargets()[0], &report,
	)
	if err == nil {
		t.Fatal("the sweep deleted a container that was no longer a target")
	}
	if len(runtime.deleted) != 0 {
		t.Fatal("the sweep deleted despite the re-inspection failing")
	}
}

// Narrowing the sweep must actually narrow it. This is what lets an operator
// migrate one container at a time on a live host.
func TestSweepOnlyTouchesTheNamedIdentities(t *testing.T) {
	runtime := newFakeSweepRuntime(
		containerFixture(t, "agentops-runner", "stopped",
			legacyOnlyLabels("runner", fixtureSpecDigest), ""),
		containerFixture(t, "agentops-triage", "stopped",
			legacyOnlyLabels("triage", fixtureSpecDigest),
			distinctVolume("agentops-triage-credentials")),
	)
	sweeper := testSweeper(runtime)
	sweeper.Only = []string{"agentops-triage"}
	if _, err := sweeper.Apply(context.Background()); err != nil {
		t.Fatalf("scoped sweep failed: %v", err)
	}
	if len(runtime.deleted) != 1 || runtime.deleted[0] != "agentops-triage" {
		t.Fatalf("scoped sweep touched the wrong containers: %v",
			runtime.deleted)
	}
	if len(runtime.createdSpec) != 1 ||
		runtime.createdSpec[0].Name != "agentops-triage" {
		t.Fatalf("scoped sweep recreated the wrong container: %#v",
			runtime.createdSpec)
	}
}

// Naming a container that is not a pending target must fail rather than
// quietly do nothing, which would read as a successful migration.
func TestSweepRejectsAnOnlyIdentityThatIsNotPending(t *testing.T) {
	runtime := newFakeSweepRuntime(containerFixture(
		t, "agentops-runner", "stopped",
		dualLabels("runner", fixtureSpecDigest), "",
	))
	sweeper := testSweeper(runtime)
	sweeper.Only = []string{"agentops-runner"}
	report, err := sweeper.Apply(context.Background())
	if err == nil {
		t.Fatal("a non-target identity was silently accepted")
	}
	if report.Applied {
		t.Fatal("report claims a sweep happened")
	}
	if len(runtime.deleted) != 0 {
		t.Fatal("the sweep mutated despite an invalid selection")
	}
}

// A delete failure must stop the sweep at the delete stage, before anything
// tries to re-attach a volume.
func TestSweepStopsAtDeleteFailure(t *testing.T) {
	runtime := newFakeSweepRuntime(containerFixture(
		t, "agentops-runner", "stopped",
		legacyOnlyLabels("runner", fixtureSpecDigest), "",
	))
	runtime.deleteErr = errors.New("container is not owned by agentopsctl")
	report, err := testSweeper(runtime).Apply(context.Background())
	if err == nil {
		t.Fatal("a failing delete did not stop the sweep")
	}
	last := report.Steps[len(report.Steps)-1]
	if last.Stage != StageDelete || last.Outcome != "failed" {
		t.Fatalf("unexpected final step: %#v", last)
	}
	if len(runtime.createdSpec) != 0 {
		t.Fatal("a replacement was created after a failed delete")
	}
}
