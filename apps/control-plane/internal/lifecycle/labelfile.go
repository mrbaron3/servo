package lifecycle

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
)

// This file performs the only privileged act in Phase 3A: rewriting a metadata
// document that belongs to Apple Container. Everything here is arranged so that
// an interrupted run leaves either the original bytes or the migrated bytes on
// disk, never a partial document, and so that the original bytes can always be
// produced again from a backup whose digest was recorded when it was taken.

// metadataFileRef names one metadata document and where its labels live inside
// it.
type metadataFileRef struct {
	// Path is the absolute location of the document.
	Path string
	// LabelPath is the field path to the labels object, which is ["labels"] for
	// volumes and networks and ["containerConfiguration", "labels"] inside a
	// container's runtime-configuration.json.
	LabelPath []string
}

// metadataFileState is one document as it was found, verified, and hashed
// before any change was planned against it.
type metadataFileState struct {
	Ref    metadataFileRef
	Bytes  []byte
	SHA256 string
	Mode   os.FileMode
	UID    int
	GID    int
	// Identity is the inode the bytes, mode, and owner above were all read
	// from. Re-checking it at apply time is what makes "this is still the same
	// file" a statement about an inode rather than about a name.
	Identity fileIdentity
	Labels   map[string]string
}

// metadataFileApplication is the durable record of one rewritten document. It
// carries everything rollback needs, so recovery never has to re-derive what
// the original looked like.
type metadataFileApplication struct {
	// Path and BackupPath are absolute and therefore never serialised into the
	// committed evidence. They are carried in the private rollback plan that
	// lives inside the 0700 backup root, next to the backups they name.
	Path       string   `json:"-"`
	LabelPath  []string `json:"labelPath"`
	BackupPath string   `json:"-"`
	// Document and Backup are the same two locations rendered relative to the
	// application root and the backup root, which is what the evidence records.
	Document     string      `json:"document"`
	Backup       string      `json:"backup"`
	BeforeSHA256 string      `json:"beforeSha256"`
	AfterSHA256  string      `json:"afterSha256"`
	BackupSHA256 string      `json:"backupSha256"`
	Mode         os.FileMode `json:"mode"`
	// BeforeLabels and AfterLabels are the COMPLETE label maps, foreign labels
	// included, because a rollback has to restore a resource's labels exactly
	// and a truncated map would silently drop somebody else's label. They are
	// never serialised into the committed evidence: a foreign label's value is
	// arbitrary third-party text that may hold anything at all. They travel in
	// the private rollback plan instead.
	BeforeLabels map[string]string `json:"-"`
	AfterLabels  map[string]string `json:"-"`
	// BeforeOwnership and AfterOwnership are the ownership keys alone, which
	// is what the evidence is allowed to publish.
	BeforeOwnership map[string]string `json:"beforeOwnershipLabels"`
	AfterOwnership  map[string]string `json:"afterOwnershipLabels"`
	// RestoredAs records how a rollback returned this document, when one ran.
	RestoredAs RestoreOutcome `json:"restoredAs,omitempty"`
}

// Ref rebuilds the reference this application was made from, so recovery paths
// can re-inspect the document without restating where its labels live.
func (application *metadataFileApplication) Ref() metadataFileRef {
	return metadataFileRef{
		Path: application.Path, LabelPath: application.LabelPath,
	}
}

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// requireOwnedByCurrentUser refuses a document owned by anybody else. Apple
// Container's metadata belongs to the user running it, and a file that does not
// is either not the file this tool means to edit or is a file it has no
// business editing.
func requireOwnedByCurrentUser(path string, uid, gid int) error {
	if uid != os.Getuid() {
		return fmt.Errorf(
			"%s is owned by uid %d, not the current user %d",
			path, uid, os.Getuid(),
		)
	}
	if gid != os.Getgid() && !inGroup(gid) {
		return fmt.Errorf(
			"%s has group %d, which the current user is not a member of",
			path, gid,
		)
	}
	return nil
}

func inGroup(gid int) bool {
	groups, err := os.Getgroups()
	if err != nil {
		return false
	}
	for _, candidate := range groups {
		if candidate == gid {
			return true
		}
	}
	return false
}

