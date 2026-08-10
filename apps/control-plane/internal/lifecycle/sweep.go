package lifecycle

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// The sweep is the mutating half of Phase 2. It walks one container at a time
// through a fixed sequence of stages, and every stage is chosen so that an
// interruption leaves the operator somewhere they can reason about:
//
//	re-inspect -> stop -> delete -> volume-release -> recreate -> verify
//
// Interruption and rollback per stage:
//
//	re-inspect      nothing has changed; rerun the sweep.
//	stop            the original still exists, still legacy-only.
//	                Roll back with `container start <id>`.
//	delete          the only window where the container does not exist. The
//	                named volume is untouched and still holds the data. Roll
//	                back by recreating from the snapshot in the evidence file.
//	volume-release  read-only polling; same position as delete.
//	recreate        the replacement exists. Roll back by deleting it and
//	                recreating from the snapshot.
//	verify          on drift the sweep stops and escalates. It deliberately
//	                does not "repair" automatically: a replacement that is not
//	                provably identical is a decision for an operator.
//
// A failure at any stage stops the whole sweep. The remaining containers are
// left untouched, because the same fault is likely to affect them too and a
// half-swept topology is harder to reason about than an unswept one.

// SweepRuntime is the container runtime surface the sweep needs. It is an
// interface so the state machine can be exercised without Apple Container,
// while the grounded suite runs the very same code against the real runtime.
type SweepRuntime interface {
	Containers(ctx context.Context) ([]ContainerActual, error)
	Volumes(ctx context.Context) ([]Resource, error)
	ImageConfiguration(
		ctx context.Context, image string,
	) (ImageConfiguration, error)
	ImageDigest(ctx context.Context, image string) (string, error)
	Stop(ctx context.Context, name string, seconds int) error
	// DeleteExisting must report ErrContainerAbsent rather than succeeding when
	// the identity has already gone.
	DeleteExisting(ctx context.Context, name string) error
	CreateContainer(ctx context.Context, spec ContainerSpec) (string, error)
	RunContainer(ctx context.Context, spec ContainerSpec) (string, error)
}

// SweepStage names one step of the per-container state machine.
type SweepStage string

const (
	StageReinspect     SweepStage = "re-inspect"
	StageStop          SweepStage = "stop"
	StageDelete        SweepStage = "delete"
	StageVolumeRelease SweepStage = "volume-release"
	StageRecreate      SweepStage = "recreate"
	StageVerify        SweepStage = "verify"
)

// SweepStep is one bounded audit line. Detail is operator-readable and carries
// no environment value or host path: it is written to durable evidence.
type SweepStep struct {
	Container string     `json:"container"`
	Stage     SweepStage `json:"stage"`
	Outcome   string     `json:"outcome"`
	Detail    string     `json:"detail"`
	// Mutating marks a step that actually changed the host. It is what makes
	// "did this sweep apply anything?" answerable from the record rather than
	// inferred from which stage it reached: stopping an already-stopped
	// container changes nothing, and a delete that failed changed nothing
	// either.
	Mutating bool `json:"mutating,omitempty"`
}

// VolumePreservation records that a named volume outlived the sweep. Phase 2
// never deletes a volume, and this is the evidence for that claim.
type VolumePreservation struct {
	Name          string `json:"name"`
	PresentBefore bool   `json:"presentBefore"`
	PresentAfter  bool   `json:"presentAfter"`
}

// SweepReport is the machine-readable record of one sweep.
type SweepReport struct {
	Applied bool                 `json:"applied"`
	Before  MigrationAudit       `json:"before"`
	After   MigrationAudit       `json:"after"`
	Steps   []SweepStep          `json:"steps"`
	Volumes []VolumePreservation `json:"volumes"`
	// Migrated names the containers this sweep actually replaced, which a
	// post-sweep inventory cannot tell apart from ones that were already dual.
	Migrated []string `json:"migrated"`
	Halted   string   `json:"halted,omitempty"`
	// VolumeListingError records that the post-sweep volume listing could not
	// be read. It is deliberately distinct from an absent volume.
	VolumeListingError string `json:"volumeListingError,omitempty"`
	// PlannedSpecs records, per target, the configuration needed to recreate it
	// if the sweep is interrupted after the delete. Environment values are
	// reduced to their keys: the runbook's rollback needs to know which
	// variables existed, and durable evidence must never carry their values.
	PlannedSpecs []PlannedReplacement `json:"plannedSpecs"`
}

