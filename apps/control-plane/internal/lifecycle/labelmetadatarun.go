package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// This file is the operator-facing shape of Phase 3A's sweep: an inventory that
// classifies every managed resource, a plan naming exact targets, and a staged
// application that stops the runtime, rewrites metadata, starts it again, and
// proves the result through the runtime's own API rather than through the files
// it just wrote.

// OwnershipInventoryRecord is one resource's ownership as the runtime reports
// it. It carries no host path: the durable evidence records identities and
// classes, never locations on the operator's disk.
type OwnershipInventoryRecord struct {
	Kind   MetadataResourceKind `json:"kind"`
	ID     string               `json:"id"`
	Class  OwnershipClass       `json:"class"`
	State  string               `json:"state,omitempty"`
	Labels map[string]string    `json:"ownershipLabels"`
}

// OwnershipInventory is the whole host, classified.
type OwnershipInventory struct {
	Records []OwnershipInventoryRecord   `json:"records"`
	Totals  map[OwnershipClass]int       `json:"totals"`
	ByKind  map[MetadataResourceKind]int `json:"byKind"`
	Managed map[MetadataResourceKind]int `json:"managed"`
	Legacy  []OwnershipInventoryRecord   `json:"legacyOnly"`
}

// TakeOwnershipInventory classifies every container, volume, and network.
func TakeOwnershipInventory(
	ctx context.Context,
	runtime *AppleRuntime,
) (*OwnershipInventory, error) {
	inventory := &OwnershipInventory{
		Totals:  make(map[OwnershipClass]int),
		ByKind:  make(map[MetadataResourceKind]int),
		Managed: make(map[MetadataResourceKind]int),
	}
	containers, err := runtime.Containers(ctx)
	if err != nil {
		return nil, err
	}
	for _, container := range containers {
		inventory.add(OwnershipInventoryRecord{
			Kind:   MetadataKindContainer,
			ID:     container.ID,
			Class:  ClassifyOwnership(container.Configuration.Labels),
			State:  container.Status.State,
			Labels: ownershipLabelSubset(container.Configuration.Labels),
		})
	}
	for kind, list := range map[MetadataResourceKind]func(
		context.Context,
	) ([]Resource, error){
		MetadataKindVolume:  runtime.Volumes,
		MetadataKindNetwork: runtime.Networks,
	} {
		resources, err := list(ctx)
		if err != nil {
			return nil, err
		}
		for _, resource := range resources {
			identity := resource.ID
			if resource.Configuration.Name != "" {
				identity = resource.Configuration.Name
			}
			inventory.add(OwnershipInventoryRecord{
				Kind:   kind,
				ID:     identity,
				Class:  ClassifyOwnership(resource.Configuration.Labels),
				Labels: ownershipLabelSubset(resource.Configuration.Labels),
			})
		}
	}
	sort.Slice(inventory.Records, func(first, second int) bool {
		if inventory.Records[first].Kind != inventory.Records[second].Kind {
			return inventory.Records[first].Kind < inventory.Records[second].Kind
		}
		return inventory.Records[first].ID < inventory.Records[second].ID
	})
	return inventory, nil
}

func (inventory *OwnershipInventory) add(record OwnershipInventoryRecord) {
	inventory.Records = append(inventory.Records, record)
	inventory.Totals[record.Class]++
	inventory.ByKind[record.Kind]++
	if record.Class.Owned() {
		inventory.Managed[record.Kind]++
	}
	if record.Class == OwnershipLegacyOnly {
		inventory.Legacy = append(inventory.Legacy, record)
	}
}

// ManagedTargets returns every owned resource, which is the complete set the
// sweep may act on.
func (inventory *OwnershipInventory) ManagedTargets() []MetadataTargetRef {
	targets := make([]MetadataTargetRef, 0, len(inventory.Records))
	for _, record := range inventory.Records {
		if record.Class.Owned() {
			targets = append(
				targets, MetadataTargetRef{Kind: record.Kind, ID: record.ID},
			)
		}
	}
	return targets
}

// RunningManagedContainers lists owned containers that are not stopped.
func (inventory *OwnershipInventory) RunningManagedContainers() []string {
	running := make([]string, 0)
	for _, record := range inventory.Records {
		if record.Kind != MetadataKindContainer || !record.Class.Owned() {
			continue
		}
		if record.State != "stopped" {
			running = append(running, record.ID)
		}
	}
	return running
}

