package lifecycle

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// A backup taken by this migration is a verbatim copy of an Apple Container
// metadata document, and a container's config.json carries
// initProcess.environment with values — POSTGRES_PASSWORD among them on this
// project's own topology. A backup directory is therefore a credential store,
// not an artifact, and the two rules below follow from that:
//
//   - it lives outside every git work tree, so a copy of a live secret can
//     never be staged, committed, or pushed by an operator who ran the sweep
//     from inside a checkout;
//   - it is created and kept at 0700, so it is not readable by other accounts
//     on a shared machine.
//
// The durable evidence that does get committed is derived from these documents
// but never contains them: it carries identities, ownership classes, the six
// ownership label keys, and digests, and records every path relative to the
// application root or the backup root.

// defaultBackupRootEnv lets an operator place the private backup root
// explicitly, which is what a machine with an encrypted volume for this kind of
// material wants.
const defaultBackupRootEnv = "AGENTOPS_LABEL_BACKUP_ROOT"

// ResolveBackupRoot returns a private directory the migration may write backups
// into, creating it at 0700 when it does not exist. An explicit path is
// honoured but still verified: the point of the check is the property, not the
// default.
func ResolveBackupRoot(explicit string) (string, error) {
	root := strings.TrimSpace(explicit)
	if root == "" {
		root = strings.TrimSpace(os.Getenv(defaultBackupRootEnv))
	}
	if root == "" {
		state := strings.TrimSpace(os.Getenv("XDG_STATE_HOME"))
		if state == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf(
					"resolve a private backup root: %w; pass --backup-dir or "+
						"set %s", err, defaultBackupRootEnv,
				)
			}
			state = filepath.Join(home, ".local", "state")
		}
		root = filepath.Join(state, "agentops", "label-metadata-backups")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve backup root %s: %w", root, err)
	}
	if err := requireOutsideGitWorkTree(absolute); err != nil {
		return "", err
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return "", fmt.Errorf("create backup root %s: %w", absolute, err)
	}
	// MkdirAll leaves an existing directory's mode alone, so a root that was
	// created wider earlier has to be narrowed rather than trusted.
	if err := os.Chmod(absolute, 0o700); err != nil {
		return "", fmt.Errorf("restrict backup root %s: %w", absolute, err)
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", fmt.Errorf("inspect backup root %s: %w", absolute, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%s is a symbolic link", absolute)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", absolute)
	}
	if info.Mode().Perm() != 0o700 {
		return "", fmt.Errorf(
			"%s is mode %v; a directory holding copies of container "+
				"configuration must be 0700",
			absolute, info.Mode().Perm(),
		)
	}
	return absolute, nil
}

// requireOutsideGitWorkTree refuses a path inside a repository checkout. It
// walks the ancestry looking for a `.git` entry rather than shelling out, so it
// gives the same answer with or without git installed, and it catches the
// worktree case where `.git` is a file rather than a directory.
func requireOutsideGitWorkTree(path string) error {
	current := path
	for {
		if _, err := os.Lstat(filepath.Join(current, ".git")); err == nil {
			return fmt.Errorf(
				"%s is inside the git work tree at %s; backups are verbatim "+
					"copies of container configuration, which carries "+
					"environment values, and must never sit where they can be "+
					"committed",
				path, current,
			)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
		current = parent
	}
}

// relativeTo renders a path underneath a root for durable evidence. A path that
// escapes the root is reduced to its base name rather than written out: the
// evidence is committed, and an absolute path on the operator's machine is not
// something a pull request should carry.
func relativeTo(root, path string) string {
	if root == "" {
		return filepath.Base(path)
	}
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return filepath.Base(path)
	}
	return relative
}