// PlannedReplacement is the redacted, durable form of a rebuilt specification.
// It exists so the pre-mutation snapshot can actually drive the rollback the
// runbook prescribes; an inventory record alone cannot, because it omits
// everything the replacement needs.
type PlannedReplacement struct {
	Name            string        `json:"name"`
	Role            string        `json:"role"`
	Image           string        `json:"image"`
	ImageDigest     string        `json:"imageDigest"`
	SpecDigest      string        `json:"specDigest"`
	Networks        []string      `json:"networks"`
	Mounts          []Mount       `json:"mounts"`
	Tmpfs           []string      `json:"tmpfs"`
	Publish         []Publication `json:"publish"`
	EnvironmentKeys []string      `json:"environmentKeys"`
	ReadOnly        bool          `json:"readOnly"`
	CapDropAll      bool          `json:"capDropAll"`
	Init            bool          `json:"init"`
	User            string        `json:"user"`
	Entrypoint      string        `json:"entrypoint"`
	Command         []string      `json:"command"`
	ObservedState   string        `json:"observedState"`
	RecreateVerb    string        `json:"recreateVerb"`
	// CPUs and MemoryMiB are what the replacement is created with. They are
	// part of the plan because the specification now restates them rather than
	// letting the runtime default twice, so a rollback that omitted them would
	// not reproduce the container.
	CPUs      int   `json:"cpus"`
	MemoryMiB int64 `json:"memoryMiB"`
	// WorkingDirOverride is the argv override, empty when the working directory
	// is inherited from the image. ObservedWorkingDirectory is the effective
	// value the container was running with. Recording only the effective one
	// would turn an inherited directory into an explicit override on rollback,
	// which is a different container configuration even when it behaves the
	// same today.
	WorkingDirOverride       string `json:"workingDirOverride"`
	ObservedWorkingDirectory string `json:"observedWorkingDirectory"`
}

// RedactedReplacement converts a rebuilt specification into its durable form.
func RedactedReplacement(
	spec ContainerSpec,
	actual ContainerActual,
) PlannedReplacement {
	keys := make([]string, 0, len(spec.Environment))
	for key := range spec.Environment {
		keys = append(keys, key)
	}
	verb := "create"
	if actual.Status.State == "running" {
		verb = "run"
	}
	return PlannedReplacement{
		Name:                     spec.Name,
		Role:                     spec.Role,
		Image:                    spec.Image,
		ImageDigest:              actual.Configuration.Image.Descriptor.Digest,
		SpecDigest:               spec.SpecDigest,
		Networks:                 spec.Networks,
		Mounts:                   spec.Mounts,
		Tmpfs:                    spec.Tmpfs,
		Publish:                  spec.Publish,
		EnvironmentKeys:          sortedStrings(keys),
		ReadOnly:                 spec.ReadOnly,
		CapDropAll:               spec.CapDropAll,
		Init:                     spec.Init,
		User:                     spec.User,
		Entrypoint:               spec.Entrypoint,
		Command:                  spec.Command,
		ObservedState:            actual.Status.State,
		RecreateVerb:             verb,
		CPUs:                     spec.CPUs,
		MemoryMiB:                spec.MemoryMiB,
		WorkingDirOverride:       spec.WorkingDir,
		ObservedWorkingDirectory: actual.Configuration.InitProcess.WorkingDirectory,
	}
}

// plannedMigration is the complete plan for one container, held in memory for
// the life of the sweep. The evidence file gets RedactedReplacement instead,
// because this carries environment values.
//
// Keeping it matters for more than convenience: the replacement is created from
// this exact specification, and the observation it was derived from is compared
// against a fresh one immediately before the delete. Re-deriving the
// specification at mutation time would silently migrate whatever now answers to
// that name, which on Apple Container is a reusable identifier.
type plannedMigration struct {
	Observed ContainerActual
	Spec     ContainerSpec
}

// LabelSweeper migrates old-only containers to dual labels.
type LabelSweeper struct {
	Runtime             SweepRuntime
	StopTimeoutSeconds  int
	ReleasePollInterval time.Duration
	ReleaseTimeout      time.Duration
	// RecreateAttempts bounds how many times the replacement is retried when
	// the runtime reports the named volume is still exclusively attached.
	RecreateAttempts int
	// SnapshotBeforeMutation receives the exact pre-mutation audit the sweep is
	// about to act on. Issue #123 requires that snapshot to be durable before
	// the first mutation, so returning an error here aborts the sweep with
	// nothing changed: a migration whose evidence cannot be written is a
	// migration nobody can audit or roll back.
	SnapshotBeforeMutation func(MigrationAudit, []PlannedReplacement) error
	// Only is the exact set of container identities this sweep may mutate. It
	// is required: Issue #123 forbids acting on a broad or unresolved selector,
	// and "every pending container" is exactly such a selector — its meaning
	// depends on whatever else happens to be on the host at that moment.
	// Naming the targets makes the blast radius a decision the operator wrote
	// down rather than one the inventory made for them.
	//
	// An identity listed here that is not a pending target is an error rather
	// than a silent no-op, a duplicate is rejected, and an identity absent from
	// here is never touched. Plan stays broad, because reading is safe.
	Only []string
}

