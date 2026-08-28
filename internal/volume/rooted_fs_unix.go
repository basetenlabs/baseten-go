//go:build linux || darwin

package volume

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"unicode/utf8"
)

const rootedDirectoryOpenFlags = os.O_RDONLY |
	syscall.O_CLOEXEC |
	syscall.O_DIRECTORY |
	syscall.O_NOFOLLOW |
	syscall.O_NONBLOCK

type rootedFileStat struct {
	mode     os.FileMode
	size     int64
	modified int64
	identity sourceIdentity
	uid      uint32
	nlink    uint64
}

func unixFileMode(mode uint32) os.FileMode {
	result := os.FileMode(mode & 0o777)
	switch mode & syscall.S_IFMT {
	case syscall.S_IFBLK:
		result |= os.ModeDevice
	case syscall.S_IFCHR:
		result |= os.ModeDevice | os.ModeCharDevice
	case syscall.S_IFDIR:
		result |= os.ModeDir
	case syscall.S_IFIFO:
		result |= os.ModeNamedPipe
	case syscall.S_IFLNK:
		result |= os.ModeSymlink
	case syscall.S_IFREG:
	case syscall.S_IFSOCK:
		result |= os.ModeSocket
	default:
		result |= os.ModeIrregular
	}
	if mode&syscall.S_ISUID != 0 {
		result |= os.ModeSetuid
	}
	if mode&syscall.S_ISGID != 0 {
		result |= os.ModeSetgid
	}
	if mode&syscall.S_ISVTX != 0 {
		result |= os.ModeSticky
	}
	return result
}

func rootedFileStatFromInfo(info fs.FileInfo) (rootedFileStat, error) {
	if info == nil {
		return rootedFileStat{}, syscall.EINVAL
	}
	raw, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return rootedFileStat{}, syscall.EINVAL
	}
	return rootedFileStat{
		mode:     info.Mode(),
		size:     info.Size(),
		modified: info.ModTime().UnixNano(),
		identity: sourceIdentityFor(info),
		uid:      raw.Uid,
		nlink:    uint64(raw.Nlink),
	}, nil
}

func rootedFileStatFromFile(file *os.File) (rootedFileStat, error) {
	info, err := file.Stat()
	if err != nil {
		return rootedFileStat{}, err
	}
	return rootedFileStatFromInfo(info)
}

func sameRootedObject(left, right rootedFileStat) bool {
	return left.identity.available &&
		right.identity.available &&
		left.identity.device == right.identity.device &&
		left.identity.inode == right.identity.inode
}

func sameRootedSnapshot(left, right rootedFileStat) bool {
	return sameRootedObject(left, right) &&
		left.identity == right.identity &&
		left.mode == right.mode &&
		left.size == right.size &&
		left.modified == right.modified
}

// os.Root deliberately follows in-root symlinks. Unix transfer paths require
// stronger no-follow semantics, so this uses *at calls with O_NOFOLLOW on each
// component while retaining the standard library's descriptor-rooted model.
type rootedDirectory struct {
	file     *os.File
	identity rootedFileStat
}

func openRootedDirectory(path string) (*rootedDirectory, error) {
	file, err := os.OpenFile(path, rootedDirectoryOpenFlags, 0)
	if err != nil {
		return nil, err
	}
	root, err := rootedDirectoryFromFile(file)
	if err != nil {
		file.Close()
		return nil, err
	}
	return root, nil
}

func rootedDirectoryFromFile(file *os.File) (*rootedDirectory, error) {
	stat, err := rootedFileStatFromFile(file)
	if err != nil {
		return nil, err
	}
	if !stat.mode.IsDir() {
		return nil, syscall.ENOTDIR
	}
	return &rootedDirectory{file: file, identity: stat}, nil
}

func (root *rootedDirectory) close() error {
	if root == nil || root.file == nil {
		return nil
	}
	file := root.file
	root.file = nil
	return file.Close()
}

