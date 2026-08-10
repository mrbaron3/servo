package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
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

// ContainerTargets lists the container identities a plan will rewrite. The
// rollback gate needs these by name: a resource an interrupted rollback already
// reverted classifies as missing-label, so a gate keyed on classification alone
// stops seeing exactly the containers a resumed run is about to touch.
func (plan *RollbackPlan) ContainerTargets() []string {
	targets := make([]string, 0, len(plan.Applied))
	for _, application := range plan.Applied {
		if application.Kind == MetadataKindContainer {
			targets = append(targets, application.ID)
		}
	}
	return targets
}

// RunningNamed lists identities from `names` that the inventory reports as a
// container in any state other than stopped, whatever this binary makes of its
// labels.
func (inventory *OwnershipInventory) RunningNamed(names []string) []string {
	wanted := make(map[string]struct{}, len(names))
	for _, name := range names {
		wanted[name] = struct{}{}
	}
	running := make([]string, 0)
	for _, record := range inventory.Records {
		if record.Kind != MetadataKindContainer {
			continue
		}
		if _, want := wanted[record.ID]; !want {
			continue
		}
		if record.State != "stopped" {
			running = append(running, record.ID)
		}
	}
	return running
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
// Missing-label and unmanaged are still excluded here, and that is not an
// oversight: stopping Apple Container affects the whole host, but blocking on
// `buildkit` or on another deployment's container would make rollback impossible
// on any real machine. The containers a plan actually targets are covered
// separately and by name, through RunningNamed — which is what catches a resumed
// rollback whose earlier resources are already back on the retired namespace and
// therefore unclassifiable.
//
// Both are courtesy checks. RequireServicesStopped, which demands two
// independent signals that the runtime is down, is what actually proves no
// document is rewritten under a live apiserver.
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
// This is the plan's trust boundary and it has to be drawn here. ParseRollbackPlan
// checks only that the plan is internally consistent — counts that line up, label
// maps that exist — and every path, digest, and label map it carries comes out of
// a file. A plan that is merely self-consistent can name a legitimate document,
// supply a backup it chose along with matching self-authored digests, and have
// rollback rewrite a real resource's ownership metadata. Internal consistency is
// exactly what a forged plan has.
//
// So the binding is a reconstruction and a verification, not a comparison:
//
//   - Every location is rebuilt from the resolved application root plus the
//     plan's own kind and identity, and the plan must agree with what was built.
//     A plan cannot name a place the layout does not put a document.
//   - Every backup sits beneath ONE canonical private root, at the same
//     kind/id/document shape, and that root must be a 0700 directory this user
//     owns outside any git work tree. A plan cannot scatter verbatim copies of a
//     container's environment across the filesystem.
//   - Every backup's bytes are hashed and must match the digests the plan
//     recorded AND decode to the BeforeLabels it claims. A plan cannot assert a
//     restoration target its own backup does not contain.
//   - The recorded before-to-after transform is re-derived: rewriting the backup
//     with the plan's AfterLabels must reproduce the AfterSHA256 it recorded. A
//     plan cannot claim a migration that never happened.
//   - Duplicate resources and duplicate documents are refused, so the number of
//     things a run touches cannot be ambiguous.
//
// All of it runs before StopSystem. A rollback that refuses must not first take
// the operator's container runtime down.
func (plan *RollbackPlan) BindToHost(host *MetadataHost) error {
	if host == nil || strings.TrimSpace(host.AppRoot) == "" {
		return fmt.Errorf("rollback requires a resolved Apple Container host")
	}
	backupRoot, err := plan.canonicalBackupRoot()
	if err != nil {
		return err
	}
	if err := requirePrivateBackupRoot(backupRoot); err != nil {
		return err
	}
	seenResources := make(map[string]struct{}, len(plan.Applied))
	for _, application := range plan.Applied {
		layout, err := layoutFor(application.Kind)
		if err != nil {
			return err
		}
		identity := application.ID
		if err := requireExactIdentity(identity, application.Kind); err != nil {
			return err
		}
		resource := string(application.Kind) + "/" + identity
		if _, duplicate := seenResources[resource]; duplicate {
			return fmt.Errorf("rollback plan names %s more than once", resource)
		}
		seenResources[resource] = struct{}{}

		directory := filepath.Join(host.AppRoot, layout.directory, identity)
		if err := requireNoSymlinkInPath(directory, host.AppRoot); err != nil {
			return err
		}
		// Run the layout-recognition refusal here, before StopSystem, as well as
		// inside the restore. Only the pre-stop run has to be able to say no; the
		// post-stop one governs the write. Without this, an unrecognised file in
		// a resource directory — a `.DS_Store` is enough — refuses the rollback
		// only after the operator's entire container runtime has been taken down
		// and brought back, which is exactly what this function's contract says
		// must not happen.
		if err := requireRecognisedLayout(
			application.Kind, identity, directory,
		); err != nil {
			return err
		}
		seenDocuments := make(map[string]struct{}, len(application.Files))
		for _, file := range application.Files {
			name := filepath.Base(file.Path)
			labelPath, known := layout.documents[name]
			if !known {
				return fmt.Errorf(
					"%s: %q is not a document this migration knows about",
					resource, name,
				)
			}
			if _, duplicate := seenDocuments[name]; duplicate {
				return fmt.Errorf("%s names %s more than once", resource, name)
			}
			seenDocuments[name] = struct{}{}
			if file.Path != filepath.Join(directory, name) {
				return fmt.Errorf(
					"%s: the plan names %s, which is not where this host keeps "+
						"that document", resource, name,
				)
			}
			// The label path decides which field is overwritten. Taking it from
			// the layout rather than trusting the plan is what stops a forged
			// plan from rewriting a non-label field.
			if !equalStringSlices(file.LabelPath, labelPath) {
				return fmt.Errorf(
					"%s: the plan puts %s's labels somewhere this host does not",
					resource, name,
				)
			}
			if err := requireNoSymlinkInPath(file.Path, host.AppRoot); err != nil {
				return err
			}
			if file.BackupPath != filepath.Join(
				backupRoot, string(application.Kind), identity, name,
			) {
				return fmt.Errorf(
					"%s: %s's backup is not where this plan's backup root keeps it",
					resource, name,
				)
			}
			// The document path is walked component by component; the backup path
			// has to be too. Checking only the root leaves <root>/<kind> or
			// <root>/<kind>/<id> swappable for a symlink, and rollback both READS
			// a backup through that path and WRITES a new one down it.
			if err := requireNoSymlinkInPath(
				file.BackupPath, backupRoot,
			); err != nil {
				return err
			}
			// Document and Backup are the only plan-controlled strings that reach
			// operator output: RestoreOutcomes prints them and every preflight
			// error embeds them. Everything else printed from a plan is either
			// reconstructed here or, like Stage and ClassBefore/ClassAfter,
			// deliberately never printed at all. Left unchecked they are a free
			// text channel out of a forged plan into the terminal of the one
			// command still permitted to rewrite runtime metadata — enough for
			// control characters that corrupt the aligned outcome table, or for a
			// fabricated "restored N resource(s)" line. Both are reconstructible
			// from values this binding already derived, so checking them costs two
			// comparisons.
			if file.Document != filepath.Join(layout.directory, identity, name) {
				return fmt.Errorf(
					"%s: the plan's recorded document location for %s does not "+
						"match where this host keeps it", resource, name,
				)
			}
			if file.Backup != filepath.Join(
				string(application.Kind), identity, name,
			) {
				return fmt.Errorf(
					"%s: the plan's recorded backup location for %s does not "+
						"match where its backup root keeps it", resource, name,
				)
			}
			if err := verifyRecordedBackup(resource, name, file); err != nil {
				return err
			}
			// The live document is verified here as well as inside the restore.
			// Both are needed and neither is redundant: this one runs before the
			// runtime is stopped, so a plan that cannot apply is refused without
			// taking Apple Container down; the one inside the restore runs again
			// afterwards, because the runtime re-serialises entity.json across a
			// stop and the document it will actually rewrite is the later one.
			if err := preflightRestore(file); err != nil {
				return fmt.Errorf("%s: %w", resource, err)
			}
		}
	}
	return nil
}

// canonicalBackupRoot derives the single root every backup in the plan must sit
// beneath, and refuses a plan whose backups are spread across more than one.
func (plan *RollbackPlan) canonicalBackupRoot() (string, error) {
	root := ""
	for _, application := range plan.Applied {
		for _, file := range application.Files {
			if strings.TrimSpace(file.BackupPath) == "" {
				return "", fmt.Errorf(
					"%s %s: the plan records a document with no backup location",
					application.Kind, application.ID,
				)
			}
			// <root>/<kind>/<id>/<document>
			candidate := filepath.Dir(filepath.Dir(filepath.Dir(file.BackupPath)))
			if root == "" {
				root = candidate
				continue
			}
			if candidate != root {
				return "", fmt.Errorf(
					"rollback plan spreads its backups across more than one root",
				)
			}
		}
	}
	if root == "" {
		return "", fmt.Errorf("rollback plan records no backup location")
	}
	return root, nil
}

// requireExactIdentity refuses an identity that could index anything other than
// one directory.
func requireExactIdentity(identity string, kind MetadataResourceKind) error {
	if identity == "" || identity == "." || identity == ".." ||
		strings.ContainsRune(identity, os.PathSeparator) ||
		strings.ContainsRune(identity, '/') ||
		identity != filepath.Clean(identity) {
		return fmt.Errorf("%q is not an exact %s identity", identity, kind)
	}
	return nil
}

// requirePrivateBackupRoot refuses a backup root that is not a private directory
// this user owns.
//
// A backup is a verbatim copy of an Apple Container metadata document, and a
// container's config.json carries initProcess.environment with values —
// POSTGRES_PASSWORD among them on this project's own topology. Rollback writes
// new backups into this root for documents that appeared after the migration, so
// this is a check on where THIS run is about to copy a credential, not on
// somebody else's past behaviour.
func requirePrivateBackupRoot(root string) error {
	resolved, err := resolveExistingAncestor(root)
	if err != nil {
		return err
	}
	if err := requireOutsideGitWorkTree(resolved); err != nil {
		return err
	}
	info, err := os.Lstat(root)
	if err != nil {
		return fmt.Errorf("inspect backup root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("the backup root is a symbolic link")
	}
	if !info.IsDir() {
		return fmt.Errorf("the backup root is not a directory")
	}
	if info.Mode().Perm() != 0o700 {
		return fmt.Errorf(
			"the backup root is mode %v; a directory holding copies of "+
				"container configuration must be 0700", info.Mode().Perm(),
		)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("the backup root has no owner information")
	}
	if int(stat.Uid) != os.Getuid() {
		return fmt.Errorf(
			"the backup root is owned by uid %d, not the current user %d",
			stat.Uid, os.Getuid(),
		)
	}
	return nil
}

// verifyRecordedBackup proves one document's backup actually contains what the
// plan says it contains, and that the plan's account of the migration is
// reproducible from it.
//
// Without this the digests are self-referential: a plan supplies both the backup
// and the hashes it should match, so they always agree. What makes them mean
// something is deriving the migrated form from the backup and requiring it to
// equal the digest the plan recorded for the live document.
func verifyRecordedBackup(
	resource, name string,
	file *metadataFileApplication,
) error {
	info, _, _, err := statMetadataFile(file.BackupPath)
	if err != nil {
		return fmt.Errorf("%s: %s's backup: %w", resource, name, err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf(
			"%s: %s's backup is readable by other accounts (%v)",
			resource, name, info.Mode().Perm(),
		)
	}
	backup, err := os.ReadFile(file.BackupPath)
	if err != nil {
		return fmt.Errorf("%s: read %s's backup: %w", resource, name, err)
	}
	digest := digestOf(backup)
	if digest != file.BackupSHA256 || digest != file.BeforeSHA256 {
		return fmt.Errorf(
			"%s: %s's backup does not match the digests the plan recorded",
			resource, name,
		)
	}
	recorded, err := readLabelsAtPath(backup, file.LabelPath)
	if err != nil {
		return fmt.Errorf("%s: %s's backup: %w", resource, name, err)
	}
	if !sameLabels(recorded, file.BeforeLabels) {
		return fmt.Errorf(
			"%s: %s's backup does not carry the labels the plan restores",
			resource, name,
		)
	}
	migrated, err := rewriteLabels(backup, file.LabelPath, file.AfterLabels)
	if err != nil {
		return fmt.Errorf("%s: %s: %w", resource, name, err)
	}
	if digestOf(migrated) != file.AfterSHA256 {
		return fmt.Errorf(
			"%s: %s's recorded migration cannot be reproduced from its backup",
			resource, name,
		)
	}
	return nil
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