// statMetadataFile verifies that a path is a regular file the current user owns
// and that neither it nor its immediate parent is a symbolic link.
func statMetadataFile(path string) (os.FileInfo, int, int, error) {
	parent := filepath.Dir(path)
	parentInfo, err := os.Lstat(parent)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("inspect %s: %w", parent, err)
	}
	if parentInfo.Mode()&os.ModeSymlink != 0 {
		return nil, 0, 0, fmt.Errorf("%s is a symbolic link", parent)
	}
	if !parentInfo.IsDir() {
		return nil, 0, 0, fmt.Errorf("%s is not a directory", parent)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("inspect %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, 0, 0, fmt.Errorf("%s is a symbolic link", path)
	}
	if !info.Mode().IsRegular() {
		return nil, 0, 0, fmt.Errorf("%s is not a regular file", path)
	}
	// A document any other user can write is not one whose contents this tool
	// can reason about between planning and applying.
	if info.Mode().Perm()&0o022 != 0 {
		return nil, 0, 0, fmt.Errorf(
			"%s is group or world writable (%v)", path, info.Mode().Perm(),
		)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, 0, 0, fmt.Errorf("%s has no owner information", path)
	}
	uid, gid := int(stat.Uid), int(stat.Gid)
	if err := requireOwnedByCurrentUser(path, uid, gid); err != nil {
		return nil, 0, 0, err
	}
	return info, uid, gid, nil
}

// inspectMetadataFile reads and verifies one document without changing it.
// Bytes, mode, owner, and inode identity all come from a single open
// descriptor, so they cannot describe different files.
func inspectMetadataFile(ref metadataFileRef) (*metadataFileState, error) {
	directory, err := openDirectory(filepath.Dir(ref.Path))
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	return inspectMetadataFileIn(directory, ref)
}

func inspectMetadataFileIn(
	directory *directoryHandle,
	ref metadataFileRef,
) (*metadataFileState, error) {
	file, err := directory.openFile(filepath.Base(ref.Path))
	if err != nil {
		return nil, err
	}
	defer file.Close()
	mode, uid, gid, identity, err := statDescriptor(file, ref.Path)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", ref.Path, err)
	}
	// The round-trip guard runs before the document is ever considered a
	// migration target, so an unrecognised shape is reported during planning
	// rather than discovered halfway through a sweep.
	if err := requireByteStableRoundTrip(data, ref.LabelPath); err != nil {
		return nil, fmt.Errorf("%s: %w", ref.Path, err)
	}
	labels, err := readLabelsAtPath(data, ref.LabelPath)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ref.Path, err)
	}
	return &metadataFileState{
		Ref:      ref,
		Bytes:    data,
		SHA256:   digestOf(data),
		Mode:     mode,
		UID:      uid,
		GID:      gid,
		Identity: identity,
		Labels:   labels,
	}, nil
}

// applyMetadataFile rewrites one document's labels. The order is deliberate:
// the document is re-opened through the validated directory and proven to be
// the same inode with the same bytes it was inspected as, the backup is taken
// and durably flushed, and only then is the replacement written through a
// same-directory temporary file and a descriptor-relative rename.
func applyMetadataFile(
	state *metadataFileState,
	labels map[string]string,
	backupPath string,
) (*metadataFileApplication, error) {
	directory, err := openDirectory(filepath.Dir(state.Ref.Path))
	if err != nil {
		return nil, err
	}
	defer directory.Close()

	// Between planning and applying, anything could have touched the file.
	// Re-reading through the validated directory and comparing both the inode
	// and the digest is what makes the plan an accurate description of what is
	// about to happen; comparing the digest alone would accept a different file
	// that happened to hold the same bytes, and comparing the path alone would
	// accept a replacement.
	current, err := inspectMetadataFileIn(directory, state.Ref)
	if err != nil {
		return nil, err
	}
	if current.Identity != state.Identity {
		return nil, fmt.Errorf(
			"%s is a different file than the one inspected; re-run the plan",
			state.Ref.Path,
		)
	}
	if current.SHA256 != state.SHA256 {
		return nil, fmt.Errorf(
			"%s changed since it was inspected; re-run the plan",
			state.Ref.Path,
		)
	}
	updated, err := rewriteLabels(current.Bytes, state.Ref.LabelPath, labels)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", state.Ref.Path, err)
	}

	// The backup is written first and with O_EXCL. Evidence of an irreversible
	// act must never silently replace an earlier run's account of it, and a
	// migration that cannot record what it is about to overwrite does not start.
	if err := writeBackup(backupPath, current.Bytes); err != nil {
		return nil, err
	}

	writeErr := writeThroughDirectory(
		directory, filepath.Base(state.Ref.Path), updated,
		current.Mode.Perm(), current.UID, current.GID,
	)
	if writeErr != nil && !WriteLanded(writeErr) {
		return nil, writeErr
	}
	// When the write landed but its durability is uncertain, the record is
	// returned ALONGSIDE the error so the caller can unwind this document.
	return &metadataFileApplication{
		Path:            state.Ref.Path,
		LabelPath:       state.Ref.LabelPath,
		BackupPath:      backupPath,
		BeforeSHA256:    state.SHA256,
		AfterSHA256:     digestOf(updated),
		BackupSHA256:    digestOf(current.Bytes),
		Mode:            current.Mode.Perm(),
		BeforeLabels:    state.Labels,
		AfterLabels:     labels,
		BeforeOwnership: ownershipLabelSubset(state.Labels),
		AfterOwnership:  ownershipLabelSubset(labels),
	}, writeErr
}