func (root *rootedDirectory) duplicate() (*rootedDirectory, error) {
	if root == nil || root.file == nil {
		return nil, os.ErrClosed
	}
	file, err := openFileAt(root.file, ".", rootedDirectoryOpenFlags, 0)
	if err != nil {
		return nil, err
	}
	duplicate, err := rootedDirectoryFromFile(file)
	if err != nil {
		file.Close()
		return nil, err
	}
	if !sameRootedObject(root.identity, duplicate.identity) {
		duplicate.close()
		return nil, syscall.ESTALE
	}
	return duplicate, nil
}

func (root *rootedDirectory) currentStat() (rootedFileStat, error) {
	if root == nil || root.file == nil {
		return rootedFileStat{}, os.ErrClosed
	}
	return rootedFileStatFromFile(root.file)
}

func (root *rootedDirectory) sync() error {
	if root == nil || root.file == nil {
		return os.ErrClosed
	}
	return root.file.Sync()
}

func (root *rootedDirectory) chmod(mode os.FileMode) error {
	if root == nil || root.file == nil {
		return os.ErrClosed
	}
	return root.file.Chmod(mode)
}

func portableComponents(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	if err := validatePortablePathSyntax(path); err != nil {
		return nil, err
	}
	return strings.Split(path, "/"), nil
}

func (root *rootedDirectory) verifyDirectoryChain(
	path string,
	expected map[string]rootedFileStat,
) error {
	components, err := portableComponents(path)
	if err != nil {
		return err
	}
	current, err := openFileAt(root.file, ".", rootedDirectoryOpenFlags, 0)
	if err != nil {
		return err
	}
	verify := func(file *os.File, relative string) error {
		want, ok := expected[relative]
		if !ok {
			return syscall.ESTALE
		}
		got, err := rootedFileStatFromFile(file)
		if err != nil {
			return err
		}
		if !sameRootedSnapshot(want, got) {
			return syscall.ESTALE
		}
		return nil
	}
	if err := verify(current, ""); err != nil {
		current.Close()
		return err
	}
	relativeEnd := 0
	for _, component := range components {
		next, err := openFileAt(current, component, rootedDirectoryOpenFlags, 0)
		if err != nil {
			current.Close()
			return err
		}
		if relativeEnd != 0 {
			relativeEnd++
		}
		relativeEnd += len(component)
		if err := verify(next, path[:relativeEnd]); err != nil {
			next.Close()
			current.Close()
			return err
		}
		if err := current.Close(); err != nil {
			next.Close()
			return err
		}
		current = next
	}
	return current.Close()
}

func (root *rootedDirectory) openParent(
	path string,
	expected map[string]rootedFileStat,
) (*os.File, string, error) {
	components, err := portableComponents(path)
	if err != nil {
		return nil, "", err
	}
	if len(components) == 0 {
		return nil, "", syscall.EINVAL
	}
	current, err := openFileAt(root.file, ".", rootedDirectoryOpenFlags, 0)
	if err != nil {
		return nil, "", err
	}
	verify := func(file *os.File, relative string) error {
		if expected == nil {
			return nil
		}
		want, ok := expected[relative]
		if !ok {
			return syscall.ESTALE
		}
		got, err := rootedFileStatFromFile(file)
		if err != nil {
			return err
		}
		if !sameRootedObject(want, got) {
			return syscall.ESTALE
		}
		return nil
	}
	if err := verify(current, ""); err != nil {
		current.Close()
		return nil, "", err
	}
	relativeEnd := 0
	for _, component := range components[:len(components)-1] {
		next, err := openFileAt(current, component, rootedDirectoryOpenFlags, 0)
		if err != nil {
			current.Close()
			return nil, "", err
		}
		if relativeEnd != 0 {
			relativeEnd++
		}
		relativeEnd += len(component)
		if err := verify(next, path[:relativeEnd]); err != nil {
			next.Close()
			current.Close()
			return nil, "", err
		}
		if err := current.Close(); err != nil {
			next.Close()
			return nil, "", err
		}
		current = next
	}
	return current, components[len(components)-1], nil
}