// RequireConflictFree stops before any stage when a resource is partially
// migrated. A conflict is neither owned nor foreign, and the runbook sends the
// operator to `container inspect` rather than to this tool.
func (inventory *OwnershipInventory) RequireConflictFree() error {
	conflicting := make([]string, 0)
	for _, record := range inventory.Records {
		if record.Class == OwnershipConflicting {
			conflicting = append(
				conflicting, string(record.Kind)+" "+record.ID,
			)
		}
	}
	if len(conflicting) > 0 {
		return fmt.Errorf(
			"%d resource(s) carry conflicting ownership labels (%s); %w",
			len(conflicting), strings.Join(conflicting, ", "),
			ErrConflictingLabels,
		)
	}
	return nil
}

// RequireCurrentOwnershipEverywhere is the gate between the two stages. Retiring
// the legacy pair while any managed resource still carries it alone would strip
// that resource's only ownership marker.
func (inventory *OwnershipInventory) RequireCurrentOwnershipEverywhere() error {
	if len(inventory.Legacy) == 0 {
		return nil
	}
	names := make([]string, 0, len(inventory.Legacy))
	for _, record := range inventory.Legacy {
		names = append(names, string(record.Kind)+" "+record.ID)
	}
	return fmt.Errorf(
		"%d managed resource(s) still carry the legacy namespace alone (%s); "+
			"run the prepare stage before retiring",
		len(names), strings.Join(names, ", "),
	)
}

// MetadataTargetRef is one exact resource an operator named.
type MetadataTargetRef struct {
	Kind MetadataResourceKind `json:"kind"`
	ID   string               `json:"id"`
}

func (ref MetadataTargetRef) String() string {
	return string(ref.Kind) + "/" + ref.ID
}

// ParseMetadataTargetRef reads the `kind/identity` spelling an operator passes
// to --only. The kind is required: a bare name would be ambiguous the moment a
// volume and a network share one, and this migration never guesses which
// resource an operator meant.
func ParseMetadataTargetRef(raw string) (MetadataTargetRef, error) {
	kind, identity, found := strings.Cut(strings.TrimSpace(raw), "/")
	if !found {
		return MetadataTargetRef{}, fmt.Errorf(
			"%q is not a target; write it as container/<id>, volume/<name>, "+
				"or network/<name>",
			raw,
		)
	}
	reference := MetadataTargetRef{
		Kind: MetadataResourceKind(strings.TrimSpace(kind)),
		ID:   strings.TrimSpace(identity),
	}
	if _, err := layoutFor(reference.Kind); err != nil {
		return MetadataTargetRef{}, err
	}
	if reference.ID == "" {
		return MetadataTargetRef{}, fmt.Errorf("%q names no resource", raw)
	}
	return reference, nil
}

// RequireDistinctTargets rejects a repeated target. A duplicate would make the
// number of resources the run touched ambiguous, and an ambiguous count is not
// something an audit can be written from.
func RequireDistinctTargets(targets []MetadataTargetRef) error {
	seen := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		key := target.String()
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("target %s is named more than once", key)
		}
		seen[key] = struct{}{}
	}
	if len(targets) == 0 {
		return fmt.Errorf(
			"no targets were named; this migration never derives its own " +
				"blast radius from the host's current contents",
		)
	}
	return nil
}

// MetadataDocumentPlan describes one document a stage would rewrite. The path is
// relative to the application root so durable evidence never records where the
// operator's home directory is.
type MetadataDocumentPlan struct {
	RelativePath string `json:"relativePath"`
	BeforeSHA256 string `json:"beforeSha256"`
}

// MetadataPlanEntry is one resource's planned change.
type MetadataPlanEntry struct {
	Kind        MetadataResourceKind   `json:"kind"`
	ID          string                 `json:"id"`
	State       string                 `json:"state,omitempty"`
	ClassBefore OwnershipClass         `json:"classBefore"`
	ClassAfter  OwnershipClass         `json:"classAfter"`
	ChangedKeys []string               `json:"changedKeys"`
	Documents   []MetadataDocumentPlan `json:"documents"`
	Satisfied   bool                   `json:"alreadySatisfied"`
}

// MetadataSweepPlan is what an operator approves before anything is written.
type MetadataSweepPlan struct {
	Stage       MetadataStage       `json:"stage"`
	Host        MetadataHost        `json:"host"`
	Entries     []MetadataPlanEntry `json:"entries"`
	ChangeCount int                 `json:"changeCount"`
}

// redactAppRoot renders a path relative to the application root. Evidence has to
// be publishable in a pull request, and the default application root sits under
// the operator's home directory.
func redactAppRoot(appRoot, path string) string {
	relative, err := filepath.Rel(appRoot, path)
	if err != nil {
		return filepath.Base(path)
	}
	return relative
}

