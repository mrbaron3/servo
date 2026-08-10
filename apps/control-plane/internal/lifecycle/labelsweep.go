package lifecycle

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Phase 3A moves managed resources onto the current ownership namespace in two
// stages, and the order is the safety property. `prepare` adds the current pair
// beside the legacy one so every resource is dual and therefore readable by
// both namespaces at once; `retire` then removes the legacy pair. Doing it in
// one step would mean that between writing and verifying, a resource carried
// neither a proven-good legacy marker nor a proven-good current one.
//
// Nothing here deletes or recreates a resource. Apple Container attaches a
// named volume exclusively to a single virtual machine, and recreating a volume
// to change its labels is the same act as destroying its data.

// MetadataStage names one half of the migration.
type MetadataStage string

const (
	// MetadataStagePrepare adds the current pair beside an existing legacy pair.
	MetadataStagePrepare MetadataStage = "prepare"
	// MetadataStageRetire removes the legacy pair once the current one is proven.
	MetadataStageRetire MetadataStage = "retire"
)

// MetadataResourceKind names the three resource families that carry ownership
// labels.
type MetadataResourceKind string

const (
	MetadataKindContainer MetadataResourceKind = "container"
	MetadataKindVolume    MetadataResourceKind = "volume"
	MetadataKindNetwork   MetadataResourceKind = "network"
)

// ownershipLabelPairs is the complete set of label pairs this migration may
// change. Anything outside it is somebody else's label and is copied through
// untouched.
func ownershipLabelPairs() [][2]string {
	return [][2]string{
		{LegacyManagedLabelKey, CurrentManagedLabelKey},
		{LegacyRoleLabelKey, CurrentRoleLabelKey},
		{LegacySpecLabelKey, CurrentSpecLabelKey},
	}
}

// ownershipKeySet is the six keys the sweep is allowed to write.
func ownershipKeySet() map[string]struct{} {
	keys := make(map[string]struct{}, len(ownershipLabelKeys))
	for _, key := range ownershipLabelKeys {
		keys[key] = struct{}{}
	}
	return keys
}

// changedLabelKeys reports every key whose presence or value differs.
func changedLabelKeys(before, after map[string]string) []string {
	changed := make([]string, 0)
	seen := make(map[string]struct{}, len(before)+len(after))
	for key := range before {
		seen[key] = struct{}{}
	}
	for key := range after {
		seen[key] = struct{}{}
	}
	for key := range seen {
		beforeValue, beforePresent := before[key]
		afterValue, afterPresent := after[key]
		if beforePresent != afterPresent || beforeValue != afterValue {
			changed = append(changed, key)
		}
	}
	sort.Strings(changed)
	return changed
}

// planOwnershipLabels returns the labels a resource should carry after one
// stage. It refuses every state it cannot reason about rather than guessing,
// because the states it would have to guess about — a conflict, a half written
// pair, a resource that is not managed at all — are exactly the ones where a
// wrong guess orphans a live resource.
func planOwnershipLabels(
	current map[string]string,
	stage MetadataStage,
) (map[string]string, error) {
	class := ClassifyOwnership(current)
	if class == OwnershipConflicting {
		return nil, conflictingLabelError(
			"resource", "ownership",
			LegacyManagedLabelKey, CurrentManagedLabelKey,
		)
	}
	if !class.Owned() {
		return nil, fmt.Errorf(
			"resource is %s and is not managed by agentopsctl; "+
				"this migration only touches resources it owns",
			class,
		)
	}
	planned := make(map[string]string, len(current))
	for key, value := range current {
		planned[key] = value
	}
	for _, pair := range ownershipLabelPairs() {
		legacyKey, currentKey := pair[0], pair[1]
		value, agreement := ReadDualLabel(current, legacyKey, currentKey)
		switch agreement {
		case LabelConflicting:
			return nil, conflictingLabelError(
				"resource", "ownership pair", legacyKey, currentKey,
			)
		case LabelAbsent:
			continue
		}
		switch stage {
		case MetadataStagePrepare:
			// Only a legacy-only pair is raised, and it is raised by adding the
			// current key — never by writing the legacy one. A pair that is
			// already dual or already current-only has met or passed this
			// stage's goal, and "topping it up" would re-create the legacy
			// labels the phase exists to remove.
			if agreement == LabelLegacyOnly {
				planned[currentKey] = value
			}
		case MetadataStageRetire:
			if agreement == LabelLegacyOnly {
				return nil, fmt.Errorf(
					"%s is present in the legacy namespace only; run the "+
						"prepare stage before retiring it",
					legacyKey,
				)
			}
			delete(planned, legacyKey)
			planned[currentKey] = value
		default:
			return nil, fmt.Errorf("unknown migration stage %q", stage)
		}
	}
	// The stage may only ever have touched the six ownership keys. Asserting it
	// here means a future edit to the loop above cannot quietly widen the blast
	// radius past what the runbook promises.
	owned := ownershipKeySet()
	for _, key := range changedLabelKeys(current, planned) {
		if _, allowed := owned[key]; !allowed {
			return nil, fmt.Errorf(
				"migration would change non-ownership label %q", key,
			)
		}
	}
	return planned, nil
}

