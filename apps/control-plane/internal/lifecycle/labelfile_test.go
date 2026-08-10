package lifecycle

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// These tests cover the part of Phase 3A that edits somebody else's files. The
// interesting cases are all refusals: the migration is only safe because it
// stops on anything it did not expect.

func writeProbeDocument(t *testing.T, directory string) string {
	t.Helper()
	path := filepath.Join(directory, "entity.json")
	if err := os.WriteFile(
		path, []byte(foundationVolumeDocument), 0o644,
	); err != nil {
		t.Fatalf("seed probe document: %v", err)
	}
	return path
}

func TestInspectMetadataFileReadsLabelsAndHash(t *testing.T) {
	path := writeProbeDocument(t, t.TempDir())
	state, err := inspectMetadataFile(metadataFileRef{
		Path: path, LabelPath: []string{"labels"},
	})
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if state.Labels["com.mrbaron3.workflow.agentopsctl"] != "v1" {
		t.Errorf("unexpected labels: %#v", state.Labels)
	}
	if len(state.SHA256) != 64 {
		t.Errorf("expected a sha256 digest, got %q", state.SHA256)
	}
	if state.Mode.Perm() != 0o644 {
		t.Errorf("expected mode 0644, got %v", state.Mode.Perm())
	}
}

func TestInspectMetadataFileRejectsSymlinkedDocument(t *testing.T) {
	directory := t.TempDir()
	real := writeProbeDocument(t, directory)
	link := filepath.Join(directory, "linked.json")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	_, err := inspectMetadataFile(metadataFileRef{
		Path: link, LabelPath: []string{"labels"},
	})
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("expected a symlink refusal, got %v", err)
	}
}

func TestInspectMetadataFileRejectsSymlinkedParentDirectory(t *testing.T) {
	// A symlinked parent is the interesting case: lstat on the file alone
	// reports a perfectly ordinary regular file while the write lands somewhere
	// the operator never named.
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeProbeDocument(t, real)
	link := filepath.Join(root, "linked")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	_, err := inspectMetadataFile(metadataFileRef{
		Path: filepath.Join(link, "entity.json"), LabelPath: []string{"labels"},
	})
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("expected a symlink refusal, got %v", err)
	}
}

func TestInspectMetadataFileRejectsNonRegularFile(t *testing.T) {
	directory := t.TempDir()
	nested := filepath.Join(directory, "entity.json")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	_, err := inspectMetadataFile(metadataFileRef{
		Path: nested, LabelPath: []string{"labels"},
	})
	if err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("expected a regular-file refusal, got %v", err)
	}
}

func TestInspectMetadataFileRejectsGroupOrWorldWritableFile(t *testing.T) {
	for name, mode := range map[string]os.FileMode{
		"group writable": 0o664,
		"world writable": 0o646,
	} {
		t.Run(name, func(t *testing.T) {
			path := writeProbeDocument(t, t.TempDir())
			if err := os.Chmod(path, mode); err != nil {
				t.Fatalf("chmod: %v", err)
			}
			_, err := inspectMetadataFile(metadataFileRef{
				Path: path, LabelPath: []string{"labels"},
			})
			if err == nil || !strings.Contains(err.Error(), "writable") {
				t.Fatalf("expected a writability refusal, got %v", err)
			}
		})
	}
}

func TestInspectMetadataFileRejectsForeignOwner(t *testing.T) {
	path := writeProbeDocument(t, t.TempDir())
	state, err := inspectMetadataFile(metadataFileRef{
		Path: path, LabelPath: []string{"labels"},
	})
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	// The owner check is asserted through its predicate because a test cannot
	// create a file owned by another user without privileges it must not have.
	if err := requireOwnedByCurrentUser(
		path, state.UID+1, state.GID,
	); err == nil {
		t.Fatal("expected a foreign owner to be refused")
	}
	if err := requireOwnedByCurrentUser(
		path, state.UID, state.GID,
	); err != nil {
		t.Fatalf("expected the real owner to be accepted, got %v", err)
	}
}