// PlanMetadataSweep resolves and verifies every named target without changing
// anything. It is the read-only rehearsal the decision gate is built from.
func PlanMetadataSweep(
	host *MetadataHost,
	stage MetadataStage,
	targets []MetadataTargetRef,
	states map[string]string,
) (*MetadataSweepPlan, error) {
	if err := RequireDistinctTargets(targets); err != nil {
		return nil, err
	}
	plan := &MetadataSweepPlan{Stage: stage, Host: *host}
	for _, reference := range targets {
		target, err := resolveMetadataTarget(
			host.AppRoot, reference.Kind, reference.ID,
		)
		if err != nil {
			return nil, err
		}
		state, err := readMetadataTarget(target)
		if err != nil {
			return nil, err
		}
		planned, err := planOwnershipLabels(state.Labels, stage)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", reference, err)
		}
		changed := changedLabelKeys(state.Labels, planned)
		entry := MetadataPlanEntry{
			Kind:        reference.Kind,
			ID:          reference.ID,
			State:       states[reference.String()],
			ClassBefore: state.Class,
			ClassAfter:  ClassifyOwnership(planned),
			ChangedKeys: changed,
			Satisfied:   len(changed) == 0,
		}
		for _, file := range state.Files {
			entry.Documents = append(entry.Documents, MetadataDocumentPlan{
				RelativePath: redactAppRoot(host.AppRoot, file.Ref.Path),
				BeforeSHA256: file.SHA256,
			})
		}
		if !entry.Satisfied {
			plan.ChangeCount++
		}
		plan.Entries = append(plan.Entries, entry)
	}
	return plan, nil
}

// MetadataSweepReport is the durable account of one applied stage.
type MetadataSweepReport struct {
	Stage      MetadataStage          `json:"stage"`
	Host       MetadataHost           `json:"host"`
	Applied    []*MetadataApplication `json:"applied"`
	Skipped    []MetadataTargetRef    `json:"skipped"`
	Halted     string                 `json:"halted,omitempty"`
	RolledBack bool                   `json:"rolledBack"`
	// BackupRoot is absolute and private; the evidence records only its base
	// name so a reader can find it without the file naming the operator's home.
	BackupRoot     string `json:"-"`
	BackupRootName string `json:"backupRootName"`
	VerifiedByAPI  bool   `json:"verifiedByApi"`
}

// ApplyMetadataSweep rewrites every planned target while the runtime is down.
// The caller is responsible for stopping and starting the services; this
// function refuses to run unless they are already proven stopped, so the proof
// and the mutation cannot drift apart.
func ApplyMetadataSweep(
	ctx context.Context,
	runtime *AppleRuntime,
	host *MetadataHost,
	stage MetadataStage,
	targets []MetadataTargetRef,
	backupRoot string,
) (*MetadataSweepReport, error) {
	if err := RequireDistinctTargets(targets); err != nil {
		return nil, err
	}
	if err := RequireServicesStopped(ctx, runtime.runner); err != nil {
		return nil, err
	}
	report := &MetadataSweepReport{
		Stage: stage, Host: *host, BackupRoot: backupRoot,
		BackupRootName: filepath.Base(backupRoot),
	}
	for _, reference := range targets {
		target, err := resolveMetadataTarget(
			host.AppRoot, reference.Kind, reference.ID,
		)
		if err != nil {
			report.Halted = err.Error()
			return report, err
		}
		state, err := readMetadataTarget(target)
		if err != nil {
			report.Halted = err.Error()
			return report, err
		}
		planned, err := planOwnershipLabels(state.Labels, stage)
		if err != nil {
			report.Halted = fmt.Sprintf("%s: %v", reference, err)
			return report, fmt.Errorf("%s: %w", reference, err)
		}
		if len(changedLabelKeys(state.Labels, planned)) == 0 {
			// Re-running a stage over a resource it already migrated is how an
			// interrupted sweep resumes, so it has to be a no-op rather than an
			// error.
			report.Skipped = append(report.Skipped, reference)
			continue
		}
		application, err := applyMetadataStage(
			target, stage, backupRoot, host.AppRoot,
		)
		if err != nil {
			report.Halted = err.Error()
			return report, err
		}
		report.Applied = append(report.Applied, application)
	}
	return report, nil
}

