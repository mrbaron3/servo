package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// This file is the operator-facing shape of what survives Phase 3A's sweep: an
// inventory that classifies every managed resource from the current ownership
// namespace, and the recovery path that can return a resource to the labels a
// Phase 3A run recorded for it.
//
// The forward halves — the plan, the staged application, and the post-restart
// verification — are gone with Phase 3B. Each of them decided what to write by
// comparing the two namespaces, and there is now only one.

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
//
// Phase 3A carried a `legacyOnly` list here, which was the gate its retire stage
// read. Phase 3B removes it: a resource carrying only the retired namespace is
// no longer distinguishable from one carrying no ownership label at all, and
// publishing an always-empty list would suggest this binary still looks.
type OwnershipInventory struct {
	Records []OwnershipInventoryRecord   `json:"records"`
	Totals  map[OwnershipClass]int       `json:"totals"`
	ByKind  map[MetadataResourceKind]int `json:"byKind"`
	Managed map[MetadataResourceKind]int `json:"managed"`
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
	sortOwnershipInventory(inventory)
	return inventory, nil
}

// sortOwnershipInventory puts the listing into a stable order. It is emitted
// into committed evidence and populated while ranging over a map whose iteration
// order Go randomises per run, so without this the same unchanged host produces
// a different diff every time it is inventoried.
func sortOwnershipInventory(inventory *OwnershipInventory) {
	records := inventory.Records
	sort.Slice(records, func(first, second int) bool {
		if records[first].Kind != records[second].Kind {
			return records[first].Kind < records[second].Kind
		}
		return records[first].ID < records[second].ID
	})
}

func (inventory *OwnershipInventory) add(record OwnershipInventoryRecord) {
	inventory.Records = append(inventory.Records, record)
	inventory.Totals[record.Class]++
	inventory.ByKind[record.Kind]++
	if record.Class.Owned() {
		inventory.Managed[record.Kind]++
	}
}

// RunningIdentifiableContainers lists containers that are not stopped and that
// this binary has some claim on: owned, or incompletely labelled in the current
// namespace.
//
// Malformed is included deliberately. Through Phase 3A the equivalent gate
// counted owned containers only, and that was complete because every shape this
// binary could own was owned. Since Phase 3B a half-labelled container of ours
// classifies as malformed, and leaving it out would let a rollback stop the
// runtime underneath a container it is about to rewrite.
//
// Missing-label and unmanaged are still excluded, and that is not an oversight:
// stopping Apple Container affects the whole host, but blocking on `buildkit` or
// on another deployment's container would make rollback impossible on any real
// machine. This gate is therefore a courtesy check over containers this binary
// can identify — RequireServicesStopped, which demands two independent signals
// that the runtime is down, is what actually proves no document is rewritten
// under a live apiserver.
func (inventory *OwnershipInventory) RunningIdentifiableContainers() []string {
	running := make([]string, 0)
	for _, record := range inventory.Records {
		if record.Kind != MetadataKindContainer {
			continue
		}
		if !record.Class.Owned() && record.Class != OwnershipMalformed {
			continue
		}
		if record.State != "stopped" {
			running = append(running, record.ID)
		}
	}
	return running
}

// RollbackPlanPath is where a Phase 3A run's private rollback plan lives.
func RollbackPlanPath(backupRoot string) string {
	return filepath.Join(backupRoot, "rollback-plan.json")
}

// RollbackPlan is the private companion to the committed evidence. It carries
// the absolute locations rollback needs, which the evidence deliberately omits,
// and it lives inside the 0700 backup root beside the backups it names.
//
// Since Phase 3B this type is only ever read. Nothing writes a new plan, because
// nothing performs a forward migration any more.
type RollbackPlan struct {
	// Stage is the Phase 3A stage name the plan was written under. It is carried
	// verbatim and never matched against: see MetadataApplication.
	Stage   string                 `json:"stage"`
	Applied []*MetadataApplication `json:"applied"`
	// Locations mirrors the absolute paths that MetadataApplication hides from
	// the committed evidence, indexed the same way as Applied.
	Locations [][]RollbackLocation `json:"locations"`
	// Labels carries the COMPLETE label maps, foreign labels included. Rollback
	// restores a resource's labels exactly, and a map truncated to the ownership
	// keys would silently drop somebody else's label. They live here rather than
	// in the evidence because a foreign label's value is arbitrary third-party
	// text.
	Labels []RollbackLabels `json:"labels"`
}

// RollbackLabels is one resource's complete before and after label maps.
type RollbackLabels struct {
	Before map[string]string `json:"before"`
	After  map[string]string `json:"after"`
}

// RollbackLocation is one document's absolute pair of locations.
type RollbackLocation struct {
	Path       string `json:"path"`
	BackupPath string `json:"backupPath"`
}

