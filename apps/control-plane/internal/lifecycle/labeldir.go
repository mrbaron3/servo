package lifecycle

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// Every read, stat, create, and rename this migration performs goes through one
// open directory descriptor rather than through a pathname.
//
// Checking a path and then acting on the same path is two lookups, and between
// them a process running as the same user can replace the file or any parent
// directory. The migration would then verify one inode's bytes, ownership, and
// mode, and rename over a different one. Binding the operations to a descriptor
// makes the directory a fixed object for the run's duration: a swap of the
// directory afterwards cannot redirect the write, because the write never
// consults the name again.
//
// One window remains and is not closable on this platform: between the final
// digest check and the rename, the destination name inside the validated
// directory could still be replaced. Darwin exposes no rename that fails if the
// destination changed. The migration narrows it — the checks and the rename are
// adjacent, the runtime is stopped, and the directory is one the current user
// alone can write — and does not claim to have closed it.

// directoryHandle is an open, validated directory that file operations are
// performed relative to.
type directoryHandle struct {
	path string
	file *os.File
}

// openDirectory opens a directory without following a final symlink and proves
// it is a directory the current user owns.
func openDirectory(path string) (*directoryHandle, error) {
	// O_NOFOLLOW is the enforcement, and it reports a symlinked directory as
	// ENOTDIR. That is correct but tells an operator nothing, so the link is
	// named first. The lstat is for the message only and is never relied on by
	// itself: it is a separate lookup and could race, which is exactly why the
	// open below still refuses to follow.
	if info, err := os.Lstat(path); err == nil &&
		info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is a symbolic link", path)
	}
	descriptor, err := unix.Open(
		path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0,
	)
	if err != nil {
		return nil, fmt.Errorf("open directory %s: %w", path, err)
	}
	file := os.NewFile(uintptr(descriptor), path)
	var status unix.Stat_t
	if err := unix.Fstat(descriptor, &status); err != nil {
		file.Close()
		return nil, fmt.Errorf("inspect directory %s: %w", path, err)
	}
	if int(status.Uid) != os.Getuid() {
		file.Close()
		return nil, fmt.Errorf(
			"%s is owned by uid %d, not the current user %d",
			path, status.Uid, os.Getuid(),
		)
	}
	// A directory other users can write is one where the destination name can be
	// replaced between the final check and the rename.
	if status.Mode&0o022 != 0 {
		file.Close()
		return nil, fmt.Errorf(
			"%s is group or world writable (%04o)", path, status.Mode&0o7777,
		)
	}
	return &directoryHandle{path: path, file: file}, nil
}

func (directory *directoryHandle) Close() error {
	return directory.file.Close()
}

func (directory *directoryHandle) fd() int {
	return int(directory.file.Fd())
}

// fileIdentity names one inode. Comparing it across two opens is what proves
// the same file is still behind a name.
type fileIdentity struct {
	Device uint64
	Inode  uint64
}

// openFile opens one entry in the directory without following a symlink.
func (directory *directoryHandle) openFile(name string) (*os.File, error) {
	descriptor, err := unix.Openat(
		directory.fd(), name,
		unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0,
	)
	if err != nil {
		return nil, fmt.Errorf("open %s/%s: %w", directory.path, name, err)
	}
	return os.NewFile(uintptr(descriptor), directory.path+"/"+name), nil
}

// statDescriptor reads the properties this migration verifies, from an open
// descriptor rather than from a name.
func statDescriptor(
	file *os.File,
	label string,
) (os.FileMode, int, int, fileIdentity, error) {
	var status unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &status); err != nil {
		return 0, 0, 0, fileIdentity{}, fmt.Errorf(
			"inspect %s: %w", label, err,
		)
	}
	mode := os.FileMode(status.Mode & 0o7777)
	if status.Mode&unix.S_IFMT != unix.S_IFREG {
		return 0, 0, 0, fileIdentity{}, fmt.Errorf(
			"%s is not a regular file", label,
		)
	}
	if status.Mode&0o022 != 0 {
		return 0, 0, 0, fileIdentity{}, fmt.Errorf(
			"%s is group or world writable (%v)", label, mode,
		)
	}
	identity := fileIdentity{
		Device: uint64(status.Dev), Inode: uint64(status.Ino),
	}
	uid, gid := int(status.Uid), int(status.Gid)
	if err := requireOwnedByCurrentUser(label, uid, gid); err != nil {
		return 0, 0, 0, fileIdentity{}, err
	}
	return mode, uid, gid, identity, nil
}

// createTemporary creates a fresh file in the directory. O_EXCL means a name a
// previous run left behind is a failure rather than something to write over.
func (directory *directoryHandle) createTemporary(
	name string,
	mode os.FileMode,
) (*os.File, error) {
	descriptor, err := unix.Openat(
		directory.fd(), name,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC,
		uint32(mode.Perm()),
	)
	if err != nil {
		return nil, fmt.Errorf(
			"create %s/%s: %w", directory.path, name, err,
		)
	}
	return os.NewFile(uintptr(descriptor), directory.path+"/"+name), nil
}

// rename moves one entry onto another inside this directory. Both names are
// resolved relative to the descriptor, so neither can be redirected by a
// directory swap after the handle was opened and validated.
func (directory *directoryHandle) rename(from, to string) error {
	if err := unix.Renameat(
		directory.fd(), from, directory.fd(), to,
	); err != nil {
		return fmt.Errorf(
			"replace %s/%s: %w", directory.path, to, err,
		)
	}
	return nil
}

func (directory *directoryHandle) remove(name string) {
	_ = unix.Unlinkat(directory.fd(), name, 0)
}

// sync flushes the directory entry itself, without which a rename can still be
// lost to a crash even after the file's contents were flushed.
func (directory *directoryHandle) sync() error {
	if err := directory.file.Sync(); err != nil {
		return fmt.Errorf("flush directory %s: %w", directory.path, err)
	}
	return nil
}