// writeBackup records the pre-migration bytes. O_EXCL rather than a truncating
// write: a second run that chose the same name must fail loudly rather than
// replace the first run's account of what it overwrote.
func writeBackup(backupPath string, contents []byte) error {
	if err := os.MkdirAll(filepath.Dir(backupPath), 0o700); err != nil {
		return fmt.Errorf("create backup directory: %w", err)
	}
	backup, err := os.OpenFile(
		backupPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600,
	)
	if err != nil {
		return fmt.Errorf("open backup %s: %w", backupPath, err)
	}
	if _, err := backup.Write(contents); err != nil {
		backup.Close()
		return fmt.Errorf("write backup %s: %w", backupPath, err)
	}
	if err := backup.Sync(); err != nil {
		backup.Close()
		return fmt.Errorf("flush backup %s: %w", backupPath, err)
	}
	if err := backup.Close(); err != nil {
		return fmt.Errorf("close backup %s: %w", backupPath, err)
	}
	return syncDirectory(filepath.Dir(backupPath))
}

// writeFileAtomically replaces a file's contents through its own directory.
func writeFileAtomically(
	path string,
	data []byte,
	mode os.FileMode,
	uid, gid int,
) error {
	directory, err := openDirectory(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return writeThroughDirectory(
		directory, filepath.Base(path), data, mode, uid, gid,
	)
}

// writeThroughDirectory writes a replacement without ever exposing a partially
// written document. The temporary file is created in the destination directory
// so the rename cannot cross a filesystem boundary, its mode is set at creation
// rather than left to the umask, and both the file and the directory entry are
// flushed before the operation is considered done. Every step is relative to an
// already-validated directory descriptor, so no step can be redirected by
// swapping a directory on the path.
func writeThroughDirectory(
	directory *directoryHandle,
	name string,
	data []byte,
	mode os.FileMode,
	uid, gid int,
) error {
	temporaryName, err := temporaryFileName()
	if err != nil {
		return err
	}
	temporary, err := directory.createTemporary(temporaryName, mode)
	if err != nil {
		return err
	}
	cleanup := func() {
		temporary.Close()
		directory.remove(temporaryName)
	}
	if _, err := temporary.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("write %s: %w", temporaryName, err)
	}
	// Chmod explicitly: the creation mode is filtered by the umask, and the
	// replacement has to carry the mode the original carried.
	if err := temporary.Chmod(mode.Perm()); err != nil {
		cleanup()
		return fmt.Errorf("set mode on %s: %w", temporaryName, err)
	}
	if err := matchOwnership(temporary, temporaryName, uid, gid); err != nil {
		cleanup()
		return err
	}
	if err := temporary.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("flush %s: %w", temporaryName, err)
	}
	if err := temporary.Close(); err != nil {
		directory.remove(temporaryName)
		return fmt.Errorf("close %s: %w", temporaryName, err)
	}
	if err := directory.rename(temporaryName, name); err != nil {
		directory.remove(temporaryName)
		return err
	}
	// Past this point the replacement IS the file. A directory-sync failure
	// means durability is uncertain, not that the rename did not happen, and
	// reporting it as an ordinary failure would make the caller drop the record
	// it needs to unwind — leaving a multi-document resource half-migrated with
	// nothing to roll back from.
	if err := directory.sync(); err != nil {
		return errDurabilityUncertain{err: err}
	}
	return nil
}

// errDurabilityUncertain marks a write whose rename succeeded but whose
// directory entry could not be flushed. The bytes on disk are the new ones.
type errDurabilityUncertain struct{ err error }

func (e errDurabilityUncertain) Error() string {
	return "the replacement was written but its directory entry could not be " +
		"flushed, so it may not survive a crash: " + e.err.Error()
}

func (e errDurabilityUncertain) Unwrap() error { return e.err }