// NewLabelSweeper returns a sweeper with timings suited to Apple Container's
// virtual machine teardown, which is where volume release actually happens.
func NewLabelSweeper(runtime SweepRuntime) *LabelSweeper {
	return &LabelSweeper{
		Runtime:             runtime,
		StopTimeoutSeconds:  20,
		ReleasePollInterval: 500 * time.Millisecond,
		ReleaseTimeout:      2 * time.Minute,
		RecreateAttempts:    3,
	}
}

// Plan takes a read-only inventory. It mutates nothing and is the dry run.
func (sweeper *LabelSweeper) Plan(
	ctx context.Context,
	phase string,
) (MigrationAudit, error) {
	containers, err := sweeper.Runtime.Containers(ctx)
	if err != nil {
		return MigrationAudit{}, err
	}
	return BuildMigrationAudit(phase, containers), nil
}

// Apply migrates every pending container. It refuses to mutate anything at all
// while a conflicting container exists: a partial migration somewhere in the
// topology means the operator's model of ownership is already wrong, and the
// remedy needs a human who can see which namespace holds the stale value.
func (sweeper *LabelSweeper) Apply(ctx context.Context) (SweepReport, error) {
	before, err := sweeper.Plan(ctx, "pre-migration")
	if err != nil {
		return SweepReport{}, err
	}
	report := SweepReport{Applied: true, Before: before}
	if before.HasConflicts() {
		report.Applied = false
		report.Halted = "conflicting containers are present; " +
			"resolve the partial migration before sweeping"
		return report, fmt.Errorf("%s", report.Halted)
	}
	// Target selection is validated before the snapshot is written, so a
	// mistyped identity fails without leaving a stray evidence file behind.
	targets, err := sweeper.selectedTargets(before)
	if err != nil {
		report.Applied = false
		report.Halted = err.Error()
		return report, err
	}
	// Every target is rebuilt before any of them is mutated. A target that
	// cannot be reproduced then stops the sweep while all of them are still
	// alive, instead of stopping it after the earlier ones have been replaced.
	plans, err := sweeper.planReplacements(ctx, targets)
	if err != nil {
		report.Applied = false
		report.Halted = err.Error()
		return report, err
	}
	planned := make([]PlannedReplacement, 0, len(plans))
	for _, plan := range plans {
		planned = append(planned, RedactedReplacement(plan.Spec, plan.Observed))
	}
	report.PlannedSpecs = planned
	if sweeper.SnapshotBeforeMutation != nil {
		if err := sweeper.SnapshotBeforeMutation(before, planned); err != nil {
			report.Applied = false
			report.Halted = "pre-mutation snapshot could not be recorded"
			return report, fmt.Errorf(
				"refusing to sweep without durable evidence: %w", err,
			)
		}
	}
	volumesBefore, err := sweeper.volumeNames(ctx)
	if err != nil {
		// Every other abort path says so in the durable record; this one must
		// too, or the evidence claims a sweep ran when none did.
		report.Applied = false
		report.Halted = "the pre-sweep volume listing could not be read: " +
			err.Error()
		return report, err
	}
	migrated := make([]string, 0, len(targets))
	verified := make(map[string]ContainerActual, len(plans))
	var sweepErr error
	for _, plan := range plans {
		if stepErr := sweeper.migrateOne(
			ctx, plan, &report, verified,
		); stepErr != nil {
			report.Halted = fmt.Sprintf(
				"halted at container %s: %v", plan.Observed.ID, stepErr,
			)
			sweepErr = stepErr
			break
		}
		migrated = append(migrated, plan.Observed.ID)
	}
	report.Migrated = migrated
	// Applied means this sweep changed the host, not that it started. A first
	// target that fails while being re-inspected has mutated nothing, and a
	// durable record claiming otherwise would send an operator looking for
	// damage that does not exist. Once anything has been stopped, deleted, or
	// recreated — for this target or an earlier one — it stays true.
	report.Applied = mutated(report)
	report.Volumes = sweeper.preservation(ctx, volumesBefore, &report)
	// The post-state is recorded even when the sweep halted. The case that
	// matters is the dangerous one — some containers migrated and one did not —
	// and that is exactly the case an empty post-state would hide.
	containers, planErr := sweeper.Runtime.Containers(ctx)
	if planErr == nil {
		after := BuildMigrationAudit("post-migration", containers)
		markMigrated(&after, migrated, verified, containers)
		report.After = after
	}
	if sweepErr != nil {
		return report, sweepErr
	}
	return report, planErr
}