// metadataLayout describes where one resource family keeps its documents.
type metadataLayout struct {
	directory string
	// documents maps a file name to the path its labels live at. A document
	// that is absent is fine; one that is present must be migrated.
	documents map[string][]string
	// companions are the non-metadata files a resource directory may contain.
	// Anything outside both sets stops the run: an unrecognised file means this
	// code's model of the on-disk layout is out of date, and acting on a stale
	// model is how a migration misses a copy of the labels.
	companions map[string]struct{}
}

func layoutFor(kind MetadataResourceKind) (metadataLayout, error) {
	switch kind {
	case MetadataKindVolume:
		return metadataLayout{
			directory:  "volumes",
			documents:  map[string][]string{"entity.json": {"labels"}},
			companions: names("volume.img"),
		}, nil
	case MetadataKindNetwork:
		return metadataLayout{
			directory:  "networks",
			documents:  map[string][]string{"entity.json": {"labels"}},
			companions: names("service.plist"),
		}, nil
	case MetadataKindContainer:
		// A container that has never been started carries only its runtime
		// configuration; starting it writes config.json beside it, and the
		// listing reads config.json in preference. Both are migrated whenever
		// both are present, because leaving either behind is a divergence.
		return metadataLayout{
			directory: "containers",
			documents: map[string][]string{
				"config.json": {"labels"},
				"runtime-configuration.json": {
					"containerConfiguration", "labels",
				},
			},
			companions: names(
				"initfs.ext4", "kernel.bin", "kernel.json", "options.json",
				"rootfs.ext4", "rootfs.json", "service.plist", "stdio.log",
				"vminitd.log",
			),
		}, nil
	default:
		return metadataLayout{}, fmt.Errorf("unknown resource kind %q", kind)
	}
}

func names(values ...string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	return set
}

// MetadataTarget is one resolved resource and the documents that carry its
// labels.
type MetadataTarget struct {
	Kind      MetadataResourceKind
	ID        string
	Directory string
	Files     []metadataFileRef
}

