package lifecycle

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
	Labels map[string]string
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
	Document     string            `json:"document"`
	Backup       string            `json:"backup"`
	BeforeSHA256 string            `json:"beforeSha256"`
	AfterSHA256  string            `json:"afterSha256"`
	BackupSHA256 string            `json:"backupSha256"`
	Mode         os.FileMode       `json:"mode"`
	BeforeLabels map[string]string `json:"beforeLabels"`
	AfterLabels  map[string]string `json:"afterLabels"`
	// RestoredAs records how a rollback returned this document, when one ran.
	RestoredAs RestoreOutcome `json:"restoredAs,omitempty"`
}

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// requireNoSymlinkInPath proves that no component between a trusted root and a
// target is a symbolic link. Checking the final file alone is not enough: a
// symlinked parent directory presents a perfectly ordinary regular file while
// the write lands somewhere the operator never named.
func requireNoSymlinkInPath(path, root string) error {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return fmt.Errorf("resolve %s under %s: %w", path, root, err)
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("%s is outside the expected root", path)
	}
	current := root
	for _, component := range strings.Split(relative, string(os.PathSeparator)) {
		if component == "." {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("inspect %s: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symbolic link", current)
		}
	}
	return nil
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
func inspectMetadataFile(ref metadataFileRef) (*metadataFileState, error) {
	info, uid, gid, err := statMetadataFile(ref.Path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(ref.Path)
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
		Ref:    ref,
		Bytes:  data,
		SHA256: digestOf(data),
		Mode:   info.Mode(),
		UID:    uid,
		GID:    gid,
		Labels: labels,
	}, nil
}

// applyMetadataFile rewrites one document's labels. The order is deliberate:
// the document is re-verified against the digest recorded at inspection, the
// backup is taken and durably flushed, and only then is the replacement written
// through a same-directory temporary file and an atomic rename.
func applyMetadataFile(
	state *metadataFileState,
	labels map[string]string,
	backupPath string,
) (*metadataFileApplication, error) {
	// Between planning and applying, anything could have touched the file.
	// Re-reading and comparing digests is what makes the plan an accurate
	// description of what is about to happen.
	current, err := os.ReadFile(state.Ref.Path)
	if err != nil {
		return nil, fmt.Errorf("re-read %s: %w", state.Ref.Path, err)
	}
	if digestOf(current) != state.SHA256 {
		return nil, fmt.Errorf(
			"%s changed since it was inspected; re-run the plan",
			state.Ref.Path,
		)
	}
	if _, _, _, err := statMetadataFile(state.Ref.Path); err != nil {
		return nil, err
	}
	updated, err := rewriteLabels(current, state.Ref.LabelPath, labels)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", state.Ref.Path, err)
	}

	// The backup is written first and with O_EXCL. Evidence of an irreversible
	// act must never silently replace an earlier run's account of it, and a
	// migration that cannot record what it is about to overwrite does not start.
	if err := os.MkdirAll(filepath.Dir(backupPath), 0o700); err != nil {
		return nil, fmt.Errorf("create backup directory: %w", err)
	}
	backup, err := os.OpenFile(
		backupPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600,
	)
	if err != nil {
		return nil, fmt.Errorf("open backup %s: %w", backupPath, err)
	}
	if _, err := backup.Write(current); err != nil {
		backup.Close()
		return nil, fmt.Errorf("write backup %s: %w", backupPath, err)
	}
	if err := backup.Sync(); err != nil {
		backup.Close()
		return nil, fmt.Errorf("flush backup %s: %w", backupPath, err)
	}
	if err := backup.Close(); err != nil {
		return nil, fmt.Errorf("close backup %s: %w", backupPath, err)
	}
	if err := syncDirectory(filepath.Dir(backupPath)); err != nil {
		return nil, err
	}

	if err := writeFileAtomically(
		state.Ref.Path, updated, state.Mode.Perm(), state.UID, state.GID,
	); err != nil {
		return nil, err
	}
	return &metadataFileApplication{
		Path:         state.Ref.Path,
		LabelPath:    state.Ref.LabelPath,
		BackupPath:   backupPath,
		BeforeSHA256: state.SHA256,
		AfterSHA256:  digestOf(updated),
		BackupSHA256: digestOf(current),
		Mode:         state.Mode.Perm(),
		BeforeLabels: state.Labels,
		AfterLabels:  labels,
	}, nil
}

// writeFileAtomically replaces a file's contents without ever exposing a
// partially written document. The temporary file is created in the destination
// directory so the rename cannot cross a filesystem boundary, its mode is set
// explicitly rather than left to the umask, and both the file and the directory
// entry are flushed before the operation is considered done.
func writeFileAtomically(
	path string,
	data []byte,
	mode os.FileMode,
	uid, gid int,
) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".agentopsctl-label-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary file in %s: %w", directory, err)
	}
	temporaryPath := temporary.Name()
	cleanup := func() {
		temporary.Close()
		os.Remove(temporaryPath)
	}
	if _, err := temporary.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("write %s: %w", temporaryPath, err)
	}
	// Chmod rather than relying on CreateTemp's 0600 or on the umask: the
	// replacement has to carry the mode the original carried.
	if err := temporary.Chmod(mode); err != nil {
		cleanup()
		return fmt.Errorf("set mode on %s: %w", temporaryPath, err)
	}
	if err := matchOwnership(temporary, temporaryPath, uid, gid); err != nil {
		cleanup()
		return err
	}
	if err := temporary.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("flush %s: %w", temporaryPath, err)
	}
	if err := temporary.Close(); err != nil {
		os.Remove(temporaryPath)
		return fmt.Errorf("close %s: %w", temporaryPath, err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		os.Remove(temporaryPath)
		return fmt.Errorf("replace %s: %w", path, err)
	}
	// Without this the rename can still be lost to a crash even though the file
	// contents were flushed.
	return syncDirectory(directory)
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
	verified, err := readLabelsAtPath(relabelled, application.LabelPath)
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
