package lifecycle

import (
	"context"
	"errors"
)

// Phase 2's container label sweep is retired as of Phase 3A of Issue #123. What
// remains here is the read-only inventory it was built around, plus the schemas
// needed to read the evidence the sweep already wrote.
//
// The sweep migrated a container by deleting it and recreating it from its own
// observed specification. That was sound while the writer emitted both
// ownership namespaces: a legacy-only container came back dual, which is what
// its verification required. Phase 3A stops writing the legacy namespace, so the
// identical code would carry a legacy-only container to current-only in one
// destructive step — the jump Issue #123 forbids — and would fail its own
// verification only after the original had already been deleted.
//
// The implementation is removed rather than left behind a flag. Destructive code
// that no caller can reach is code no test can honestly exercise, and an
// unexercised delete-and-recreate path sitting next to a live migration is an
// invitation to re-enable it without re-deriving why it was withdrawn.
//
// `agentopsctl migrate-label-metadata` replaced it with two reviewable stages
// that moved the same labels and deleted nothing. Phase 3B retired those stages
// too, because both existed to compare the two namespaces. No forward container
// label migration remains in this binary; what remains is that command's
// `--rollback`.

// SweepRuntime is what the read-only inventory needs. It was much wider while
// the sweep could mutate; everything the mutation required has gone with it.
type SweepRuntime interface {
	Containers(ctx context.Context) ([]ContainerActual, error)
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

// LabelSweeper takes the Phase 2 inventory. Its mutating half is retired.
//
// It carried an `Only` field through Phase 3A so a caller narrowing a sweep
// would reach the refusal rather than a compile error. Nothing reads it: the CLI
// rejects a non-empty `--only` before it would be assigned, so the field was
// always the empty slice, and a field that cannot hold a value is not a
// migration aid.
type LabelSweeper struct {
	Runtime SweepRuntime
}

func NewLabelSweeper(runtime SweepRuntime) *LabelSweeper {
	return &LabelSweeper{Runtime: runtime}
}

// Plan takes a read-only inventory. It mutates nothing and never starts the
// runtime; it is how an operator reads the phase gate.
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

// ErrLabelSweepRetired marks the Phase 2 container sweep as withdrawn.
var ErrLabelSweepRetired = errors.New(
	"the Phase 2 container label sweep is retired as of Phase 3A of " +
		"Issue #123: it deleted and recreated containers, which now produces a " +
		"current-only replacement in one step and skips the staged migration " +
		"the epic required. Its staged replacement was retired in turn by " +
		"Phase 3B, which removed every read of the legacy namespace; no " +
		"forward container label migration remains in this binary",
)

// Apply is retired. It refuses before reading anything from the runtime, so no
// caller can reach a destructive path and none can be made to start the
// services as a side effect of trying.
func (sweeper *LabelSweeper) Apply(context.Context) (SweepReport, error) {
	return SweepReport{
		Applied: false,
		Halted:  ErrLabelSweepRetired.Error(),
	}, ErrLabelSweepRetired
}