// resolveMetadataTarget locates one resource's documents by exact identity. The
// identity is never used to build a glob and never allowed to contain a path
// separator: a selector that can resolve to more than one resource is the thing
// Issue #123 forbids.
func resolveMetadataTarget(
	appRoot string,
	kind MetadataResourceKind,
	identity string,
) (MetadataTarget, error) {
	layout, err := layoutFor(kind)
	if err != nil {
		return MetadataTarget{}, err
	}
	if identity == "" ||
		identity == "." || identity == ".." ||
		strings.ContainsRune(identity, os.PathSeparator) ||
		strings.ContainsRune(identity, '/') ||
		identity != filepath.Clean(identity) {
		return MetadataTarget{}, fmt.Errorf(
			"%q is not an exact %s identity", identity, kind,
		)
	}
	directory := filepath.Join(appRoot, layout.directory, identity)
	if err := requireNoSymlinkInPath(directory, appRoot); err != nil {
		return MetadataTarget{}, err
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return MetadataTarget{}, fmt.Errorf(
			"%s %s is not present on this host: %w", kind, identity, err,
		)
	}
	if !info.IsDir() {
		return MetadataTarget{}, fmt.Errorf(
			"%s is not a directory", directory,
		)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return MetadataTarget{}, fmt.Errorf("read %s: %w", directory, err)
	}
	target := MetadataTarget{Kind: kind, ID: identity, Directory: directory}
	present := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if _, known := layout.companions[name]; known {
			continue
		}
		labelPath, isDocument := layout.documents[name]
		if !isDocument {
			return MetadataTarget{}, fmt.Errorf(
				"%s holds an unrecognised file %q; this migration will not "+
					"run against a layout it does not know",
				directory, name,
			)
		}
		present = append(present, name)
		target.Files = append(target.Files, metadataFileRef{
			Path:      filepath.Join(directory, name),
			LabelPath: labelPath,
		})
	}
	if len(target.Files) == 0 {
		return MetadataTarget{}, fmt.Errorf(
			"%s %s carries no metadata document", kind, identity,
		)
	}
	// A stable order keeps the plan, the evidence, and the backup manifest
	// listing the same documents in the same sequence on every run.
	sort.Slice(target.Files, func(first, second int) bool {
		return target.Files[first].Path < target.Files[second].Path
	})
	return target, nil
}

// MetadataTargetState is one resource's verified documents and the label set
// they agree on.
type MetadataTargetState struct {
	Target MetadataTarget
	Files  []*metadataFileState
	Labels map[string]string
	Class  OwnershipClass
}

// readMetadataTarget verifies every document and requires them to agree. A
// container whose two documents carry different labels is a half-completed
// migration, and continuing from it would migrate one copy and silently leave
// the other.
func readMetadataTarget(target MetadataTarget) (*MetadataTargetState, error) {
	state := &MetadataTargetState{Target: target}
	for _, ref := range target.Files {
		file, err := inspectMetadataFile(ref)
		if err != nil {
			return nil, err
		}
		if state.Labels == nil {
			state.Labels = file.Labels
		} else if !sameLabels(state.Labels, file.Labels) {
			return nil, fmt.Errorf(
				"%s %s: %s disagrees with %s about the ownership labels; "+
					"resolve the divergence before migrating",
				target.Kind, target.ID,
				filepath.Base(ref.Path),
				filepath.Base(target.Files[0].Path),
			)
		}
		state.Files = append(state.Files, file)
	}
	state.Class = ClassifyOwnership(state.Labels)
	return state, nil
}

func sameLabels(first, second map[string]string) bool {
	if len(first) != len(second) {
		return false
	}
	for key, value := range first {
		if other, present := second[key]; !present || other != value {
			return false
		}
	}
	return true
}

// MetadataApplication is the durable record of one resource's migration.
type MetadataApplication struct {
	Kind         MetadataResourceKind       `json:"kind"`
	ID           string                     `json:"id"`
	Stage        MetadataStage              `json:"stage"`
	Directory    string                     `json:"-"`
	BackupRoot   string                     `json:"-"`
	ClassBefore  OwnershipClass             `json:"classBefore"`
	ClassAfter   OwnershipClass             `json:"classAfter"`
	ChangedKeys  []string                   `json:"changedKeys"`
	BeforeLabels map[string]string          `json:"beforeLabels"`
	AfterLabels  map[string]string          `json:"afterLabels"`
	Files        []*metadataFileApplication `json:"files"`
	// Reconciled names documents that did not exist when the migration ran and
	// were brought back in line by a later rollback.
	Reconciled []string `json:"reconciled,omitempty"`
}