func TestApplyMetadataFileWritesAtomicallyAndBacksUp(t *testing.T) {
	directory := t.TempDir()
	path := writeProbeDocument(t, directory)
	backups := filepath.Join(directory, "backup")
	state, err := inspectMetadataFile(metadataFileRef{
		Path: path, LabelPath: []string{"labels"},
	})
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	updated := map[string]string{
		"com.mrbaron3.workflow.agentopsctl": "v1",
		"com.mrbaron3.servo.agentopsctl":    "v1",
	}
	backupPath := filepath.Join(backups, "entity.json")
	result, err := applyMetadataFile(state, updated, backupPath)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if result.BeforeSHA256 == result.AfterSHA256 {
		t.Error("expected the digest to change")
	}
	// The backup has to be the original bytes exactly, because it is the only
	// thing rollback can restore from.
	backup, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if string(backup) != foundationVolumeDocument {
		t.Errorf("backup is not the original bytes:\n%s", backup)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	if !strings.Contains(
		string(after), `"com.mrbaron3.servo.agentopsctl":"v1"`,
	) {
		t.Errorf("label was not written:\n%s", after)
	}
	// Mode has to survive: these files are read by a launchd service running as
	// the same user, and a mode change is a behaviour change.
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("expected mode 0644 preserved, got %v", info.Mode().Perm())
	}
	// No temporary file may be left in the directory.
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp") {
			t.Errorf("temporary file left behind: %s", entry.Name())
		}
	}
}

