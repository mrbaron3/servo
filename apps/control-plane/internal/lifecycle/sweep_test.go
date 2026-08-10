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
	// imageEnvironment is what the probe image declares for itself.
	imageEnvironment []string
	imageEnvErr      error
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
		// The fixture's PATH is an image default; everything else on it is
		// specification-supplied.
		imageEnvironment: []string{"PATH=/usr/bin"},
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

func (runtime *fakeSweepRuntime) ImageEnvironment(
	_ context.Context,
	_ string,
) ([]string, error) {
	return runtime.imageEnvironment, runtime.imageEnvErr
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
	replacement := synthesizeFromSpec(spec, previous, runtime.imageEnvironment)
	if runtime.synthesize != nil {
		replacement = runtime.synthesize(spec, previous)
	}
	replacement.Status.State = state
	runtime.containers = append(runtime.containers, replacement)
	return spec.Name, nil
}

// synthesizeFromSpec models what Apple Container would materialize from a
// specification. It deliberately builds the replacement from the SPEC rather
// than copying the pre-delete record: copying would make every field the
// rebuild forgot reappear for free, so a rebuild that silently dropped a mount,
// a capability, or an environment variable would still pass every equivalence
// test. Fields the runtime defaults rather than the specification supplies
// (image digest, resources, image-default entrypoint) are carried over, because
// that is what the real runtime does.
func synthesizeFromSpec(
	spec ContainerSpec,
	previous ContainerActual,
	imageEnvironment []string,
) ContainerActual {
	replacement := previous
	labels := map[string]string{
		LegacyManagedLabelKey:  ManagedLabelValue,
		CurrentManagedLabelKey: ManagedLabelValue,
		LegacyRoleLabelKey:     spec.Role,
		CurrentRoleLabelKey:    spec.Role,
	}
	if spec.SpecDigest != "" {
		labels[LegacySpecLabelKey] = spec.SpecDigest
		labels[CurrentSpecLabelKey] = spec.SpecDigest
	}
	replacement.Configuration.Labels = labels
	replacement.Configuration.ReadOnly = spec.ReadOnly
	replacement.Configuration.UseInit = spec.Init
	// Apple Container reports these as empty arrays rather than omitting them,
	// so the fake has to as well; a nil here would look like drift.
	replacement.Configuration.CapAdd = append([]string{}, spec.CapAdd...)
	replacement.Configuration.CapDrop = []string{}
	if spec.CapDropAll {
		replacement.Configuration.CapDrop = []string{"ALL"}
	}
	replacement.Configuration.Image.Reference = spec.Image
	replacement.Configuration.InitProcess.User.Raw.UserString = spec.User
	if spec.Entrypoint != "" {
		replacement.Configuration.InitProcess.Executable = spec.Entrypoint
		replacement.Configuration.InitProcess.Arguments =
			append([]string(nil), spec.Command...)
	}

	// The effective environment is what the image declares plus what the
	// specification supplies.
	environment := append([]string(nil), imageEnvironment...)
	declared := make(map[string]bool, len(imageEnvironment))
	for _, entry := range imageEnvironment {
		if key, _, ok := strings.Cut(entry, "="); ok {
			declared[key] = true
		}
	}
	keys := make([]string, 0, len(spec.Environment))
	for key := range spec.Environment {
		keys = append(keys, key)
	}
	for _, key := range sortedStrings(keys) {
		if declared[key] {
			continue
		}
		environment = append(environment, key+"="+spec.Environment[key])
	}
	replacement.Configuration.InitProcess.Environment = environment

	// Mounts, networks, and publications are kept in their observed order but
	// only when the specification still asks for them, so anything the rebuild
	// dropped is simply absent from the replacement.
	wantedTmpfs := make(map[string]bool, len(spec.Tmpfs))
	for _, target := range spec.Tmpfs {
		wantedTmpfs[target] = true
	}
	wantedVolumes := make(map[string]Mount, len(spec.Mounts))
	for _, mount := range spec.Mounts {
		wantedVolumes[mount.Target] = mount
	}
	mounts := make([]ContainerMount, 0, len(previous.Configuration.Mounts))
	for _, mount := range previous.Configuration.Mounts {
		if _, isTmpfs := mount.Type["tmpfs"]; isTmpfs {
			if wantedTmpfs[mount.Destination] {
				mounts = append(mounts, mount)
			}
			continue
		}
		if _, isVolume := mount.Type["volume"]; isVolume {
			wanted, present := wantedVolumes[mount.Destination]
			if !present {
				continue
			}
			rebuilt := mount
			rebuilt.Options = nil
			if wanted.ReadOnly {
				rebuilt.Options = []string{"ro"}
			}
			mounts = append(mounts, rebuilt)
		}
		// Anything else was never expressible, so it does not come back.
	}
	replacement.Configuration.Mounts = mounts

	wantedNetworks := make(map[string]bool, len(spec.Networks))
	for _, network := range spec.Networks {
		wantedNetworks[network] = true
	}
	networks := replacement.Configuration.Networks[:0:0]
	for _, network := range previous.Configuration.Networks {
		if wantedNetworks[network.Network] {
			networks = append(networks, network)
		}
	}
	replacement.Configuration.Networks = networks

	published := make([]map[string]any, 0, len(spec.Publish))
	for _, publication := range spec.Publish {
		for _, observed := range previous.Configuration.PublishedPorts {
			if port, ok := numericField(observed["containerPort"]); ok &&
				port == publication.ContainerPort {
				published = append(published, observed)
			}
		}
	}
	replacement.Configuration.PublishedPorts = published
	return replacement
}

