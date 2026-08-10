package lifecycle

import (
	"context"
	"fmt"
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
	Stop(ctx context.Context, name string, seconds int) error
	Delete(ctx context.Context, name string) error
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
	Halted  string               `json:"halted,omitempty"`
}

// LabelSweeper migrates old-only containers to dual labels.
type LabelSweeper struct {
	Runtime             SweepRuntime
	StopTimeoutSeconds  int
	ReleasePollInterval time.Duration
	ReleaseTimeout      time.Duration
	// SnapshotBeforeMutation receives the exact pre-mutation audit the sweep is
	// about to act on. Issue #123 requires that snapshot to be durable before
	// the first mutation, so returning an error here aborts the sweep with
	// nothing changed: a migration whose evidence cannot be written is a
	// migration nobody can audit or roll back.
	SnapshotBeforeMutation func(MigrationAudit) error
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
	if sweeper.SnapshotBeforeMutation != nil {
		if err := sweeper.SnapshotBeforeMutation(before); err != nil {
			report.Applied = false
			report.Halted = "pre-mutation snapshot could not be recorded"
			return report, fmt.Errorf(
				"refusing to sweep without durable evidence: %w", err,
			)
		}
	}
	volumesBefore, err := sweeper.volumeNames(ctx)
	if err != nil {
		return report, err
	}
	for _, target := range targets {
		if stepErr := sweeper.migrateOne(ctx, target, &report); stepErr != nil {
			report.Halted = fmt.Sprintf(
				"halted at container %s: %v", target.ID, stepErr,
			)
			report.Volumes = sweeper.preservation(ctx, volumesBefore)
			return report, stepErr
		}
	}
	report.Volumes = sweeper.preservation(ctx, volumesBefore)
	after, err := sweeper.Plan(ctx, "post-migration")
	if err != nil {
		return report, err
	}
	report.After = after
	return report, nil
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
	planned ContainerInventoryRecord,
	report *SweepReport,
) error {
	// Stage 1: re-inspect. The plan may be seconds or minutes old, and the
	// next stage deletes something. Ownership, role, and specification
	// agreement are all re-proven against the exact resolved identity.
	actual, err := sweeper.containerByID(ctx, planned.ID)
	if err != nil {
		return sweeper.fail(report, planned.ID, StageReinspect, err)
	}
	if actual == nil {
		return sweeper.fail(report, planned.ID, StageReinspect, fmt.Errorf(
			"container disappeared between planning and migration",
		))
	}
	current := InventoryContainer(*actual)
	if current.Disposition != MigrationPending {
		return sweeper.fail(report, planned.ID, StageReinspect, fmt.Errorf(
			"container is now %q, not a migration target", current.Disposition,
		))
	}
	spec, err := RebuildMigratedSpec(*actual)
	if err != nil {
		return sweeper.fail(report, planned.ID, StageReinspect, err)
	}
	volumes := NamedVolumeAttachments(*actual)
	wasRunning := actual.Status.State == "running"
	sweeper.ok(report, planned.ID, StageReinspect, fmt.Sprintf(
		"legacy-only, state=%s, named volumes=%d",
		actual.Status.State, len(volumes),
	))

	// Stage 2: stop. Skipped when the container is already stopped, so a
	// deliberately stopped topology is never started by a relabelling.
	if wasRunning {
		if err := sweeper.Runtime.Stop(
			ctx, planned.ID, sweeper.StopTimeoutSeconds,
		); err != nil {
			return sweeper.fail(report, planned.ID, StageStop, err)
		}
		sweeper.ok(report, planned.ID, StageStop, "graceful stop completed")
	} else {
		sweeper.ok(report, planned.ID, StageStop, "already stopped; not started")
	}

	// Stage 3: delete the exact resolved identity. Delete re-proves ownership
	// itself, so a container that changed underneath us still cannot be
	// removed by this path.
	if err := sweeper.Runtime.Delete(ctx, planned.ID); err != nil {
		return sweeper.fail(report, planned.ID, StageDelete, err)
	}
	sweeper.ok(report, planned.ID, StageDelete, "exact container deleted; "+
		"named volumes untouched")

	// Stage 4: prove the named volumes are released before re-attaching them.
	// Apple Container attaches a named volume to one virtual machine
	// exclusively; attaching before the previous holder is gone fails the
	// replacement instead of the migration, which is much harder to diagnose.
	if err := sweeper.proveVolumeRelease(ctx, planned.ID, volumes); err != nil {
		return sweeper.fail(report, planned.ID, StageVolumeRelease, err)
	}
	sweeper.ok(report, planned.ID, StageVolumeRelease, fmt.Sprintf(
		"%d named volume(s) released and still present", len(volumes),
	))

	// Stage 5: recreate in the state the original was observed in.
	if wasRunning {
		_, err = sweeper.Runtime.RunContainer(ctx, spec)
	} else {
		_, err = sweeper.Runtime.CreateContainer(ctx, spec)
	}
	if err != nil {
		return sweeper.fail(report, planned.ID, StageRecreate, err)
	}
	sweeper.ok(report, planned.ID, StageRecreate, fmt.Sprintf(
		"replacement materialized with both ownership namespaces (running=%t)",
		wasRunning,
	))

	// Stage 6: prove the replacement differs only by the gained namespace.
	replacement, err := sweeper.containerByID(ctx, planned.ID)
	if err != nil {
		return sweeper.fail(report, planned.ID, StageVerify, err)
	}
	if replacement == nil {
		return sweeper.fail(report, planned.ID, StageVerify, fmt.Errorf(
			"replacement is not present after recreation",
		))
	}
	if err := VerifyMigrationEquivalence(*actual, *replacement); err != nil {
		return sweeper.fail(report, planned.ID, StageVerify, err)
	}
	sweeper.ok(report, planned.ID, StageVerify,
		"replacement is equivalent and dual-labeled")
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
) []VolumePreservation {
	after, err := sweeper.volumeNames(ctx)
	if err != nil {
		after = map[string]bool{}
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