func TestApplyMetadataFileRefusesToOverwriteAnExistingBackup(t *testing.T) {
	directory := t.TempDir()
	path := writeProbeDocument(t, directory)
	backupPath := filepath.Join(directory, "backup", "entity.json")
	if err := os.MkdirAll(filepath.Dir(backupPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(backupPath, []byte("earlier run"), 0o644); err != nil {
		t.Fatalf("seed backup: %v", err)
	}
	state, err := inspectMetadataFile(metadataFileRef{
		Path: path, LabelPath: []string{"labels"},
	})
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if _, err := applyMetadataFile(
		state, map[string]string{"com.mrbaron3.servo.agentopsctl": "v1"},
		backupPath,
	); err == nil {
		t.Fatal("expected an existing backup to stop the write")
	}
	// The document must be untouched when the backup could not be taken.
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(current) != foundationVolumeDocument {
		t.Errorf("document changed despite the backup failure:\n%s", current)
	}
}

func TestApplyMetadataFileRefusesWhenTheDocumentChangedSinceInspection(t *testing.T) {
	directory := t.TempDir()
	path := writeProbeDocument(t, directory)
	state, err := inspectMetadataFile(metadataFileRef{
		Path: path, LabelPath: []string{"labels"},
	})
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	// Somebody else edits the file between planning and applying.
	if err := os.WriteFile(
		path, []byte(`{"labels":{"other":"1"}}`), 0o644,
	); err != nil {
		t.Fatalf("concurrent write: %v", err)
	}
	if _, err := applyMetadataFile(
		state, map[string]string{"com.mrbaron3.servo.agentopsctl": "v1"},
		filepath.Join(directory, "backup", "entity.json"),
	); err == nil {
		t.Fatal("expected a concurrent change to stop the write")
	}
}

func TestRestoreMetadataFileReturnsTheExactOriginalBytes(t *testing.T) {
	directory := t.TempDir()
	path := writeProbeDocument(t, directory)
	backupPath := filepath.Join(directory, "backup", "entity.json")
	state, err := inspectMetadataFile(metadataFileRef{
		Path: path, LabelPath: []string{"labels"},
	})
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	result, err := applyMetadataFile(
		state,
		map[string]string{"com.mrbaron3.servo.agentopsctl": "v1"},
		backupPath,
	)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	outcome, err := restoreMetadataFile(result)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if outcome != RestoreExactBytes {
		t.Fatalf("expected a byte-exact restore, got %q", outcome)
	}
	restored, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read restored: %v", err)
	}
	if string(restored) != foundationVolumeDocument {
		t.Fatalf(
			"rollback did not restore the original bytes:\n want %s\n got  %s",
			foundationVolumeDocument, restored,
		)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("rollback changed mode to %v", info.Mode().Perm())
	}
}

func TestRestoreMetadataFileRefusesACorruptedBackup(t *testing.T) {
	directory := t.TempDir()
	path := writeProbeDocument(t, directory)
	backupPath := filepath.Join(directory, "backup", "entity.json")
	state, err := inspectMetadataFile(metadataFileRef{
		Path: path, LabelPath: []string{"labels"},
	})
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	result, err := applyMetadataFile(
		state,
		map[string]string{"com.mrbaron3.servo.agentopsctl": "v1"},
		backupPath,
	)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if err := os.WriteFile(backupPath, []byte(`{"labels":{}}`), 0o644); err != nil {
		t.Fatalf("corrupt backup: %v", err)
	}
	// Rollback verifies the backup against the digest recorded when it was
	// taken. Restoring bytes that are not the originals would be a second
	// unreviewed mutation wearing the word "rollback".
	if _, err := restoreMetadataFile(result); err == nil {
		t.Fatal("expected a corrupted backup to be refused")
	}
}

func TestRequireNoSymlinkInPathAcceptsAnOrdinaryPath(t *testing.T) {
	directory := t.TempDir()
	path := writeProbeDocument(t, directory)
	if err := requireNoSymlinkInPath(path, directory); err != nil {
		t.Fatalf("expected an ordinary path to be accepted, got %v", err)
	}
}

func TestUmaskDoesNotWidenTheTemporaryFile(t *testing.T) {
	// A 0600 source must not come back as 0644 because the temporary file was
	// created with a default mode and then chmod-ed.
	directory := t.TempDir()
	path := writeProbeDocument(t, directory)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	state, err := inspectMetadataFile(metadataFileRef{
		Path: path, LabelPath: []string{"labels"},
	})
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if _, err := applyMetadataFile(
		state, map[string]string{"com.mrbaron3.servo.agentopsctl": "v1"},
		filepath.Join(directory, "backup", "entity.json"),
	); err != nil {
		t.Fatalf("apply: %v", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("expected 0600 preserved, got %v", info.Mode().Perm())
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("no syscall stat on this platform")
	}
	if int(stat.Uid) != os.Getuid() {
		t.Errorf("owner changed to %d", stat.Uid)
	}
}

func TestRestoreMetadataFileRelabelsAReserialisedDocument(t *testing.T) {
	// Apple Container rewrites volumes/<name>/entity.json on every
	// `container system start`, preserving every field value but not the key
	// order. A rollback taken after a restart therefore cannot match bytes, and
	// restoring the stale bytes anyway would revert whatever else the runtime
	// had recorded. The migration re-applies the recorded pre-migration labels
	// instead, and only after proving nothing but the labels differs.
	directory := t.TempDir()
	path := writeProbeDocument(t, directory)
	state, err := inspectMetadataFile(metadataFileRef{
		Path: path, LabelPath: []string{"labels"},
	})
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	result, err := applyMetadataFile(
		state,
		map[string]string{"com.mrbaron3.servo.agentopsctl": "v1"},
		filepath.Join(directory, "backup", "entity.json"),
	)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	// Re-serialise with the keys in a different order, exactly as the runtime
	// does, keeping every value identical.
	reserialised := `{"labels":{"com.mrbaron3.servo.agentopsctl":"v1"},` +
		`"sizeInBytes":549755813888,` +
		`"source":"\/Users\/example\/volumes\/probe\/volume.img",` +
		`"name":"probe","creationDate":808039875.620406,"options":{},` +
		`"format":"ext4","driver":"local"}`
	if err := os.WriteFile(path, []byte(reserialised), 0o644); err != nil {
		t.Fatalf("re-serialise: %v", err)
	}
	outcome, err := restoreMetadataFile(result)
	if err != nil {
		t.Fatalf("restore a re-serialised document: %v", err)
	}
	if outcome != RestoreRelabelled {
		t.Fatalf("expected a relabelling restore, got %q", outcome)
	}
	restored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	labels, err := readLabelsAtPath(restored, []string{"labels"})
	if err != nil {
		t.Fatal(err)
	}
	if !sameLabels(labels, result.BeforeLabels) {
		t.Fatalf("labels were not restored: %#v", labels)
	}
	// The runtime's key order is kept: this restore rewrites labels, not layout.
	if !strings.HasPrefix(string(restored), `{"labels":`) {
		t.Fatalf("restore reordered the document:\n%s", restored)
	}
}

func TestRestoreMetadataFileRefusesWhenANonLabelFieldChanged(t *testing.T) {
	directory := t.TempDir()
	path := writeProbeDocument(t, directory)
	state, err := inspectMetadataFile(metadataFileRef{
		Path: path, LabelPath: []string{"labels"},
	})
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	result, err := applyMetadataFile(
		state,
		map[string]string{"com.mrbaron3.servo.agentopsctl": "v1"},
		filepath.Join(directory, "backup", "entity.json"),
	)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	// Same labels, but a different size: this is not a re-serialisation, it is
	// a real change somebody else made, and rollback must not paper over it.
	tampered := `{"labels":{"com.mrbaron3.servo.agentopsctl":"v1"},` +
		`"sizeInBytes":1,` +
		`"source":"\/Users\/example\/volumes\/probe\/volume.img",` +
		`"name":"probe","creationDate":808039875.620406,"options":{},` +
		`"format":"ext4","driver":"local"}`
	if err := os.WriteFile(path, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := restoreMetadataFile(result); err == nil {
		t.Fatal("expected a changed non-label field to stop the rollback")
	}
}

func TestApplyMetadataFileRefusesWhenTheFileWasReplacedByAnotherInode(t *testing.T) {
	// A digest comparison alone would accept a different file that happens to
	// hold the same bytes. Binding the check to the inode is what makes
	// "this is still the same document" true rather than merely plausible.
	directory := t.TempDir()
	path := writeProbeDocument(t, directory)
	state, err := inspectMetadataFile(metadataFileRef{
		Path: path, LabelPath: []string{"labels"},
	})
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	// Replace the file with a fresh inode carrying byte-identical content.
	replacement := filepath.Join(directory, "replacement.json")
	if err := os.WriteFile(
		replacement, []byte(foundationVolumeDocument), 0o644,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if _, err := applyMetadataFile(
		state, map[string]string{"com.mrbaron3.servo.agentopsctl": "v1"},
		filepath.Join(directory, "backup", "entity.json"),
	); err == nil {
		t.Fatal("expected a replaced inode to stop the write")
	} else if !strings.Contains(err.Error(), "different file") {
		t.Fatalf("expected an inode refusal, got %v", err)
	}
}

func TestOpenDirectoryRefusesAGroupWritableDirectory(t *testing.T) {
	// A directory other users can write is one where the destination name can be
	// swapped between the final check and the rename.
	root := t.TempDir()
	wide := filepath.Join(root, "wide")
	if err := os.Mkdir(wide, 0o755); err != nil {
		t.Fatal(err)
	}
	// Chmod rather than a wide Mkdir: the creation mode is filtered by the
	// umask, so Mkdir(0o775) would silently produce an ordinary 0755 directory
	// and the test would pass without testing anything.
	if err := os.Chmod(wide, 0o775); err != nil {
		t.Fatal(err)
	}
	if _, err := openDirectory(wide); err == nil {
		t.Fatal("expected a group writable directory to be refused")
	}
}

func TestWriteThroughDirectoryLeavesNoTemporaryBehindOnFailure(t *testing.T) {
	directory := t.TempDir()
	handle, err := openDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	// Renaming onto a name that is a directory fails, which exercises the
	// cleanup path after the temporary file has already been written.
	if err := os.Mkdir(filepath.Join(directory, "occupied"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeThroughDirectory(
		handle, "occupied", []byte("{}"), 0o644, os.Getuid(), os.Getgid(),
	); err == nil {
		t.Fatal("expected the rename to fail")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp") {
			t.Fatalf("temporary file left behind: %s", entry.Name())
		}
	}
}