func (root *rootedDirectory) openDirectory(
	path string,
	expected map[string]rootedFileStat,
) (*os.File, rootedFileStat, error) {
	if path == "" {
		file, err := openFileAt(root.file, ".", rootedDirectoryOpenFlags, 0)
		if err != nil {
			return nil, rootedFileStat{}, err
		}
		stat, err := rootedFileStatFromFile(file)
		if err != nil {
			file.Close()
			return nil, rootedFileStat{}, err
		}
		if expected != nil {
			want, ok := expected[""]
			if !ok || !sameRootedObject(want, stat) {
				file.Close()
				return nil, rootedFileStat{}, syscall.ESTALE
			}
		}
		return file, stat, nil
	}
	parent, name, err := root.openParent(path, expected)
	if err != nil {
		return nil, rootedFileStat{}, err
	}
	defer parent.Close()
	file, err := openFileAt(parent, name, rootedDirectoryOpenFlags, 0)
	if err != nil {
		return nil, rootedFileStat{}, err
	}
	stat, err := rootedFileStatFromFile(file)
	if err != nil {
		file.Close()
		return nil, rootedFileStat{}, err
	}
	if expected != nil {
		want, ok := expected[path]
		if !ok || !sameRootedObject(want, stat) {
			file.Close()
			return nil, rootedFileStat{}, syscall.ESTALE
		}
	}
	return file, stat, nil
}

func (root *rootedDirectory) openDirectoryRoot(
	path string,
	expected map[string]rootedFileStat,
) (*rootedDirectory, error) {
	file, _, err := root.openDirectory(path, expected)
	if err != nil {
		return nil, err
	}
	child, err := rootedDirectoryFromFile(file)
	if err != nil {
		file.Close()
		return nil, err
	}
	return child, nil
}

func (root *rootedDirectory) openRegular(
	path string,
	flags int,
	perm os.FileMode,
	expected map[string]rootedFileStat,
) (*os.File, rootedFileStat, error) {
	parent, name, err := root.openParent(path, expected)
	if err != nil {
		return nil, rootedFileStat{}, err
	}
	defer parent.Close()
	flags |= syscall.O_CLOEXEC | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
	file, err := openFileAt(parent, name, flags, perm)
	if err != nil {
		return nil, rootedFileStat{}, err
	}
	stat, err := rootedFileStatFromFile(file)
	if err != nil {
		file.Close()
		return nil, rootedFileStat{}, err
	}
	if !stat.mode.IsRegular() {
		file.Close()
		return nil, rootedFileStat{}, syscall.EPERM
	}
	return file, stat, nil
}

func (root *rootedDirectory) lstat(
	path string,
	expected map[string]rootedFileStat,
) (rootedFileStat, error) {
	if path == "" {
		return root.currentStat()
	}
	parent, name, err := root.openParent(path, expected)
	if err != nil {
		return rootedFileStat{}, err
	}
	defer parent.Close()
	return statAt(parent, name)
}

func (root *rootedDirectory) readlink(
	path string,
	expected map[string]rootedFileStat,
) (string, error) {
	parent, name, err := root.openParent(path, expected)
	if err != nil {
		return "", err
	}
	defer parent.Close()
	return readlinkAt(parent, name)
}

func (root *rootedDirectory) mkdir(
	path string,
	perm os.FileMode,
	expected map[string]rootedFileStat,
) error {
	parent, name, err := root.openParent(path, expected)
	if err != nil {
		return err
	}
	defer parent.Close()
	return mkdirAt(parent, name, perm)
}

func (root *rootedDirectory) symlink(
	target string,
	path string,
	expected map[string]rootedFileStat,
) error {
	parent, name, err := root.openParent(path, expected)
	if err != nil {
		return err
	}
	defer parent.Close()
	return symlinkAt(target, parent, name)
}

func (root *rootedDirectory) remove(
	path string,
	directory bool,
	expected map[string]rootedFileStat,
) error {
	parent, name, err := root.openParent(path, expected)
	if err != nil {
		return err
	}
	defer parent.Close()
	return unlinkAt(parent, name, directory)
}