// markMigrated distinguishes a container this sweep migrated from one that was
// already dual before it started. Both are "skipped" to a fresh inventory, and
// only the sweep knows which is which.
// It rewrites a record only when the container still carrying that identity is
// byte-for-byte the one that was verified. Anything else answering to the name
// by now was not proven by this sweep, and relabelling it "proven equivalent"
// would erase its real classification — a container that turned conflicting in
// the meantime would have its conflict subtracted from the totals the Phase 3
// gate is read from.
func markMigrated(
	audit *MigrationAudit,
	migrated []string,
	verified map[string]ContainerActual,
	containers []ContainerActual,
) {
	if len(migrated) == 0 {
		return
	}
	final := make(map[string]ContainerActual, len(containers))
	for _, container := range containers {
		final[container.ID] = container
	}
	confirmed := make(map[string]bool, len(migrated))
	for _, id := range migrated {
		expected, proven := verified[id]
		if !proven {
			continue
		}
		current, present := final[id]
		if !present {
			continue
		}
		if equal, err := canonicallyEqual(expected, current); err != nil ||
			!equal {
			continue
		}
		confirmed[id] = true
	}
	for index := range audit.Records {
		if !confirmed[audit.Records[index].ID] {
			continue
		}
		audit.Totals[audit.Records[index].Disposition]--
		audit.Records[index].Disposition = MigrationMigrated
		audit.Records[index].Reason =
			"migrated by this sweep and proven equivalent"
		audit.Totals[MigrationMigrated]++
	}
}

// planReplacements rebuilds every target's specification up front and reduces
// each to its durable, redacted form.
func (sweeper *LabelSweeper) planReplacements(
	ctx context.Context,
	targets []ContainerInventoryRecord,
) ([]plannedMigration, error) {
	plans := make([]plannedMigration, 0, len(targets))
	for _, target := range targets {
		actual, err := sweeper.containerByID(ctx, target.ID)
		if err != nil {
			return nil, err
		}
		if actual == nil {
			return nil, fmt.Errorf(
				"container %s disappeared between planning and rebuild",
				target.ID,
			)
		}
		image, err := sweeper.Runtime.ImageConfiguration(
			ctx, actual.Configuration.Image.Reference,
		)
		if err != nil {
			return nil, err
		}
		spec, err := RebuildMigratedSpec(*actual, image)
		if err != nil {
			return nil, err
		}
		plans = append(plans, plannedMigration{Observed: *actual, Spec: spec})
	}
	return plans, nil
}

// requireUnchangedSincePlan proves the container answering to this name is
// still the one the plan was built from. Apple Container names are reusable, so
// without this the sweep could delete a container it never inspected and
// recreate it from somebody else's configuration. The runtime-assigned status
// beyond the lifecycle state is excluded: an address does not change identity.
func requireUnchangedSincePlan(planned, fresh ContainerActual) error {
	if err := requireConfigurationUnchanged(planned, fresh); err != nil {
		return err
	}
	if planned.Status.State != fresh.Status.State {
		return fmt.Errorf(
			"container %s changed lifecycle state from %q to %q since the "+
				"snapshot", planned.ID, planned.Status.State, fresh.Status.State,
		)
	}
	return nil
}

// requireConfigurationUnchanged compares everything except the lifecycle state.
// The state is separated out because the sweep changes it itself: by the time
// it deletes a container it has already stopped it, so the useful question at
// that point is not "is this the state I planned?" but "is this still the same
// container, and is it stopped?".
func requireConfigurationUnchanged(planned, fresh ContainerActual) error {
	equal, err := canonicallyEqual(
		planned.Configuration, fresh.Configuration,
	)
	if err != nil {
		return fmt.Errorf(
			"container %s could not be compared against its plan: %w",
			planned.ID, err,
		)
	}
	if !equal {
		return fmt.Errorf(
			"container %s changed between the snapshot and this step; another "+
				"actor is mutating it", planned.ID,
		)
	}
	return nil
}

// requireUnmovedImage proves the container's image reference still resolves to
// the digest it was created from. Phase 2 replaces a container's labels, not
// its contents, and a moved tag would quietly turn one into the other.
func (sweeper *LabelSweeper) requireUnmovedImage(
	ctx context.Context,
	actual ContainerActual,
) error {
	observed := strings.TrimSpace(
		actual.Configuration.Image.Descriptor.Digest,
	)
	if observed == "" {
		return fmt.Errorf(
			"container %s records no image digest, so its replacement cannot "+
				"be pinned to the same content", actual.ID,
		)
	}
	current, err := sweeper.Runtime.ImageDigest(
		ctx, actual.Configuration.Image.Reference,
	)
	if err != nil {
		return err
	}
	if current != observed {
		// The digests are named but not echoed alongside operator-supplied
		// text; they are safe to show and are what the operator needs.
		return fmt.Errorf(
			"container %s was created from image digest %s but its reference "+
				"now resolves to %s; recreating it would replace its contents, "+
				"not its labels", actual.ID, observed, current,
		)
	}
	return nil
}

