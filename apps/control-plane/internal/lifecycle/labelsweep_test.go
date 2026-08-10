package lifecycle

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// These cases cover what Phase 3B keeps of the Phase 3A metadata migration: the
// path that returns a document to the exact bytes a Phase 3A run recorded for
// it.
//
// Every fixture here is reconstructed rather than produced. The forward stages
// are gone, so no test can migrate a document and then roll it back; instead each
// one writes the documents a Phase 3A run would have left on disk plus the
// backups it would have taken, which is precisely what an operator's retained
// backup root contains. Building the fixture this way is not a workaround — it is
// the only input the recovery path will ever see again.

// seedAppRoot writes an application root holding one volume, one network, and
// two containers, all labelled the way Phase 3A's sweep left them: current
// namespace only.
func seedAppRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(
		filepath.Join(root, "volumes", "vol-a", "entity.json"),
		`{"name":"vol-a","labels":{"`+CurrentManagedLabelKey+`":"v1"}}`,
	)
	write(filepath.Join(root, "volumes", "vol-a", "volume.img"), "")
	write(
		filepath.Join(root, "networks", "net-a", "entity.json"),
		`{"name":"net-a","labels":{"`+CurrentManagedLabelKey+`":"v1"}}`,
	)
	write(filepath.Join(root, "networks", "net-a", "service.plist"), "")
	// A container that was started carries both documents; one that was only
	// created carries just the runtime configuration.
	write(
		filepath.Join(root, "containers", "ctr-started", "config.json"),
		`{"id":"ctr-started","labels":{"`+CurrentManagedLabelKey+`":"v1"}}`,
	)
	write(
		filepath.Join(
			root, "containers", "ctr-started", "runtime-configuration.json",
		),
		`{"containerConfiguration":{"id":"ctr-started","labels":{"`+
			CurrentManagedLabelKey+`":"v1"}}}`,
	)
	write(
		filepath.Join(
			root, "containers", "ctr-created", "runtime-configuration.json",
		),
		`{"containerConfiguration":{"id":"ctr-created","labels":{"`+
			CurrentManagedLabelKey+`":"v1"}}}`,
	)
	return root
}

// legacyTriple is the label map a resource carried before Phase 3A migrated it,
// including a foreign label so every test asserts that recovery restores a
// complete map rather than the ownership keys it happens to recognise.
func legacyTriple() map[string]string {
	return map[string]string{
		legacyManagedLabelKey: "v1",
		"com.example.foreign": "keep-me",
	}
}