func (root *rootedDirectory) chmodNoFollow(
	path string,
	mode os.FileMode,
	expected map[string]rootedFileStat,
	identity rootedFileStat,
) (rootedFileStat, error) {
	parent, name, err := root.openParent(path, expected)
	if err != nil {
		return rootedFileStat{}, err
	}
	defer parent.Close()
	before, err := statAt(parent, name)
	if err != nil ||
		!sameRootedObject(identity, before) ||
		!rootedOwnedByCurrentUser(before) {
		if err != nil {
			return rootedFileStat{}, err
		}
		return rootedFileStat{}, syscall.ESTALE
	}
	if before.mode.Perm() == mode.Perm() {
		return before, nil
	}
	if err := chmodAtNoFollow(parent, name, mode, before); err != nil {
		return rootedFileStat{}, err
	}
	after, err := statAt(parent, name)
	if err != nil ||
		!sameRootedObject(before, after) ||
		after.mode.Perm() != mode.Perm() {
		if err != nil {
			return rootedFileStat{}, err
		}
		return rootedFileStat{}, syscall.ESTALE
	}
	return after, nil
}

func (root *rootedDirectory) rename(
	oldPath string,
	newRoot *rootedDirectory,
	newPath string,
	noReplace bool,
) error {
	oldParent, oldName, err := root.openParent(oldPath, nil)
	if err != nil {
		return err
	}
	defer oldParent.Close()
	newParent, newName, err := newRoot.openParent(newPath, nil)
	if err != nil {
		return err
	}
	defer newParent.Close()
	return renameAt(oldParent, oldName, newParent, newName, noReplace)
}

func (root *rootedDirectory) exchange(
	oldPath string,
	newRoot *rootedDirectory,
	newPath string,
) error {
	oldParent, oldName, err := root.openParent(oldPath, nil)
	if err != nil {
		return err
	}
	defer oldParent.Close()
	newParent, newName, err := newRoot.openParent(newPath, nil)
	if err != nil {
		return err
	}
	defer newParent.Close()
	return exchangeAt(oldParent, oldName, newParent, newName)
}