// applyMetadataStage migrates every document of one resource, or none of them.
// The unwind on failure matters more than the happy path: a resource whose two
// documents disagree is worse than one that was never touched.
func applyMetadataStage(
	target MetadataTarget,
	stage MetadataStage,
	backupDirectory string,
	appRoot string,
) (*MetadataApplication, error) {
	state, err := readMetadataTarget(target)
	if err != nil {
		return nil, err
	}
	planned, err := planOwnershipLabels(state.Labels, stage)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", target.Kind, target.ID, err)
	}
	application := &MetadataApplication{
		Kind:         target.Kind,
		ID:           target.ID,
		Stage:        stage,
		Directory:    target.Directory,
		BackupRoot:   backupDirectory,
		ClassBefore:  state.Class,
		ClassAfter:   ClassifyOwnership(planned),
		ChangedKeys:  changedLabelKeys(state.Labels, planned),
		BeforeLabels: state.Labels,
		AfterLabels:  planned,
	}
	for _, file := range state.Files {
		backupPath := filepath.Join(
			backupDirectory,
			string(target.Kind), target.ID, filepath.Base(file.Ref.Path),
		)
		applied, err := applyMetadataFile(file, planned, backupPath)
		if applied != nil {
			applied.Document = relativeTo(appRoot, applied.Path)
			applied.Backup = relativeTo(backupDirectory, applied.BackupPath)
		}
		if err != nil {
			// Unwind in reverse so the resource is left exactly as it was
			// found. A rollback failure here is reported alongside the original
			// error rather than replacing it: the operator needs both.
			if unwindErr := unwind(application.Files); unwindErr != nil {
				return nil, fmt.Errorf(
					"%s %s: %w (and the partial migration could not be "+
						"unwound: %v)",
					target.Kind, target.ID, err, unwindErr,
				)
			}
			return nil, fmt.Errorf("%s %s: %w", target.Kind, target.ID, err)
		}
		application.Files = append(application.Files, applied)
	}
	return application, nil
}

func unwind(applied []*metadataFileApplication) error {
	for index := len(applied) - 1; index >= 0; index-- {
		outcome, err := restoreMetadataFile(applied[index])
		if err != nil {
			return err
		}
		applied[index].RestoredAs = outcome
	}
	return nil
}

// rollbackMetadataApplication returns one resource's ownership labels to what
// they were before the migration touched it, including in documents that did
// not exist when the migration ran.
//
// That last part is not hypothetical. Starting a container materialises
// config.json from the runtime's in-memory model, and the listing reads
// config.json in preference to runtime-configuration.json. A rollback that
// restored only the documents it had recorded would leave such a container
// reporting its migrated labels while its other document said otherwise — a
// half-reverted resource, which is worse than either end state.
func rollbackMetadataApplication(application *MetadataApplication) error {
	if err := unwind(application.Files); err != nil {
		return err
	}
	return reconcileDocumentsCreatedSince(application)
}

// reconcileDocumentsCreatedSince writes the recorded pre-migration labels into
// any known document that appeared after the migration ran. It refuses unless
// that document's labels are exactly what the migration left behind, which is
// the only way to tell "the runtime derived this from the migrated state" from
// "somebody else wrote this".
func reconcileDocumentsCreatedSince(application *MetadataApplication) error {
	if application.Directory == "" {
		return nil
	}
	layout, err := layoutFor(application.Kind)
	if err != nil {
		return err
	}
	recorded := make(map[string]struct{}, len(application.Files))
	for _, file := range application.Files {
		recorded[filepath.Base(file.Path)] = struct{}{}
	}
	names := make([]string, 0, len(layout.documents))
	for name := range layout.documents {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, known := recorded[name]; known {
			continue
		}
		path := filepath.Join(application.Directory, name)
		if _, err := os.Lstat(path); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("inspect %s: %w", path, err)
		}
		ref := metadataFileRef{Path: path, LabelPath: layout.documents[name]}
		state, err := inspectMetadataFile(ref)
		if err != nil {
			return err
		}
		if !sameLabels(state.Labels, application.AfterLabels) {
			return fmt.Errorf(
				"%s %s: %s appeared after the migration and does not carry the "+
					"labels the migration wrote; resolve it by hand",
				application.Kind, application.ID, name,
			)
		}
		backupPath := filepath.Join(
			application.BackupRoot, string(application.Kind), application.ID,
			name+".created-since",
		)
		if _, err := applyMetadataFile(
			state, application.BeforeLabels, backupPath,
		); err != nil {
			return err
		}
		application.Reconciled = append(application.Reconciled, name)
	}
	return nil
}