// requireStillPlanned re-resolves an exact identity immediately before a
// destructive step and proves it is still the container the plan was built
// from. Proving ownership alone is not enough: another agentopsctl instance
// could have replaced this reusable name with a *different* managed container,
// which would pass every label check while being something this sweep never
// inspected. The full comparison is repeated rather than reused from
// re-inspection, because the whole point is the gap between the two.
func (sweeper *LabelSweeper) requireStillPlanned(
	ctx context.Context,
	planned ContainerActual,
	expectedState string,
) error {
	actual, err := sweeper.containerByID(ctx, planned.ID)
	if err != nil {
		return err
	}
	if actual == nil {
		return fmt.Errorf(
			"container %s vanished before this step; another actor is "+
				"changing it", planned.ID,
		)
	}
	if err := RequireManaged(
		"container "+planned.ID, actual.Configuration.Labels,
	); err != nil {
		return err
	}
	if err := requireConfigurationUnchanged(planned, *actual); err != nil {
		return err
	}
	if actual.Status.State != expectedState {
		return fmt.Errorf(
			"container %s is %q immediately before this step, not the %q this "+
				"sweep left it in", planned.ID, actual.Status.State, expectedState,
		)
	}
	return nil
}

// selectedTargets resolves Only against the planned targets. It fails closed in
// three ways, each of which is a case where continuing would mutate more, or
// something other, than the operator named:
//
//   - an empty set, which is the broad selector Issue #123 forbids;
//   - an identity that is not a pending target, because an operator who named
//     a container expects it migrated and a silent no-op reads as success;
//   - a duplicate, because it makes the intended target count ambiguous.
func (sweeper *LabelSweeper) selectedTargets(
	audit MigrationAudit,
) ([]ContainerInventoryRecord, error) {
	if len(sweeper.Only) == 0 {
		return nil, fmt.Errorf(
			"a sweep must name the exact containers it may mutate; " +
				"refusing to act on a broad selector",
		)
	}
	byID := make(map[string]ContainerInventoryRecord, len(audit.Records))
	for _, target := range audit.MigrationTargets() {
		byID[target.ID] = target
	}
	seen := make(map[string]bool, len(sweeper.Only))
	selected := make([]ContainerInventoryRecord, 0, len(sweeper.Only))
	for _, id := range sweeper.Only {
		if seen[id] {
			return nil, fmt.Errorf(
				"container %s is named more than once in the target set", id,
			)
		}
		seen[id] = true
		target, pending := byID[id]
		if !pending {
			return nil, fmt.Errorf(
				"container %s was requested but is not a pending migration "+
					"target", id,
			)
		}
		selected = append(selected, target)
	}
	return selected, nil
}