func (root *rootedDirectory) readDirNames(
	path string,
	expected map[string]rootedFileStat,
	maxEntries int,
) ([]string, error) {
	directory, _, err := root.openDirectory(path, expected)
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	var names []string
	for {
		entries, err := directory.ReadDir(128)
		for _, entry := range entries {
			name := entry.Name()
			if name == "." || name == ".." || !utf8.ValidString(name) ||
				strings.ContainsRune(name, filepath.Separator) {
				return nil, syscall.EINVAL
			}
			names = append(names, name)
			if maxEntries >= 0 && len(names) > maxEntries {
				return nil, syscall.EFBIG
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(names)
	return names, nil
}

func (root *rootedDirectory) readDirNamesAtMost(
	path string,
	expected map[string]rootedFileStat,
	maxEntries int,
) ([]string, error) {
	directory, _, err := root.openDirectory(path, expected)
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	entries, err := directory.ReadDir(maxEntries)
	if errors.Is(err, io.EOF) {
		err = nil
	}
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if name == "." || name == ".." || !utf8.ValidString(name) ||
			strings.ContainsRune(name, filepath.Separator) {
			return nil, syscall.EINVAL
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func (root *rootedDirectory) entryMatches(path string, identity rootedFileStat) bool {
	stat, err := root.lstat(path, nil)
	return err == nil && sameRootedObject(stat, identity)
}

func (root *rootedDirectory) removeTree(
	path string,
	identity rootedFileStat,
	maxEntries int,
) error {
	parent, name, err := root.openParent(path, nil)
	if err != nil {
		return err
	}
	defer parent.Close()
	current, err := openFileAt(parent, name, rootedDirectoryOpenFlags, 0)
	if err != nil {
		return err
	}
	currentStat, err := rootedFileStatFromFile(current)
	if err != nil {
		current.Close()
		return err
	}
	if !sameRootedObject(identity, currentStat) {
		current.Close()
		return syscall.ESTALE
	}
	remaining := maxEntries
	if err := removeDirectoryContents(current, &remaining); err != nil {
		current.Close()
		return err
	}
	if err := current.Close(); err != nil {
		return err
	}
	after, err := statAt(parent, name)
	if err != nil {
		return err
	}
	if !sameRootedObject(identity, after) {
		return syscall.ESTALE
	}
	return unlinkAt(parent, name, true)
}

func removeDirectoryContents(directory *os.File, remaining *int) error {
	for {
		entries, err := directory.ReadDir(128)
		for _, entry := range entries {
			if *remaining == 0 {
				return syscall.EFBIG
			}
			if *remaining > 0 {
				*remaining = *remaining - 1
			}
			name := entry.Name()
			before, statErr := statAt(directory, name)
			if statErr != nil {
				return statErr
			}
			if before.mode.IsDir() {
				if !rootedOwnedByCurrentUser(before) {
					return syscall.EPERM
				}
				before, statErr = chmodDirectoryForRemoval(directory, name, before)
				if statErr != nil {
					return statErr
				}
				child, openErr := openFileAt(directory, name, rootedDirectoryOpenFlags, 0)
				if openErr != nil {
					return openErr
				}
				opened, statErr := rootedFileStatFromFile(child)
				if statErr != nil || !sameRootedObject(before, opened) {
					child.Close()
					if statErr != nil {
						return statErr
					}
					return syscall.ESTALE
				}
				if removeErr := removeDirectoryContents(child, remaining); removeErr != nil {
					child.Close()
					return removeErr
				}
				if closeErr := child.Close(); closeErr != nil {
					return closeErr
				}
				after, statErr := statAt(directory, name)
				if statErr != nil || !sameRootedObject(before, after) {
					if statErr != nil {
						return statErr
					}
					return syscall.ESTALE
				}
				if removeErr := unlinkAt(directory, name, true); removeErr != nil {
					return removeErr
				}
				continue
			}
			after, statErr := statAt(directory, name)
			if statErr != nil || !sameRootedObject(before, after) {
				if statErr != nil {
					return statErr
				}
				return syscall.ESTALE
			}
			if removeErr := unlinkAt(directory, name, false); removeErr != nil {
				return removeErr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func chmodDirectoryForRemoval(
	parent *os.File,
	name string,
	before rootedFileStat,
) (rootedFileStat, error) {
	if before.mode.Perm() == 0o700 {
		return before, nil
	}
	if err := chmodAtNoFollow(parent, name, 0o700, before); err != nil {
		return rootedFileStat{}, err
	}
	after, err := statAt(parent, name)
	if err != nil || !sameRootedObject(before, after) || !after.mode.IsDir() {
		if err != nil {
			return rootedFileStat{}, err
		}
		return rootedFileStat{}, syscall.ESTALE
	}
	return after, nil
}

func chmodOpenedAtNoFollow(
	parent *os.File,
	name string,
	mode os.FileMode,
	identity rootedFileStat,
) error {
	flags := os.O_RDONLY | syscall.O_CLOEXEC | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
	if identity.mode.IsDir() {
		flags |= syscall.O_DIRECTORY
	} else if !identity.mode.IsRegular() {
		return syscall.EPERM
	}
	file, err := openFileAt(parent, name, flags, 0)
	if err != nil {
		return err
	}
	opened, err := rootedFileStatFromFile(file)
	if err != nil ||
		!sameRootedSnapshot(identity, opened) ||
		!rootedOwnedByCurrentUser(opened) {
		file.Close()
		if err != nil {
			return err
		}
		return syscall.ESTALE
	}
	if err := file.Chmod(mode); err != nil {
		file.Close()
		return err
	}
	after, err := rootedFileStatFromFile(file)
	closeErr := file.Close()
	if err != nil ||
		closeErr != nil ||
		!sameRootedObject(opened, after) ||
		after.mode.Perm() != mode.Perm() {
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		return syscall.ESTALE
	}
	return nil
}

func validateOwnedPrivateDirectory(stat rootedFileStat) error {
	if !stat.mode.IsDir() ||
		!rootedOwnedByCurrentUser(stat) ||
		stat.mode.Perm() != 0o700 {
		return syscall.EPERM
	}
	return nil
}

func validateOwnedRegular(stat rootedFileStat, mode os.FileMode) error {
	if !stat.mode.IsRegular() ||
		!rootedOwnedByCurrentUser(stat) ||
		stat.nlink != 1 ||
		stat.mode.Perm() != mode.Perm() {
		return syscall.EPERM
	}
	return nil
}

func rootedOwnedByCurrentUser(stat rootedFileStat) bool {
	return stat.uid == uint32(os.Geteuid())
}

func isNotExist(err error) bool {
	return errors.Is(err, fs.ErrNotExist)
}