// ParseRollbackPlan restores a plan and reattaches the absolute locations to the
// applications they belong to.
//
// Every consistency check here is on the plan's own internal shape — counts that
// must line up, maps that must be present. None of it inspects a label key: the
// plan may legitimately carry labels in a namespace this binary no longer reads,
// and restoring it faithfully is the entire point of keeping this path.
func ParseRollbackPlan(raw []byte) (*RollbackPlan, error) {
	var plan RollbackPlan
	if err := json.Unmarshal(raw, &plan); err != nil {
		return nil, fmt.Errorf("parse rollback plan: %w", err)
	}
	if len(plan.Applied) == 0 {
		return nil, fmt.Errorf("rollback plan records no applied change")
	}
	if len(plan.Locations) != len(plan.Applied) ||
		len(plan.Labels) != len(plan.Applied) {
		return nil, fmt.Errorf(
			"rollback plan records %d resources but %d location sets and %d "+
				"label sets",
			len(plan.Applied), len(plan.Locations), len(plan.Labels),
		)
	}
	for index, application := range plan.Applied {
		// The complete label maps are only in the plan, so they are reattached
		// before anything tries to restore from them. Without this a rollback
		// would write the ownership subset and drop every foreign label.
		application.BeforeLabels = plan.Labels[index].Before
		application.AfterLabels = plan.Labels[index].After
		if len(application.BeforeLabels) == 0 ||
			len(application.AfterLabels) == 0 {
			return nil, fmt.Errorf(
				"%s %s: rollback plan carries no label maps",
				application.Kind, application.ID,
			)
		}
		for _, file := range application.Files {
			file.BeforeLabels = application.BeforeLabels
			file.AfterLabels = application.AfterLabels
		}
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

// BindToHost proves a decoded plan describes documents belonging to this host,
// and nothing else, before any of it is acted on.
//
// This is the plan's trust boundary and it has to be drawn here. `ParseRollbackPlan`
// checks only that the plan is internally consistent — counts that line up, label
// maps that exist — and every path it carries is an absolute location read
// verbatim out of a file. A stale, hand-edited, or substituted plan would
// otherwise stop Apple Container and then rewrite any JSON file this user can
// write whose digests happened to match, selecting the field to overwrite
// through its own `labelPath`.
//
// The check is a reconstruction rather than a comparison: each document's path is
// rebuilt from the resolved application root plus the plan's own kind and
// identity, and the plan must agree with what was rebuilt. A plan cannot
// therefore name a location the layout does not put a document at, whatever its
// path field says.
//
// It runs before StopSystem. A rollback that refuses must not first take the
// operator's container runtime down.
func (plan *RollbackPlan) BindToHost(host *MetadataHost) error {
	if host == nil || strings.TrimSpace(host.AppRoot) == "" {
		return fmt.Errorf("rollback requires a resolved Apple Container host")
	}
	for _, application := range plan.Applied {
		layout, err := layoutFor(application.Kind)
		if err != nil {
			return err
		}
		// The identity indexes a directory, so it may not be able to escape one.
		identity := application.ID
		if identity == "" || identity == "." || identity == ".." ||
			strings.ContainsRune(identity, os.PathSeparator) ||
			strings.ContainsRune(identity, '/') ||
			identity != filepath.Clean(identity) {
			return fmt.Errorf(
				"%q is not an exact %s identity", identity, application.Kind,
			)
		}
		directory := filepath.Join(host.AppRoot, layout.directory, identity)
		if err := requireNoSymlinkInPath(directory, host.AppRoot); err != nil {
			return err
		}
		for _, file := range application.Files {
			name := filepath.Base(file.Path)
			labelPath, known := layout.documents[name]
			if !known {
				return fmt.Errorf(
					"%s %s: %q is not a document this migration knows about",
					application.Kind, identity, name,
				)
			}
			expected := filepath.Join(directory, name)
			if file.Path != expected {
				return fmt.Errorf(
					"%s %s: the plan names %s, which is not where this host keeps "+
						"that document", application.Kind, identity, file.Path,
				)
			}
			// The label path decides which field is overwritten. Taking it from
			// the layout rather than trusting the plan is what stops a forged
			// plan from rewriting a non-label field.
			if !equalStringSlices(file.LabelPath, labelPath) {
				return fmt.Errorf(
					"%s %s: the plan puts %s's labels somewhere this host does not",
					application.Kind, identity, name,
				)
			}
			if err := requireNoSymlinkInPath(file.Path, host.AppRoot); err != nil {
				return err
			}
			if err := requireBackupRootIsPrivate(file.BackupPath); err != nil {
				return err
			}
		}
	}
	return nil
}

// requireBackupRootIsPrivate refuses a backup location inside a git work tree.
// Rollback writes new backups of its own — a document that appeared after the
// migration is copied before it is brought back in line — so this is not a check
// on somebody else's past behaviour but on where this run is about to write a
// verbatim copy of a container's environment.
func requireBackupRootIsPrivate(backupPath string) error {
	if strings.TrimSpace(backupPath) == "" {
		return fmt.Errorf("the plan records a document with no backup location")
	}
	resolved, err := resolveExistingAncestor(filepath.Dir(backupPath))
	if err != nil {
		return err
	}
	return requireOutsideGitWorkTree(resolved)
}

func equalStringSlices(first, second []string) bool {
	if len(first) != len(second) {
		return false
	}
	for index := range first {
		if first[index] != second[index] {
			return false
		}
	}
	return true
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