// WriteLanded reports whether an error came from a write that had already
// replaced the destination. Callers use it to keep the record they need to
// unwind rather than discarding it with the error.
func WriteLanded(err error) bool {
	var uncertain errDurabilityUncertain
	return errors.As(err, &uncertain)
}

// temporaryFileName returns a name no concurrent run can collide with. The
// randomness matters because the create is O_EXCL: a predictable name that
// another run left behind would fail this run rather than be overwritten.
func temporaryFileName() (string, error) {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate temporary file name: %w", err)
	}
	return ".agentopsctl-label-" + hex.EncodeToString(raw) + ".tmp", nil
}

// matchOwnership makes the replacement carry the original's owner and group. A
// new file inherits the directory's group on macOS, which is usually already
// right; when it is not, the group is corrected and then verified rather than
// assumed.
func matchOwnership(file *os.File, path string, uid, gid int) error {
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect %s: %w", path, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s has no owner information", path)
	}
	if int(stat.Uid) == uid && int(stat.Gid) == gid {
		return nil
	}
	if err := file.Chown(uid, gid); err != nil {
		return fmt.Errorf(
			"restore owner %d:%d on %s: %w", uid, gid, path, err,
		)
	}
	info, err = file.Stat()
	if err != nil {
		return fmt.Errorf("inspect %s: %w", path, err)
	}
	if stat, ok = info.Sys().(*syscall.Stat_t); !ok ||
		int(stat.Uid) != uid || int(stat.Gid) != gid {
		return fmt.Errorf("could not restore owner %d:%d on %s", uid, gid, path)
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("flush directory %s: %w", path, err)
	}
	return nil
}

// ErrRollbackUnknownState marks a document that is neither its pre-migration
// nor its post-migration content, and whose non-label fields do not match what
// the migration recorded either. Restoring over it would be a second unreviewed
// mutation wearing the word "rollback".
var ErrRollbackUnknownState = errors.New(
	"document is in neither its recorded before nor after state",
)

// RestoreOutcome names how one document was returned to its pre-migration
// labels. The distinction is recorded in the evidence rather than smoothed over,
// because the two modes carry different guarantees.
type RestoreOutcome string

const (
	// RestoreAlreadyBefore means the document was never migrated, or was
	// already rolled back. This is what makes an interrupted run resumable.
	RestoreAlreadyBefore RestoreOutcome = "already-before"
	// RestoreExactBytes means the document was byte-identical to what the
	// migration wrote and has been replaced with the backup byte for byte.
	RestoreExactBytes RestoreOutcome = "bytes"
	// RestoreRelabelled means Apple Container had re-serialised the document
	// since the migration wrote it, so the recorded pre-migration labels were
	// written back into the document as it stands now. This is only taken after
	// proving every non-label field still matches, by value, what the migration
	// left behind.
	//
	// It exists because the runtime rewrites volumes/<name>/entity.json on every
	// `container system start`, preserving field values but not key order.
	// Restoring the old bytes over that would silently revert whatever else the
	// runtime had recorded since.
	RestoreRelabelled RestoreOutcome = "relabelled"
)