// migrateOne walks a single container through the state machine.
func (sweeper *LabelSweeper) migrateOne(
	ctx context.Context,
	plan plannedMigration,
	report *SweepReport,
	verified map[string]ContainerActual,
) error {
	id := plan.Observed.ID
	// Stage 1: re-inspect. The plan may be seconds or minutes old, and the next
	// stage deletes something. The fresh observation is compared against the one
	// the plan was built from, so a container that changed — or a different
	// container that took the same reusable name — stops the sweep here rather
	// than being deleted and recreated from somebody else's configuration.
	actual, err := sweeper.containerByID(ctx, id)
	if err != nil {
		return sweeper.fail(report, id, StageReinspect, err)
	}
	if actual == nil {
		return sweeper.fail(report, id, StageReinspect, fmt.Errorf(
			"container disappeared between planning and migration",
		))
	}
	if err := requireUnchangedSincePlan(plan.Observed, *actual); err != nil {
		return sweeper.fail(report, id, StageReinspect, err)
	}
	if current := InventoryContainer(*actual); current.Disposition !=
		MigrationPending {
		return sweeper.fail(report, id, StageReinspect, fmt.Errorf(
			"container is now %q, not a migration target", current.Disposition,
		))
	}
	// The specification carries a mutable image reference, so a tag rebuilt
	// since this container was created would make the "replacement" different
	// content. That is a redeployment, and without this check it would only be
	// discovered by the equivalence gate — after the irreversible delete.
	if err := sweeper.requireUnmovedImage(ctx, *actual); err != nil {
		return sweeper.fail(report, id, StageReinspect, err)
	}
	// The specification is the one captured in the plan, not a fresh derivation.
	spec := plan.Spec
	volumes := NamedVolumeAttachments(plan.Observed)
	wasRunning := plan.Observed.Status.State == "running"
	sweeper.ok(report, id, StageReinspect, fmt.Sprintf(
		"unchanged since the snapshot, state=%s, named volumes=%d",
		plan.Observed.Status.State, len(volumes),
	))

	// Stage 2: stop. Skipped when the container is already stopped, so a
	// deliberately stopped topology is never started by a relabelling.
	//
	// Ownership is re-proven immediately before stopping. Apple Container
	// identifies containers by a reusable name, so between the re-inspection
	// above and this call another actor could have replaced the name with a
	// container this binary does not own. The window cannot be closed entirely
	// without an immutable generation identifier the runtime does not expose,
	// but it is narrowed to a single call and the stop is refused outright when
	// the name no longer resolves to something owned.
	if wasRunning {
		if err := sweeper.requireStillPlanned(
			ctx, plan.Observed, plan.Observed.Status.State,
		); err != nil {
			return sweeper.fail(report, id, StageStop, err)
		}
		if err := sweeper.Runtime.Stop(
			ctx, id, sweeper.StopTimeoutSeconds,
		); err != nil {
			return sweeper.fail(report, id, StageStop, err)
		}
		sweeper.changed(report, id, StageStop, "graceful stop completed")
	} else {
		sweeper.ok(report, id, StageStop, "already stopped; not started")
	}

	// Stage 3: delete the exact resolved identity. Delete re-proves ownership
	// itself, so a container that changed underneath us still cannot be
	// removed by this path. It is also a no-op when the name has already
	// vanished, which is not success here: something else is mutating the same
	// container, and continuing would recreate a container from a snapshot of a
	// world that no longer exists.
	if err := sweeper.requireStillPlanned(ctx, plan.Observed, "stopped"); err != nil {
		return sweeper.fail(report, id, StageDelete, err)
	}
	if err := sweeper.Runtime.DeleteExisting(ctx, id); err != nil {
		return sweeper.fail(report, id, StageDelete, err)
	}
	sweeper.changed(report, id, StageDelete, "exact container deleted; "+
		"named volumes untouched")

	// Stage 4: prove the named volumes are released before re-attaching them.
	// Apple Container attaches a named volume to one virtual machine
	// exclusively; attaching before the previous holder is gone fails the
	// replacement instead of the migration, which is much harder to diagnose.
	if err := sweeper.proveVolumeRelease(ctx, id, volumes); err != nil {
		return sweeper.fail(report, id, StageVolumeRelease, err)
	}
	sweeper.ok(report, id, StageVolumeRelease, fmt.Sprintf(
		"%d named volume(s) released and still present", len(volumes),
	))

	// Stage 5: recreate in the state the original was observed in.
	if err := sweeper.materialize(
		ctx, spec, plan.Observed, volumes, wasRunning, report,
	); err != nil {
		return sweeper.fail(report, id, StageRecreate, err)
	}
	sweeper.changed(report, id, StageRecreate, fmt.Sprintf(
		"replacement materialized with both ownership namespaces (running=%t)",
		wasRunning,
	))

	// Stage 6: prove the replacement differs only by the gained namespace.
	replacement, err := sweeper.containerByID(ctx, id)
	if err != nil {
		return sweeper.fail(report, id, StageVerify, err)
	}
	if replacement == nil {
		return sweeper.fail(report, id, StageVerify, fmt.Errorf(
			"replacement is not present after recreation",
		))
	}
	if err := VerifyMigrationEquivalence(*actual, *replacement); err != nil {
		// Not repairing an unprovable replacement is deliberate, but it is not
		// the same as leaving it running. A replacement that cannot be proven
		// equivalent is quarantined so it cannot serve traffic or hold its
		// volume while an operator decides what to do; it is never deleted,
		// because it is the only remaining copy of that configuration.
		if wasRunning {
			if stopErr := sweeper.Runtime.Stop(
				ctx, id, sweeper.StopTimeoutSeconds,
			); stopErr != nil {
				return sweeper.fail(report, id, StageVerify, fmt.Errorf(
					"%w; the unproven replacement could not be quarantined: %v",
					err, stopErr,
				))
			}
			sweeper.changed(report, id, StageVerify,
				"unproven replacement stopped pending an operator decision")
		}
		return sweeper.fail(report, id, StageVerify, err)
	}
	sweeper.ok(report, id, StageVerify,
		"replacement is equivalent and dual-labeled")
	// The verified post-state is retained so the final audit can confirm the
	// record still carrying this identity is the one that was proven, rather
	// than relabelling whatever answers to the name by then.
	verified[id] = *replacement
	return nil
}