// phase3ARecord reconstructs the record a Phase 3A run left behind for one
// resource: backups holding the labels it replaced, and the digests and complete
// label maps that let recovery prove what it is putting back.
func phase3ARecord(
	t *testing.T,
	kind MetadataResourceKind,
	id, appRoot, backupRoot string,
	before map[string]string,
) *MetadataApplication {
	t.Helper()
	layout, err := layoutFor(kind)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(appRoot, layout.directory, id)
	names := make([]string, 0, len(layout.documents))
	for name := range layout.documents {
		if _, err := os.Lstat(filepath.Join(directory, name)); err == nil {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		t.Fatalf("%s %s has no metadata document to seed a record from", kind, id)
	}
	application := &MetadataApplication{
		Kind:         kind,
		ID:           id,
		Stage:        "retire",
		Directory:    directory,
		BackupRoot:   backupRoot,
		ClassBefore:  "legacy-only",
		ClassAfter:   "current-only",
		BeforeLabels: before,
	}
	for _, name := range names {
		ref := metadataFileRef{
			Path: filepath.Join(directory, name), LabelPath: layout.documents[name],
		}
		state, err := inspectMetadataFile(ref)
		if err != nil {
			t.Fatal(err)
		}
		backupBytes, err := rewriteLabels(state.Bytes, ref.LabelPath, before)
		if err != nil {
			t.Fatal(err)
		}
		backupPath := filepath.Join(backupRoot, string(kind), id, name)
		if err := writeBackup(backupPath, backupBytes); err != nil {
			t.Fatal(err)
		}
		application.AfterLabels = state.Labels
		application.Files = append(application.Files, &metadataFileApplication{
			Path:            ref.Path,
			LabelPath:       ref.LabelPath,
			BackupPath:      backupPath,
			Document:        filepath.Join(layout.directory, id, name),
			Backup:          filepath.Join(string(kind), id, name),
			BeforeSHA256:    digestOf(backupBytes),
			AfterSHA256:     state.SHA256,
			BackupSHA256:    digestOf(backupBytes),
			Mode:            state.Mode.Perm(),
			BeforeLabels:    before,
			AfterLabels:     state.Labels,
			BeforeOwnership: ownershipLabelSubset(before),
			AfterOwnership:  ownershipLabelSubset(state.Labels),
		})
	}
	return application
}

func labelsOnDisk(t *testing.T, path string, labelPath []string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	labels, err := readLabelsAtPath(raw, labelPath)
	if err != nil {
		t.Fatal(err)
	}
	return labels
}

func TestLayoutForKnowsEveryResourceFamilyAndNothingElse(t *testing.T) {
	for _, kind := range []MetadataResourceKind{
		MetadataKindVolume, MetadataKindNetwork, MetadataKindContainer,
	} {
		layout, err := layoutFor(kind)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if layout.directory == "" || len(layout.documents) == 0 {
			t.Fatalf("%s has an empty layout: %#v", kind, layout)
		}
	}
	if _, err := layoutFor(MetadataResourceKind("image")); err == nil {
		t.Fatal("an unknown resource kind was accepted")
	}
}

func TestRollbackRestoresEveryDocumentToItsExactBytes(t *testing.T) {
	for _, testCase := range []struct {
		kind MetadataResourceKind
		id   string
	}{
		{MetadataKindVolume, "vol-a"},
		{MetadataKindNetwork, "net-a"},
		// Two documents, which is where a partial restore would be worst: the
		// listing prefers config.json, so a half-reverted container would report
		// one set of labels while its other document held another.
		{MetadataKindContainer, "ctr-started"},
		{MetadataKindContainer, "ctr-created"},
	} {
		t.Run(string(testCase.kind)+"/"+testCase.id, func(t *testing.T) {
			root := seedAppRoot(t)
			backups := filepath.Join(t.TempDir(), "private-backups")
			before := legacyTriple()
			record := phase3ARecord(
				t, testCase.kind, testCase.id, root, backups, before,
			)
			if err := rollbackMetadataApplication(record); err != nil {
				t.Fatalf("rollback: %v", err)
			}
			for _, file := range record.Files {
				if file.RestoredAs != RestoreExactBytes {
					t.Fatalf(
						"%s restored as %q, want %q",
						file.Document, file.RestoredAs, RestoreExactBytes,
					)
				}
				labels := labelsOnDisk(t, file.Path, file.LabelPath)
				if !sameLabels(labels, before) {
					t.Fatalf("%s holds %v, want %v", file.Document, labels, before)
				}
				// A foreign label must survive: the plan carries complete maps so
				// recovery cannot drop somebody else's key.
				if labels["com.example.foreign"] != "keep-me" {
					t.Fatalf("%s lost its foreign label: %v", file.Document, labels)
				}
			}
		})
	}
}

// TestRollbackIsIdempotent matters because rollback is the recovery path. It has
// to survive being run twice, and being resumed after an interruption partway
// through, without reporting the second run as a failure.
func TestRollbackIsIdempotent(t *testing.T) {
	root := seedAppRoot(t)
	backups := filepath.Join(t.TempDir(), "private-backups")
	before := legacyTriple()
	record := phase3ARecord(
		t, MetadataKindContainer, "ctr-started", root, backups, before,
	)
	if err := rollbackMetadataApplication(record); err != nil {
		t.Fatalf("first rollback: %v", err)
	}
	if err := rollbackMetadataApplication(record); err != nil {
		t.Fatalf("second rollback: %v", err)
	}
	for _, file := range record.Files {
		if file.RestoredAs != RestoreAlreadyBefore {
			t.Fatalf(
				"%s reported %q on the second run, want %q",
				file.Document, file.RestoredAs, RestoreAlreadyBefore,
			)
		}
		if labels := labelsOnDisk(
			t, file.Path, file.LabelPath,
		); !sameLabels(labels, before) {
			t.Fatalf("%s drifted on the second run: %v", file.Document, labels)
		}
	}
}

// TestRollbackReconcilesADocumentCreatedSinceTheMigration covers the case the
// recovery path exists in its current shape for. Starting a container
// materialises config.json from the runtime's in-memory model, so a rollback
// that restored only the documents it had recorded would leave the container
// reporting its migrated labels through the document the listing prefers.
func TestRollbackReconcilesADocumentCreatedSinceTheMigration(t *testing.T) {
	root := seedAppRoot(t)
	backups := filepath.Join(t.TempDir(), "private-backups")
	before := legacyTriple()
	record := phase3ARecord(
		t, MetadataKindContainer, "ctr-created", root, backups, before,
	)
	if len(record.Files) != 1 {
		t.Fatalf("fixture recorded %d documents, want 1", len(record.Files))
	}
	// The runtime materialises config.json carrying the labels the migration
	// wrote.
	created := filepath.Join(root, "containers", "ctr-created", "config.json")
	if err := os.WriteFile(
		created,
		[]byte(`{"id":"ctr-created","labels":{"`+CurrentManagedLabelKey+`":"v1"}}`),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	if err := rollbackMetadataApplication(record); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if len(record.Reconciled) != 1 || record.Reconciled[0] != "config.json" {
		t.Fatalf("rollback did not record the reconciliation: %#v", record.Reconciled)
	}
	if labels := labelsOnDisk(
		t, created, []string{"labels"},
	); !sameLabels(labels, before) {
		t.Fatalf("the created-since document was not reconciled: %v", labels)
	}
}

func TestRollbackRefusesACreatedSinceDocumentItCannotAccountFor(t *testing.T) {
	root := seedAppRoot(t)
	backups := filepath.Join(t.TempDir(), "private-backups")
	record := phase3ARecord(
		t, MetadataKindContainer, "ctr-created", root, backups, legacyTriple(),
	)
	// Neither the labels the migration wrote nor the ones it replaced.
	if err := os.WriteFile(
		filepath.Join(root, "containers", "ctr-created", "config.json"),
		[]byte(`{"id":"ctr-created","labels":{"com.example.other":"v9"}}`),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	err := rollbackMetadataApplication(record)
	if err == nil {
		t.Fatal("a document the rollback cannot account for was written anyway")
	}
	if !strings.Contains(err.Error(), "resolve it by hand") {
		t.Fatalf("unexpected refusal: %v", err)
	}
	// Nothing may have been restored: the check precedes every write, so the
	// resource is left whole rather than between two states.
	if labels := labelsOnDisk(
		t,
		record.Files[0].Path,
		record.Files[0].LabelPath,
	); labels[CurrentManagedLabelKey] != "v1" {
		t.Fatalf("a refused rollback still rewrote the recorded document: %v", labels)
	}
}

func TestRollbackRefusesADocumentInAnUnknownState(t *testing.T) {
	root := seedAppRoot(t)
	backups := filepath.Join(t.TempDir(), "private-backups")
	record := phase3ARecord(
		t, MetadataKindVolume, "vol-a", root, backups, legacyTriple(),
	)
	// A non-label field changed since the migration: restoring the old bytes over
	// it would revert whatever else recorded that change.
	if err := os.WriteFile(
		record.Files[0].Path,
		[]byte(`{"name":"vol-renamed","labels":{"`+CurrentManagedLabelKey+`":"v1"}}`),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	err := rollbackMetadataApplication(record)
	if !errors.Is(err, ErrRollbackUnknownState) {
		t.Fatalf("an unrecognised document was accepted for restore: %v", err)
	}
}

func TestRollbackRefusesACorruptedBackup(t *testing.T) {
	root := seedAppRoot(t)
	backups := filepath.Join(t.TempDir(), "private-backups")
	record := phase3ARecord(
		t, MetadataKindVolume, "vol-a", root, backups, legacyTriple(),
	)
	if err := os.WriteFile(
		record.Files[0].BackupPath,
		[]byte(`{"name":"vol-a","labels":{"com.example.tampered":"yes"}}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	err := rollbackMetadataApplication(record)
	if err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("a backup that no longer matches its digest was restored: %v", err)
	}
	if labels := labelsOnDisk(
		t, record.Files[0].Path, record.Files[0].LabelPath,
	); labels[CurrentManagedLabelKey] != "v1" {
		t.Fatalf("a refused rollback still rewrote the document: %v", labels)
	}
}

// TestRollbackRestoresLabelsThisBinaryCannotRead is the property that makes the
// retained path safe to keep at all. It restores recorded maps verbatim and
// never consults a label key, so it works on exactly the namespace Phase 3B
// removed from the reader.
func TestRollbackRestoresLabelsThisBinaryCannotRead(t *testing.T) {
	root := seedAppRoot(t)
	backups := filepath.Join(t.TempDir(), "private-backups")
	before := legacyTriple()
	record := phase3ARecord(t, MetadataKindVolume, "vol-a", root, backups, before)
	if class := ClassifyOwnership(record.AfterLabels); class != OwnershipOwned {
		t.Fatalf("the fixture is not owned before rollback: %q", class)
	}
	if err := rollbackMetadataApplication(record); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	restored := labelsOnDisk(t, record.Files[0].Path, record.Files[0].LabelPath)
	if restored[legacyManagedLabelKey] != "v1" {
		t.Fatalf("the retired namespace was not restored: %v", restored)
	}
	// And the resource is now invisible to this binary, which is the one-way
	// boundary the runbook documents.
	if class := ClassifyOwnership(restored); class != OwnershipMissingLabel {
		t.Fatalf("a rolled-back resource still classifies as %q", class)
	}
}

// A write whose rename succeeded but whose directory entry could not be flushed
// has already replaced the document. It is the one state that can leave a
// container half-reverted — one document rolled back, its sibling pushed
// forward — and it is unreachable without forcing an fsync failure after a
// successful rename. These cases force it.

// failDirectorySyncAfter makes the Nth directory sync fail, counting from one.
func failDirectorySyncAfter(t *testing.T, n int) {
	t.Helper()
	original := syncDirectoryEntry
	calls := 0
	syncDirectoryEntry = func(directory *directoryHandle) error {
		calls++
		if calls == n {
			return errors.New("injected directory sync failure")
		}
		return original(directory)
	}
	t.Cleanup(func() { syncDirectoryEntry = original })
}

// TestRollbackUnwindsALandedRecordedWrite covers the primary path: a recorded
// document is restored, its directory sync fails, and the resource must end
// wholly migrated rather than partly reverted.
func TestRollbackUnwindsALandedRecordedWrite(t *testing.T) {
	root := seedAppRoot(t)
	backups := filepath.Join(t.TempDir(), "private-backups")
	before := legacyTriple()
	record := phase3ARecord(
		t, MetadataKindContainer, "ctr-started", root, backups, before,
	)
	if len(record.Files) != 2 {
		t.Fatalf("fixture recorded %d documents, want 2", len(record.Files))
	}
	// The first restore in the loop is the last file; fail its sync.
	failDirectorySyncAfter(t, 1)
	err := rollbackMetadataApplication(record)
	if err == nil {
		t.Fatal("a durability-uncertain restore was reported as success")
	}
	// Every document must be back at what the migration wrote. A document left
	// at the pre-migration labels while its sibling holds the migrated ones is
	// the half-reverted container this path exists to prevent.
	for _, file := range record.Files {
		labels := labelsOnDisk(t, file.Path, file.LabelPath)
		if !sameLabels(labels, record.AfterLabels) {
			t.Fatalf(
				"%s ended at %v, want the migrated labels %v",
				file.Document, labels, record.AfterLabels,
			)
		}
	}
	// The record has to agree with the disk. The failure path prints these
	// outcomes to tell an operator which half of the host is reverted, so an
	// outcome left on a document the unwind put back is a false statement about
	// durable state — and the operator's next decision is whether re-running the
	// plan is safe.
	if outcomes := record.RestoreOutcomes(); len(outcomes) != 0 {
		t.Fatalf(
			"a fully undone rollback still reports restorations: %v", outcomes,
		)
	}
	for _, file := range record.Files {
		if file.RestoredAs != "" {
			t.Fatalf(
				"%s kept outcome %q after being put back",
				file.Document, file.RestoredAs,
			)
		}
	}
}

// TestResumedRollbackKeepsWhatTheInterruptedRunAlreadyReverted covers the
// recovery the runbook actually prescribes. An interrupted rollback leaves some
// documents reverted; re-running the same plan is documented as idempotent. On
// that re-run an already-reverted document returns RestoreAlreadyBefore, which
// reports no write — so it must stay out of the unwind set. Unwinding it would
// push a document this run merely observed forward to the migrated labels and
// undo the first run's completed work, which is losing ground, not resuming.
func TestResumedRollbackKeepsWhatTheInterruptedRunAlreadyReverted(t *testing.T) {
	root := seedAppRoot(t)
	backups := filepath.Join(t.TempDir(), "private-backups")
	before := legacyTriple()
	record := phase3ARecord(
		t, MetadataKindContainer, "ctr-started", root, backups, before,
	)
	if len(record.Files) != 2 {
		t.Fatalf("fixture recorded %d documents, want 2", len(record.Files))
	}
	// Stand in for the interrupted run: the loop restores in reverse, so the
	// last file is the one a previous attempt would have completed first.
	done := record.Files[len(record.Files)-1]
	backup, err := os.ReadFile(done.BackupPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(done.Path, backup, 0o644); err != nil {
		t.Fatal(err)
	}
	// `done` needs no write, so the first directory sync of this run belongs to
	// the other document. Fail it.
	failDirectorySyncAfter(t, 1)
	if err := rollbackMetadataApplication(record); err == nil {
		t.Fatal("a durability-uncertain restore was reported as success")
	}
	labels := labelsOnDisk(t, done.Path, done.LabelPath)
	if !sameLabels(labels, record.BeforeLabels) {
		t.Fatalf(
			"the resumed rollback undid work the interrupted run had completed: "+
				"%s ended at %v, want the pre-migration labels %v",
			done.Document, labels, record.BeforeLabels,
		)
	}
	if done.RestoredAs != RestoreAlreadyBefore {
		t.Fatalf(
			"%s reported %q, want %q",
			done.Document, done.RestoredAs, RestoreAlreadyBefore,
		)
	}
}

// TestRollbackUnwindsALandedCreatedSinceWrite covers the created-since path,
// whose record runs the opposite way round: applyMetadataFile records "what I
// found" and "what I wrote", so handing it to reapply unswapped would rewrite
// the rollback destination that is already on disk.
func TestRollbackUnwindsALandedCreatedSinceWrite(t *testing.T) {
	root := seedAppRoot(t)
	backups := filepath.Join(t.TempDir(), "private-backups")
	before := legacyTriple()
	record := phase3ARecord(
		t, MetadataKindContainer, "ctr-created", root, backups, before,
	)
	if len(record.Files) != 1 {
		t.Fatalf("fixture recorded %d documents, want 1", len(record.Files))
	}
	created := filepath.Join(root, "containers", "ctr-created", "config.json")
	if err := os.WriteFile(
		created,
		[]byte(`{"id":"ctr-created","labels":{"`+CurrentManagedLabelKey+`":"v1"}}`),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	// Sync 1 is the recorded document's restore; sync 2 is the created-since
	// write, which is the one whose record direction was wrong.
	failDirectorySyncAfter(t, 2)
	err := rollbackMetadataApplication(record)
	if err == nil {
		t.Fatal("a durability-uncertain reconciliation was reported as success")
	}
	for path, labelPath := range map[string][]string{
		created:              {"labels"},
		record.Files[0].Path: record.Files[0].LabelPath,
	} {
		labels := labelsOnDisk(t, path, labelPath)
		if !sameLabels(labels, record.AfterLabels) {
			t.Fatalf(
				"%s ended at %v, want the migrated labels %v",
				filepath.Base(path), labels, record.AfterLabels,
			)
		}
	}
}