// restoreMetadataFile returns one document's ownership labels to what they were
// before the migration touched it, preferring an exact byte restore and falling
// back — explicitly, and only on proof — to rewriting the labels in place.
func restoreMetadataFile(
	application *metadataFileApplication,
) (RestoreOutcome, error) {
	backup, err := os.ReadFile(application.BackupPath)
	if err != nil {
		return "", fmt.Errorf(
			"read backup %s: %w", application.BackupPath, err,
		)
	}
	// The backup is verified against the digest taken when it was written, so a
	// damaged or replaced backup cannot be restored on top of a live file.
	if digestOf(backup) != application.BackupSHA256 {
		return "", fmt.Errorf(
			"backup %s no longer matches the digest recorded when it was taken",
			application.BackupPath,
		)
	}
	if digestOf(backup) != application.BeforeSHA256 {
		return "", fmt.Errorf(
			"backup %s is not the pre-migration content of %s",
			application.BackupPath, application.Path,
		)
	}
	current, err := os.ReadFile(application.Path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", application.Path, err)
	}
	// Owner and mode are taken from the file in front of us rather than from the
	// record. A rollback run from a saved plan has no owner in it — those fields
	// are absolute-machine detail that the plan deliberately does not serialise —
	// and the file on disk is in any case the authority for who owns it.
	// statMetadataFile has already proven it is a regular file this user owns.
	info, uid, gid, err := statMetadataFile(application.Path)
	if err != nil {
		return "", err
	}
	mode := info.Mode().Perm()
	switch digestOf(current) {
	case application.BeforeSHA256:
		return RestoreAlreadyBefore, nil
	case application.AfterSHA256:
		if err := writeFileAtomically(
			application.Path, backup, mode, uid, gid,
		); err != nil {
			return "", err
		}
		restored, err := os.ReadFile(application.Path)
		if err != nil {
			return "", fmt.Errorf("verify %s: %w", application.Path, err)
		}
		if digestOf(restored) != application.BeforeSHA256 {
			return "", fmt.Errorf(
				"%s did not return to its pre-migration digest",
				application.Path,
			)
		}
		return RestoreExactBytes, nil
	}

	// The document is neither shape. The only case this migration will accept is
	// the one it has actually observed: Apple Container re-serialising the file
	// with the same values in a different key order. Proving that means
	// reconstructing what the migration wrote and comparing everything except
	// the labels, by value.
	rewritten, err := rewriteLabels(
		backup, application.LabelPath, application.AfterLabels,
	)
	if err != nil {
		return "", fmt.Errorf("%s: %w", application.Path, err)
	}
	same, err := equalExceptLabels(current, rewritten, application.LabelPath)
	if err != nil {
		return "", fmt.Errorf("%s: %w", application.Path, err)
	}
	if !same {
		return "", fmt.Errorf("%s: %w", application.Path, ErrRollbackUnknownState)
	}
	// The labels themselves must still be the ones the migration wrote,
	// otherwise something other than a re-serialisation has edited them.
	currentLabels, err := readLabelsAtPath(current, application.LabelPath)
	if err != nil {
		return "", fmt.Errorf("%s: %w", application.Path, err)
	}
	if !sameLabels(currentLabels, application.AfterLabels) {
		return "", fmt.Errorf("%s: %w", application.Path, ErrRollbackUnknownState)
	}
	// The forward path never writes a document it cannot reproduce byte for
	// byte, and the recovery path must not be the weaker one. equalExceptLabels
	// above is deliberately insensitive to key order and whitespace, so without
	// this guard a document that was not compact would come back reformatted
	// outside its labels field — exactly what the guard exists to prevent.
	if err := requireByteStableRoundTrip(
		current, application.LabelPath,
	); err != nil {
		return "", fmt.Errorf("%s: %w", application.Path, err)
	}
	relabelled, err := rewriteLabels(
		current, application.LabelPath, application.BeforeLabels,
	)
	if err != nil {
		return "", fmt.Errorf("%s: %w", application.Path, err)
	}
	if err := writeFileAtomically(
		application.Path, relabelled, mode, uid, gid,
	); err != nil {
		return "", err
	}
	// Read the file back rather than the buffer that was just written to it.
	// readLabelsAtPath(rewriteLabels(x, path, L), path) returns L by
	// construction, so verifying the buffer verifies nothing.
	written, err := os.ReadFile(application.Path)
	if err != nil {
		return "", fmt.Errorf("verify %s: %w", application.Path, err)
	}
	verified, err := readLabelsAtPath(written, application.LabelPath)
	if err != nil {
		return "", fmt.Errorf("verify %s: %w", application.Path, err)
	}
	if !sameLabels(verified, application.BeforeLabels) {
		return "", fmt.Errorf(
			"%s did not return to its pre-migration labels", application.Path,
		)
	}
	return RestoreRelabelled, nil
}

// equalExceptLabels compares two documents by value, ignoring the labels field.
// Key order is irrelevant here on purpose: this is the comparison that decides
// whether a document differs only because the runtime re-serialised it.
func equalExceptLabels(first, second []byte, path []string) (bool, error) {
	strip := func(document []byte) (any, error) {
		var decoded any
		decoder := json.NewDecoder(bytes.NewReader(document))
		decoder.UseNumber()
		if err := decoder.Decode(&decoded); err != nil {
			return nil, fmt.Errorf("parse document: %w", err)
		}
		cursor, ok := decoded.(map[string]any)
		if !ok {
			return nil, errors.New("document is not a JSON object")
		}
		for _, key := range path[:len(path)-1] {
			next, present := cursor[key].(map[string]any)
			if !present {
				return nil, fmt.Errorf("document has no %q object", key)
			}
			cursor = next
		}
		delete(cursor, path[len(path)-1])
		return decoded, nil
	}
	firstStripped, err := strip(first)
	if err != nil {
		return false, err
	}
	secondStripped, err := strip(second)
	if err != nil {
		return false, err
	}
	return reflect.DeepEqual(firstStripped, secondStripped), nil
}