// exclusiveAttachPattern matches the way Apple Container reports that a named
// volume is still attached to a virtual machine that has not finished tearing
// down. The container listing can already show the previous holder as gone
// while the block device is still attached, so listing-based release is a
// necessary precondition and not a proof; this is the signal that says so.
var exclusiveAttachPattern = regexp.MustCompile(
	`(?i)(vz\s*(error)?\s*code\s*=\s*2|already (in use|attached)|resource busy|device or resource busy|attach(ment)? (failed|busy))`,
)

// materialize creates the replacement, retrying a bounded number of times when
// the runtime reports the named volume is still exclusively attached.
//
// The release proof before this point is derived from listings, and Apple
// Container removes a container record before its virtual machine has finished
// releasing the block device. Rather than claim a stronger proof than the
// runtime offers, the sweep treats that specific failure as transient.
//
// Reconciliation is fail-closed, and deliberately never deletes. Apple
// Container names carry no generation identifier, so a record answering to this
// name after a failed create cannot be attributed to that failed create:
// another actor could have taken the name in the gap. The only two safe
// readings are "this is indistinguishable from the replacement I intended, so
// accept it" and "I cannot account for this, so stop and leave it alone".
func (sweeper *LabelSweeper) materialize(
	ctx context.Context,
	spec ContainerSpec,
	observed ContainerActual,
	volumes []VolumeAttachment,
	wasRunning bool,
	report *SweepReport,
) error {
	attempts := sweeper.RecreateAttempts
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		// The name has to be free before each attempt. Creating over a record
		// this sweep did not make is not its call to make.
		if err := sweeper.requireNameFree(ctx, spec.Name); err != nil {
			return err
		}
		var err error
		if wasRunning {
			_, err = sweeper.Runtime.RunContainer(ctx, spec)
		} else {
			_, err = sweeper.Runtime.CreateContainer(ctx, spec)
		}
		if err == nil {
			return nil
		}
		lastErr = err
		if !exclusiveAttachPattern.MatchString(err.Error()) ||
			attempt == attempts {
			return err
		}
		// A busy create may still have left a record — or somebody else may now
		// hold the name. The runtime cannot tell those apart, so neither can
		// this code, and neither may delete.
		existing, lookupErr := sweeper.containerByID(ctx, spec.Name)
		if lookupErr != nil {
			return lookupErr
		}
		if existing != nil {
			if equivalenceErr := VerifyMigrationEquivalence(
				observed, *existing,
			); equivalenceErr != nil {
				return fmt.Errorf(
					"container %s reported the named volume busy and a record "+
						"now holds that name which this sweep cannot account "+
						"for (%v); it has been left untouched for an operator",
					spec.Name, equivalenceErr,
				)
			}
			// Indistinguishable from the replacement that was intended: the
			// create landed after all. Stage 6 verifies it again on the way out.
			sweeper.changed(report, spec.Name, StageRecreate, fmt.Sprintf(
				"attempt %d reported the volume busy but left a record "+
					"matching the intended replacement; accepted without "+
					"recreating", attempt,
			))
			return nil
		}
		// The name is free. Retrying is only meaningful when a named volume
		// could still be holding the attachment open.
		if len(volumes) == 0 {
			return err
		}
		if releaseErr := sweeper.proveVolumeRelease(
			ctx, spec.Name, volumes,
		); releaseErr != nil {
			return releaseErr
		}
		sweeper.ok(report, spec.Name, StageRecreate, fmt.Sprintf(
			"attempt %d found the named volume still attached and the name "+
				"free; re-proved release and retrying", attempt,
		))
	}
	return lastErr
}

// requireNameFree proves nothing answers to this identity yet.
func (sweeper *LabelSweeper) requireNameFree(
	ctx context.Context,
	name string,
) error {
	existing, err := sweeper.containerByID(ctx, name)
	if err != nil {
		return err
	}
	if existing != nil {
		return fmt.Errorf(
			"container %s already exists; refusing to create over a record "+
				"this sweep cannot attribute to itself", name,
		)
	}
	return nil
}