// VerifyMetadataSweep re-reads the runtime's own view after a restart and
// proves it matches what the sweep intended. Checking the files this run just
// wrote would only prove the writer agreed with itself; the question that
// matters is whether Apple Container now reports the labels the migration was
// for.
func VerifyMetadataSweep(
	ctx context.Context,
	runtime *AppleRuntime,
	report *MetadataSweepReport,
) error {
	inventory, err := TakeOwnershipInventory(ctx, runtime)
	if err != nil {
		return err
	}
	observed := make(map[string]OwnershipInventoryRecord, len(inventory.Records))
	for _, record := range inventory.Records {
		observed[MetadataTargetRef{Kind: record.Kind, ID: record.ID}.String()] =
			record
	}
	for _, application := range report.Applied {
		key := MetadataTargetRef{
			Kind: application.Kind, ID: application.ID,
		}.String()
		record, present := observed[key]
		if !present {
			return fmt.Errorf(
				"%s is no longer reported by the runtime after the sweep", key,
			)
		}
		if record.Class != application.ClassAfter {
			return fmt.Errorf(
				"%s reports ownership class %q after the sweep, expected %q",
				key, record.Class, application.ClassAfter,
			)
		}
		for key, want := range application.AfterLabels {
			if _, owned := ownershipKeySet()[key]; !owned {
				continue
			}
			if record.Labels[key] != want {
				return fmt.Errorf(
					"%s label %s reads %q, expected %q",
					application.ID, key, record.Labels[key], want,
				)
			}
		}
		for key := range record.Labels {
			if _, present := application.AfterLabels[key]; !present {
				return fmt.Errorf(
					"%s still reports retired label %s", application.ID, key,
				)
			}
		}
	}
	report.VerifiedByAPI = true
	return nil
}

// RollbackPlan is the private companion to the committed evidence. It carries
// the absolute locations rollback needs, which the evidence deliberately omits,
// and it lives inside the 0700 backup root beside the backups it names.
type RollbackPlan struct {
	Stage   MetadataStage          `json:"stage"`
	Applied []*MetadataApplication `json:"applied"`
	// Locations mirrors the absolute paths that MetadataApplication hides from
	// the committed evidence, indexed the same way as Applied.
	Locations [][]RollbackLocation `json:"locations"`
}

// RollbackLocation is one document's absolute pair of locations.
type RollbackLocation struct {
	Path       string `json:"path"`
	BackupPath string `json:"backupPath"`
}

// BuildRollbackPlan lifts the absolute paths back out of the report so they can
// be written to the private plan.
func BuildRollbackPlan(report *MetadataSweepReport) *RollbackPlan {
	plan := &RollbackPlan{Stage: report.Stage, Applied: report.Applied}
	for _, application := range report.Applied {
		locations := make([]RollbackLocation, 0, len(application.Files))
		for _, file := range application.Files {
			locations = append(locations, RollbackLocation{
				Path: file.Path, BackupPath: file.BackupPath,
			})
		}
		plan.Locations = append(plan.Locations, locations)
	}
	return plan
}

// ParseRollbackPlan restores a plan and reattaches the absolute locations to the
// applications they belong to.
func ParseRollbackPlan(raw []byte) (*RollbackPlan, error) {
	var plan RollbackPlan
	if err := json.Unmarshal(raw, &plan); err != nil {
		return nil, fmt.Errorf("parse rollback plan: %w", err)
	}
	if len(plan.Applied) == 0 {
		return nil, fmt.Errorf("rollback plan records no applied change")
	}
	if len(plan.Locations) != len(plan.Applied) {
		return nil, fmt.Errorf(
			"rollback plan records %d resources but %d location sets",
			len(plan.Applied), len(plan.Locations),
		)
	}
	for index, application := range plan.Applied {
		locations := plan.Locations[index]
		if len(locations) != len(application.Files) {
			return nil, fmt.Errorf(
				"%s %s records %d documents but %d locations",
				application.Kind, application.ID,
				len(application.Files), len(locations),
			)
		}
		for fileIndex, location := range locations {
			application.Files[fileIndex].Path = location.Path
			application.Files[fileIndex].BackupPath = location.BackupPath
		}
		// Directory and BackupRoot are derived rather than stored twice, so the
		// plan cannot disagree with itself about where a resource lives.
		if len(locations) > 0 {
			application.Directory = filepath.Dir(locations[0].Path)
			application.BackupRoot = filepath.Dir(
				filepath.Dir(filepath.Dir(locations[0].BackupPath)),
			)
		}
	}
	return &plan, nil
}

// RollbackMetadataSweep restores every document a recorded run rewrote.
func RollbackMetadataSweep(plan *RollbackPlan) error {
	for index := len(plan.Applied) - 1; index >= 0; index-- {
		if err := rollbackMetadataApplication(plan.Applied[index]); err != nil {
			return fmt.Errorf(
				"%s %s: %w",
				plan.Applied[index].Kind, plan.Applied[index].ID, err,
			)
		}
	}
	return nil
}

// RestoreOutcomes renders how each of one resource's documents was restored.
func (application *MetadataApplication) RestoreOutcomes() []string {
	outcomes := make([]string, 0, len(application.Files))
	for _, file := range application.Files {
		outcomes = append(
			outcomes,
			fmt.Sprintf("%s: %s", file.Document, file.RestoredAs),
		)
	}
	for _, name := range application.Reconciled {
		outcomes = append(outcomes, name+": reconciled")
	}
	return outcomes
}