// distinctVolume renders a mount list so two containers in one test do not
// accidentally share a named volume.
func distinctVolume(name string) string {
	return `[
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

// testSweeper builds a sweeper whose targets are always named, mirroring the
// real contract: a sweep may only mutate containers the caller listed.
func testSweeper(runtime SweepRuntime, only ...string) *LabelSweeper {
	sweeper := NewLabelSweeper(runtime)
	sweeper.ReleasePollInterval = time.Millisecond
	sweeper.ReleaseTimeout = 250 * time.Millisecond
	sweeper.Only = only
	return sweeper
}

// "Every old-only container" is a selector whose meaning depends on what else
// is on the host. Mutating on one is exactly what Issue #123 forbids.
func TestSweepRefusesABroadSelector(t *testing.T) {
	runtime := newFakeSweepRuntime(containerFixture(
		t, "agentops-runner", "stopped",
		legacyOnlyLabels("runner", fixtureSpecDigest), "",
	))
	report, err := testSweeper(runtime).Apply(context.Background())
	if err == nil {
		t.Fatal("a sweep with no named targets was allowed to mutate")
	}
	if report.Applied {
		t.Fatal("report claims a broad sweep was applied")
	}
	if len(runtime.deleted) != 0 || len(runtime.createdSpec) != 0 {
		t.Fatalf("a broad sweep mutated the host: %v", runtime.deleted)
	}
}

// A repeated identity makes the intended target count ambiguous.
func TestSweepRejectsDuplicateTargetIdentities(t *testing.T) {
	runtime := newFakeSweepRuntime(containerFixture(
		t, "agentops-runner", "stopped",
		legacyOnlyLabels("runner", fixtureSpecDigest), "",
	))
	_, err := testSweeper(
		runtime, "agentops-runner", "agentops-runner",
	).Apply(context.Background())
	if err == nil {
		t.Fatal("a duplicated identity was accepted")
	}
	if len(runtime.deleted) != 0 {
		t.Fatal("the sweep mutated despite an ambiguous target set")
	}
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
	report, err := testSweeper(runtime, "agentops-runner").
		Apply(context.Background())
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
	report, err := testSweeper(runtime, "agentops-runner").
		Apply(context.Background())
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
	// The post-sweep audit must say this sweep migrated it, not merely that it
	// is dual now: a fresh inventory cannot tell those apart.
	if report.After.Totals[MigrationPending] != 0 ||
		report.After.Totals[MigrationMigrated] != 1 {
		t.Fatalf("post-sweep inventory is not clean: %#v", report.After.Totals)
	}
	if len(report.Migrated) != 1 || report.Migrated[0] != "agentops-runner" {
		t.Fatalf("migrated set = %v", report.Migrated)
	}
	if len(report.PlannedSpecs) != 1 ||
		report.PlannedSpecs[0].Name != "agentops-runner" {
		t.Fatalf("planned replacements = %#v", report.PlannedSpecs)
	}
	// The durable plan carries environment keys, never their values.
	for _, key := range report.PlannedSpecs[0].EnvironmentKeys {
		if strings.Contains(key, "=") || strings.Contains(key, "secret") {
			t.Fatalf("planned replacement leaked an environment value: %q", key)
		}
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
	if _, err := testSweeper(runtime, "agentops-runner").
		Apply(context.Background()); err != nil {
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
	report, err := testSweeper(runtime, "agentops-runner").
		Apply(context.Background())
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
	report, err := testSweeper(runtime, "agentops-runner").
		Apply(context.Background())
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
	report, err := testSweeper(runtime, "agentops-runner").
		Apply(context.Background())
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
		containerFixtureWithMounts(t, "agentops-triage", "stopped",
			legacyOnlyLabels("triage", fixtureSpecDigest),
			distinctVolume("agentops-triage-credentials"), ""),
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
	report, err := testSweeper(
		runtime, "agentops-runner", "agentops-triage",
	).Apply(context.Background())
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
		containerFixtureWithMounts(t, "agentops-triage", "stopped",
			legacyOnlyLabels("triage", fixtureSpecDigest),
			distinctVolume("agentops-triage-credentials"), ""),
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

// A migration nobody can audit or roll back must not start. This guard is
// load-bearing in both the runbook and the PR, so it is tested rather than
// asserted.
func TestSweepRefusesToMutateWhenTheSnapshotCannotBeWritten(t *testing.T) {
	runtime := newFakeSweepRuntime(containerFixture(
		t, "agentops-runner", "stopped",
		legacyOnlyLabels("runner", fixtureSpecDigest), "",
	))
	sweeper := testSweeper(runtime, "agentops-runner")
	sweeper.SnapshotBeforeMutation = func(
		MigrationAudit, []PlannedReplacement,
	) error {
		return errors.New("evidence directory is read-only")
	}
	report, err := sweeper.Apply(context.Background())
	if err == nil {
		t.Fatal("the sweep ran without durable evidence")
	}
	if report.Applied {
		t.Fatal("report claims the sweep was applied")
	}
	if len(runtime.deleted) != 0 || len(runtime.createdSpec) != 0 ||
		len(runtime.stopped) != 0 {
		t.Fatalf("the sweep mutated before its snapshot: %v", runtime.deleted)
	}
}

// The snapshot has to carry enough to drive the documented rollback, and the
// plan is built for every target before any of them is touched.
func TestSnapshotCarriesEveryPlannedReplacementBeforeMutating(t *testing.T) {
	runtime := newFakeSweepRuntime(
		containerFixture(t, "agentops-runner", "stopped",
			legacyOnlyLabels("runner", fixtureSpecDigest), ""),
		containerFixtureWithMounts(t, "agentops-triage", "stopped",
			legacyOnlyLabels("triage", fixtureSpecDigest),
			distinctVolume("agentops-triage-credentials"), ""),
	)
	sweeper := testSweeper(runtime, "agentops-runner", "agentops-triage")
	var captured []PlannedReplacement
	var mutationsAtSnapshot int
	sweeper.SnapshotBeforeMutation = func(
		_ MigrationAudit, planned []PlannedReplacement,
	) error {
		captured = planned
		mutationsAtSnapshot = len(runtime.deleted) + len(runtime.createdSpec)
		return nil
	}
	if _, err := sweeper.Apply(context.Background()); err != nil {
		t.Fatalf("sweep failed: %v", err)
	}
	if mutationsAtSnapshot != 0 {
		t.Fatal("the snapshot was taken after mutation had begun")
	}
	if len(captured) != 2 {
		t.Fatalf("planned replacements = %d, want 2", len(captured))
	}
	for _, planned := range captured {
		if planned.Image == "" || planned.Role == "" ||
			planned.RecreateVerb == "" || planned.ObservedState == "" {
			t.Fatalf("planned replacement is not actionable: %#v", planned)
		}
	}
}

// A target that cannot be reproduced must stop the sweep while every container
// is still alive, not after the earlier ones have been replaced.
func TestSweepBlocksAllTargetsBeforeMutatingAnyOfThem(t *testing.T) {
	runtime := newFakeSweepRuntime(
		containerFixture(t, "agentops-runner", "stopped",
			legacyOnlyLabels("runner", fixtureSpecDigest), ""),
		// The second target carries a label the writer cannot reproduce.
		containerFixtureWithMounts(t, "agentops-triage", "stopped", `{
			"com.mrbaron3.workflow.agentopsctl": "v1",
			"com.mrbaron3.workflow.role": "triage",
			"com.example.team": "platform"
		}`, distinctVolume("agentops-triage-credentials"), ""),
	)
	_, err := testSweeper(
		runtime, "agentops-runner",
	).Apply(context.Background())
	if err != nil {
		t.Fatalf("the reproducible target should still migrate: %v", err)
	}
	// Now include the unreproducible one; nothing further may be mutated.
	deletionsBefore := len(runtime.deleted)
	_, err = testSweeper(
		runtime, "agentops-triage",
	).Apply(context.Background())
	if err == nil {
		t.Fatal("an unreproducible target was swept")
	}
	if len(runtime.deleted) != deletionsBefore {
		t.Fatal("the sweep mutated an unreproducible target")
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
	report, err := testSweeper(runtime, "agentops-runner").
		Apply(context.Background())
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
