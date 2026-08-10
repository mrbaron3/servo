package lifecycle

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Phase 3A moved managed resources onto the current ownership namespace by
// rewriting Apple Container's own metadata documents in two stages: `prepare`
// added the current label pair beside the legacy one, and `retire` removed the
// legacy pair once the current one was proven. Phase 3B removes both stages.
//
// They are removed rather than left behind a flag. Every line of the forward
// path existed to read, compare, or write the legacy namespace, and Phase 3B's
// whole claim is that this binary does none of those things. A forward stage
// that could still run would also be able to write a legacy key back onto a
// resource that no reader can see — the one state the migration cannot recover
// from on its own.
//
// What remains is recovery. A Phase 3A run recorded, in a private 0700 backup
// root, the exact bytes of every document it rewrote and the complete label maps
// on either side of the change. This file can still return those documents to
// those bytes. It does so without interpreting a single label key: the recorded
// maps are restored verbatim, which is what makes the retained path safe even
// though it restores labels this binary can no longer read.
//
// That asymmetry is the point. Phase 3B can undo Phase 3A but cannot redo it,
// and a host returned to its pre-Phase-3A labels must then be operated by a
// pre-Phase-3B binary. The runbook states that boundary; it is a deliberate
// operational choice, not a fallback this code makes for anyone.
//
// Nothing here deletes or recreates a resource. Apple Container attaches a named
// volume exclusively to a single virtual machine, and recreating a volume to
// change its labels is the same act as destroying its data.

// MetadataResourceKind names the three resource families that carry ownership
// labels.
type MetadataResourceKind string

const (
	MetadataKindContainer MetadataResourceKind = "container"
	MetadataKindVolume    MetadataResourceKind = "volume"
	MetadataKindNetwork   MetadataResourceKind = "network"
)