// proveVolumeRelease polls until the deleted container is gone from the
// listing and no surviving container still mounts the volumes it held, then
// proves each volume still exists. Both halves matter: the first is the
// exclusivity precondition, the second is the promise that Phase 2 never
// destroys data.
func (sweeper *LabelSweeper) proveVolumeRelease(
	ctx context.Context,
	deleted string,
	volumes []VolumeAttachment,
) error {
	deadline := time.Now().Add(sweeper.ReleaseTimeout)
	ticker := time.NewTicker(sweeper.ReleasePollInterval)
	defer ticker.Stop()
	for {
		released, reason, err := sweeper.volumesReleased(ctx, deleted, volumes)
		if err != nil {
			return err
		}
		if released {
			return sweeper.proveVolumesPresent(ctx, volumes)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf(
				"named volumes were not released within %s: %s",
				sweeper.ReleaseTimeout, reason,
			)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("volume release wait: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func (sweeper *LabelSweeper) volumesReleased(
	ctx context.Context,
	deleted string,
	volumes []VolumeAttachment,
) (bool, string, error) {
	containers, err := sweeper.Runtime.Containers(ctx)
	if err != nil {
		return false, "", err
	}
	holders := make(map[string]string, len(volumes))
	for _, container := range containers {
		if container.ID == deleted {
			return false, fmt.Sprintf(
				"container %s is still present", deleted,
			), nil
		}
		for _, attachment := range NamedVolumeAttachments(container) {
			holders[attachment.Name] = container.ID
		}
	}
	for _, attachment := range volumes {
		if holder, held := holders[attachment.Name]; held {
			return false, fmt.Sprintf(
				"volume %s is still attached to container %s",
				attachment.Name, holder,
			), nil
		}
	}
	return true, "", nil
}

func (sweeper *LabelSweeper) proveVolumesPresent(
	ctx context.Context,
	volumes []VolumeAttachment,
) error {
	if len(volumes) == 0 {
		return nil
	}
	present, err := sweeper.volumeNames(ctx)
	if err != nil {
		return err
	}
	for _, attachment := range volumes {
		if !present[attachment.Name] {
			return fmt.Errorf(
				"named volume %s disappeared during migration", attachment.Name,
			)
		}
	}
	return nil
}

func (sweeper *LabelSweeper) volumeNames(
	ctx context.Context,
) (map[string]bool, error) {
	volumes, err := sweeper.Runtime.Volumes(ctx)
	if err != nil {
		return nil, err
	}
	names := make(map[string]bool, len(volumes))
	for _, volume := range volumes {
		name := volume.ID
		if volume.Configuration.Name != "" {
			name = volume.Configuration.Name
		}
		names[name] = true
	}
	return names, nil
}

// preservation reports, for every volume observed before the sweep, whether it
// is still there afterwards. A best-effort listing failure is reported as
// absent-after rather than silently dropped.
func (sweeper *LabelSweeper) preservation(
	ctx context.Context,
	before map[string]bool,
	report *SweepReport,
) []VolumePreservation {
	after, err := sweeper.volumeNames(ctx)
	if err != nil {
		// Recording "presentAfter: false" here would write the strongest
		// possible false claim into durable evidence — that every named volume,
		// including the database's, was destroyed. An unreadable listing is not
		// an absent volume, so it is reported as what it is.
		report.VolumeListingError = err.Error()
		return nil
	}
	names := make([]string, 0, len(before))
	for name := range before {
		names = append(names, name)
	}
	records := make([]VolumePreservation, 0, len(names))
	for _, name := range sortedStrings(names) {
		records = append(records, VolumePreservation{
			Name:          name,
			PresentBefore: true,
			PresentAfter:  after[name],
		})
	}
	return records
}

func (sweeper *LabelSweeper) containerByID(
	ctx context.Context,
	id string,
) (*ContainerActual, error) {
	containers, err := sweeper.Runtime.Containers(ctx)
	if err != nil {
		return nil, err
	}
	for index := range containers {
		if containers[index].ID == id {
			return &containers[index], nil
		}
	}
	return nil, nil
}

func (sweeper *LabelSweeper) ok(
	report *SweepReport,
	container string,
	stage SweepStage,
	detail string,
) {
	report.Steps = append(report.Steps, SweepStep{
		Container: container, Stage: stage, Outcome: "ok", Detail: detail,
	})
}

// changed records a step that actually altered the host.
func (sweeper *LabelSweeper) changed(
	report *SweepReport,
	container string,
	stage SweepStage,
	detail string,
) {
	report.Steps = append(report.Steps, SweepStep{
		Container: container, Stage: stage, Outcome: "ok", Detail: detail,
		Mutating: true,
	})
}

// mutated reports whether any step changed the host.
func mutated(report SweepReport) bool {
	for _, step := range report.Steps {
		if step.Mutating {
			return true
		}
	}
	return false
}

func (sweeper *LabelSweeper) fail(
	report *SweepReport,
	container string,
	stage SweepStage,
	err error,
) error {
	report.Steps = append(report.Steps, SweepStep{
		Container: container, Stage: stage, Outcome: "failed",
		Detail: err.Error(),
	})
	return err
}