// metadataLayout describes where one resource family keeps its documents.
type metadataLayout struct {
	directory string
	// documents maps a file name to the path its labels live at. A document
	// that is absent is fine; one that is present must be restored.
	documents map[string][]string
	// companions are the non-metadata files a resource directory may contain.
	// Anything outside both sets stops the run: an unrecognised file means this
	// code's model of the on-disk layout is out of date, and acting on a stale
	// model is how a recovery misses a copy of the labels.
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
		// listing reads config.json in preference. Both are handled whenever both
		// are present, because leaving either behind is a divergence.
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

// backupAttemptSuffix distinguishes one rollback attempt's backup of a
// created-since document from an earlier attempt's.
func backupAttemptSuffix() (string, error) {
	raw := make([]byte, 4)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate backup attempt identity: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

func names(values ...string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	return set
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

// MetadataApplication is the durable record of one resource's Phase 3A
// migration, as recovery reads it back out of a retained rollback plan.
//
// Since Phase 3B nothing produces one of these; they are only ever decoded. The
// fields that describe what Phase 3A did are therefore treated as an opaque
// restoration record: they are carried verbatim, reported verbatim, and never
// re-interpreted against the current ownership contract. That matters most for
// the two class fields and the stage name, which spell values — `dual`,
// `legacy-only`, `prepare`, `retire` — that no longer exist in this binary's
// vocabulary. Re-typing them would either lose the record or invent a meaning
// for it.
type MetadataApplication struct {
	Kind MetadataResourceKind `json:"kind"`
	ID   string               `json:"id"`
	// Stage is the Phase 3A stage name recorded by the binary that wrote this
	// plan. Nothing reads it: it is carried so the record stays complete and so a
	// human reading the plan file can see which stage it undoes. It is never
	// matched against, and it is never printed — it is plan-controlled text.
	Stage      string `json:"stage"`
	Directory  string `json:"-"`
	BackupRoot string `json:"-"`
	// ClassBefore and ClassAfter are the ownership classes the Phase 3A binary
	// recorded. They are strings rather than OwnershipClass precisely because
	// they may hold classes this binary no longer defines. Like Stage they are
	// carried, not read, and not printed.
	ClassBefore string   `json:"classBefore"`
	ClassAfter  string   `json:"classAfter"`
	ChangedKeys []string `json:"changedKeys"`
	// The complete label maps are what rollback restores, so they must include
	// foreign labels; they are therefore private to the rollback plan and never
	// reach the committed evidence.
	BeforeLabels map[string]string `json:"-"`
	AfterLabels  map[string]string `json:"-"`
	// Only the ownership keys are published.
	BeforeOwnership map[string]string          `json:"beforeOwnershipLabels"`
	AfterOwnership  map[string]string          `json:"afterOwnershipLabels"`
	Files           []*metadataFileApplication `json:"files"`
	// Reconciled names documents that did not exist when the migration ran and
	// were brought back in line by a later rollback.
	Reconciled []string `json:"reconciled,omitempty"`
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
	// Every document is checked before any document is written. A rollback that
	// restored one document and then discovered the second was unacceptable
	// would produce exactly the half-reverted resource this function exists to
	// prevent — and on a container, where the listing prefers config.json, that
	// half state is indistinguishable from a successful migration.
	created, err := planDocumentsCreatedSince(application)
	if err != nil {
		return err
	}
	for _, file := range application.Files {
		if err := preflightRestore(file); err != nil {
			return err
		}
	}
	restored := make([]*metadataFileApplication, 0, len(application.Files))
	for index := len(application.Files) - 1; index >= 0; index-- {
		file := application.Files[index]
		outcome, err := restoreMetadataFile(file)
		if outcome != "" {
			// Recorded before the error is handled: restoreMetadataFile returns
			// an outcome alongside a durability-uncertain error precisely because
			// the document was changed, and a document that changed has to be in
			// the unwind set.
			file.RestoredAs = outcome
			// RestoreAlreadyBefore is the one outcome that reports NO write: the
			// document was found at its pre-migration labels and left alone. It
			// must stay out of the unwind set, because the unwind writes
			// AfterLabels — it would push a document this run merely observed
			// FORWARD to the migrated labels.
			//
			// That case is the supported recovery, not a corner: an interrupted
			// rollback leaves some documents reverted, the runbook says to re-run
			// the same plan, and on the re-run those documents return
			// already-before. Unwinding them would undo the first run's completed
			// work, so resuming would lose ground exactly where it promises to be
			// idempotent.
			if outcome != RestoreAlreadyBefore {
				restored = append(restored, file)
			}
		}
		if err != nil {
			// Put back what this rollback already undid, so a failure leaves the
			// resource in the state it was found in rather than between two.
			return errors.Join(err, undoPartialRollback(application, restored))
		}
	}
	for _, pending := range created {
		applied, err := applyMetadataFile(
			pending.state, application.BeforeLabels, pending.backupPath,
		)
		if applied != nil {
			// This record's label maps run the opposite way to every other record
			// in `restored`, and the difference is easy to miss because both are
			// called Before/After. applyMetadataFile records them from ITS point
			// of view — "what I found" and "what I wrote" — so its AfterLabels is
			// the pre-migration map, the rollback destination. Every plan record's
			// AfterLabels is the migrated map. `reapply` writes AfterLabels, so
			// handing it this record unswapped would rewrite the rollback
			// destination that is already on disk while the recorded documents
			// were pushed forward — the half-reverted container this function
			// exists to prevent, on the document the listing prefers.
			applied.BeforeLabels, applied.AfterLabels =
				applied.AfterLabels, applied.BeforeLabels
			restored = append(restored, applied)
			application.Reconciled = append(application.Reconciled, pending.name)
		}
		if err != nil {
			return errors.Join(err, undoPartialRollback(application, restored))
		}
	}
	return nil
}

// undoPartialRollback puts back everything this rollback had already written
// and then makes the resource's own record agree with the result.
//
// The Reconciled names are cleared only when the undo fully succeeded. A
// created-since document that was pushed back to its migrated labels was not
// reconciled by this run, and leaving the name behind would report a document
// as handled that is sitting in the opposite state. When the undo is partial,
// the names stay: something may genuinely still be reverted, and over-reporting
// sends the operator to look, which is the safe direction.
func undoPartialRollback(
	application *MetadataApplication,
	restored []*metadataFileApplication,
) error {
	if err := reapply(restored); err != nil {
		return err
	}
	application.Reconciled = nil
	return nil
}

// reapply returns documents a failed rollback had already restored to their
// post-migration content, so the resource ends up wholly migrated rather than
// partly reverted. Each document it puts back has its recorded outcome cleared,
// because that outcome describes a restoration that no longer stands.
func reapply(restored []*metadataFileApplication) error {
	var failures []error
	for _, file := range restored {
		state, err := inspectMetadataFile(file.Ref())
		if err != nil {
			failures = append(failures, err)
			continue
		}
		rewritten, err := rewriteLabels(
			state.Bytes, file.LabelPath, file.AfterLabels,
		)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if err := writeFileAtomically(
			file.Path, rewritten, state.Mode.Perm(), state.UID, state.GID,
		); err != nil {
			failures = append(failures, err)
			continue
		}
		// The document is back to its post-migration content, so it did NOT land
		// as a restoration. Clearing the outcome here is what keeps the failure
		// report describing durable on-disk state: the outcome was recorded the
		// moment the document changed, deliberately, because a changed document
		// has to be in the unwind set — but once the unwind puts it back, an
		// outcome left behind tells the operator a document is in the reverted
		// half when it is in the migrated one. A file whose reapply failed above
		// keeps its outcome, which is correct: that one really is still reverted.
		file.RestoredAs = ""
	}
	if len(failures) > 0 {
		return fmt.Errorf(
			"the partial rollback could not be undone either: %w",
			errors.Join(failures...),
		)
	}
	return nil
}

// preflightRestore proves one document can be restored before any document is.
func preflightRestore(file *metadataFileApplication) error {
	backup, err := os.ReadFile(file.BackupPath)
	if err != nil {
		return fmt.Errorf("read backup for %s: %w", file.Document, err)
	}
	if digestOf(backup) != file.BackupSHA256 ||
		digestOf(backup) != file.BeforeSHA256 {
		return fmt.Errorf(
			"backup for %s no longer matches the digest recorded when it was "+
				"taken", file.Document,
		)
	}
	current, err := os.ReadFile(file.Path)
	if err != nil {
		return fmt.Errorf("read %s: %w", file.Document, err)
	}
	if _, _, _, err := statMetadataFile(file.Path); err != nil {
		return err
	}
	switch digestOf(current) {
	case file.BeforeSHA256, file.AfterSHA256:
		return nil
	}
	// Not either recorded shape: only an Apple Container re-serialisation is
	// acceptable, and proving that is the same work restore does — including
	// the byte-stability guard. equalExceptLabels below is deliberately
	// insensitive to key order and whitespace, so without this a non-compact
	// document would pass preflight and then be refused by the restore, after
	// earlier documents had already been written.
	if err := requireByteStableRoundTrip(current, file.LabelPath); err != nil {
		return fmt.Errorf("%s: %w", file.Document, err)
	}
	rewritten, err := rewriteLabels(backup, file.LabelPath, file.AfterLabels)
	if err != nil {
		return fmt.Errorf("%s: %w", file.Document, err)
	}
	same, err := equalExceptLabels(current, rewritten, file.LabelPath)
	if err != nil {
		return fmt.Errorf("%s: %w", file.Document, err)
	}
	currentLabels, err := readLabelsAtPath(current, file.LabelPath)
	if err != nil {
		return fmt.Errorf("%s: %w", file.Document, err)
	}
	if !same || !sameLabels(currentLabels, file.AfterLabels) {
		return fmt.Errorf("%s: %w", file.Document, ErrRollbackUnknownState)
	}
	return nil
}

// pendingCreatedDocument is one document that appeared after the migration and
// has been proven safe to bring back in line.
type pendingCreatedDocument struct {
	name       string
	state      *metadataFileState
	backupPath string
}

// planDocumentsCreatedSince verifies, without writing anything, every known
// document that did not exist when the migration ran.
// requireRecognisedLayout refuses a resource directory holding a file this
// migration does not know. An unrecognised file means this code's model of the
// on-disk layout is out of date, and acting on a stale model is how a recovery
// misses a copy of the labels. Phase 3A enforced this while resolving an
// operator-named target; that resolution went with the forward stages.
//
// It is deliberately separate from the created-since scan that follows it so
// BindToHost can run it BEFORE StopSystem. The scan itself cannot move: it
// compares document bytes and labels, and the runtime re-serialises entity.json
// across a stop, so only the post-stop answer governs the write. Recognising
// the directory's contents needs none of that — it is a listing and a name
// comparison — and a `.DS_Store` left by Finder in `~/Library/Application
// Support/...` is enough to refuse a rollback. Discovering that only after the
// stop costs the operator their whole container runtime for a command that was
// always going to say no.
func requireRecognisedLayout(
	kind MetadataResourceKind,
	id, directory string,
) error {
	if directory == "" {
		return nil
	}
	layout, err := layoutFor(kind)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return fmt.Errorf("read %s: %w", directory, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if _, known := layout.companions[name]; known {
			continue
		}
		if _, known := layout.documents[name]; known {
			continue
		}
		return fmt.Errorf(
			"%s %s: %q is a file this migration does not recognise; recovery "+
				"will not run against a layout it does not know",
			kind, id, name,
		)
	}
	return nil
}

func planDocumentsCreatedSince(
	application *MetadataApplication,
) ([]pendingCreatedDocument, error) {
	if application.Directory == "" {
		return nil, nil
	}
	layout, err := layoutFor(application.Kind)
	if err != nil {
		return nil, err
	}
	recorded := make(map[string]struct{}, len(application.Files))
	for _, file := range application.Files {
		recorded[filepath.Base(file.Path)] = struct{}{}
	}
	if err := requireRecognisedLayout(
		application.Kind, application.ID, application.Directory,
	); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(layout.documents))
	for name := range layout.documents {
		names = append(names, name)
	}
	sort.Strings(names)
	pending := make([]pendingCreatedDocument, 0)
	for _, name := range names {
		if _, known := recorded[name]; known {
			continue
		}
		path := filepath.Join(application.Directory, name)
		if _, err := os.Lstat(path); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("inspect %s: %w", path, err)
		}
		state, err := inspectMetadataFile(metadataFileRef{
			Path: path, LabelPath: layout.documents[name],
		})
		if err != nil {
			return nil, err
		}
		// A rollback that already brought this document back is a no-op, not an
		// error. Rollback is the recovery path: it has to survive being run
		// twice, and being resumed after an interruption partway through.
		if sameLabels(state.Labels, application.BeforeLabels) {
			continue
		}
		if !sameLabels(state.Labels, application.AfterLabels) {
			return nil, fmt.Errorf(
				"%s %s: %s appeared after the migration and carries neither the "+
					"labels the migration wrote nor the ones it replaced; "+
					"resolve it by hand",
				application.Kind, application.ID, name,
			)
		}
		backupPath := filepath.Join(
			application.BackupRoot, string(application.Kind), application.ID,
			name+".created-since",
		)
		// The backup name is made unique per attempt rather than reused: O_EXCL
		// is what keeps one run from overwriting another run's record of an
		// irreversible act, and a fixed name would turn a second rollback
		// attempt into a failure for the wrong reason.
		if _, err := os.Lstat(backupPath); err == nil {
			suffix, suffixErr := backupAttemptSuffix()
			if suffixErr != nil {
				return nil, suffixErr
			}
			backupPath += "." + suffix
		}
		pending = append(pending, pendingCreatedDocument{
			name: name, state: state, backupPath: backupPath,
		})
	}
	return pending, nil
}
