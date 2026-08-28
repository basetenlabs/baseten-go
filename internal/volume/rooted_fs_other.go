//go:build !linux && !darwin

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

type rootedFileStat struct {
	mode     os.FileMode
	size     int64
	modified int64
	identity sourceIdentity
	info     fs.FileInfo
	uid      uint32
	nlink    uint64
}

func rootedFileStatFromInfo(info fs.FileInfo) (rootedFileStat, error) {
	if info == nil {
		return rootedFileStat{}, syscall.EINVAL
	}
	return rootedFileStat{
		mode:     info.Mode(),
		size:     info.Size(),
		modified: info.ModTime().UnixNano(),
		identity: sourceIdentityFor(info),
		info:     info,
		nlink:    1,
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
	return left.info != nil && right.info != nil && os.SameFile(left.info, right.info)
}

func sameRootedSnapshot(left, right rootedFileStat) bool {
	return sameRootedObject(left, right) &&
		left.mode == right.mode &&
		left.size == right.size &&
		left.modified == right.modified
}

type rootedDirectory struct {
	root     *os.Root
	identity rootedFileStat
}

func openRootedDirectory(path string) (*rootedDirectory, error) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		if err != nil {
			return nil, err
		}
		return nil, syscall.ENOTDIR
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	opened, err := root.Stat(".")
	if err != nil {
		root.Close()
		return nil, err
	}
	before, err := rootedFileStatFromInfo(info)
	if err != nil {
		root.Close()
		return nil, err
	}
	after, err := rootedFileStatFromInfo(opened)
	if err != nil || !sameRootedObject(before, after) {
		root.Close()
		if err != nil {
			return nil, err
		}
		return nil, syscall.ESTALE
	}
	return &rootedDirectory{root: root, identity: after}, nil
}

func (root *rootedDirectory) close() error {
	if root == nil || root.root == nil {
		return nil
	}
	opened := root.root
	root.root = nil
	return opened.Close()
}

func (root *rootedDirectory) duplicate() (*rootedDirectory, error) {
	return nil, errAtomicPullUnsupported
}

func (root *rootedDirectory) currentStat() (rootedFileStat, error) {
	if root == nil || root.root == nil {
		return rootedFileStat{}, os.ErrClosed
	}
	info, err := root.root.Stat(".")
	if err != nil {
		return rootedFileStat{}, err
	}
	return rootedFileStatFromInfo(info)
}

func (root *rootedDirectory) sync() error {
	return errAtomicPullUnsupported
}

func (root *rootedDirectory) chmod(os.FileMode) error {
	return errAtomicPullUnsupported
}

func portableNativePath(path string) (string, error) {
	if path == "" {
		return ".", nil
	}
	if err := validatePortablePathSyntax(path); err != nil {
		return "", err
	}
	return filepath.FromSlash(path), nil
}

func (root *rootedDirectory) verifyDirectoryChain(
	path string,
	expected map[string]rootedFileStat,
) error {
	if path != "" {
		if err := validatePortablePathSyntax(path); err != nil {
			return err
		}
	}
	wantRoot, ok := expected[""]
	if !ok {
		return syscall.ESTALE
	}
	gotRoot, err := root.currentStat()
	if err != nil || !sameRootedSnapshot(wantRoot, gotRoot) {
		if err != nil {
			return err
		}
		return syscall.ESTALE
	}
	if path == "" {
		return nil
	}
	relativeEnd := 0
	for _, component := range strings.Split(path, "/") {
		if relativeEnd != 0 {
			relativeEnd++
		}
		relativeEnd += len(component)
		relative := path[:relativeEnd]
		want, ok := expected[relative]
		if !ok {
			return syscall.ESTALE
		}
		info, err := root.root.Lstat(filepath.FromSlash(relative))
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			if err != nil {
				return err
			}
			return syscall.ESTALE
		}
		got, err := rootedFileStatFromInfo(info)
		if err != nil || !sameRootedSnapshot(want, got) {
			if err != nil {
				return err
			}
			return syscall.ESTALE
		}
	}
	return nil
}

func (root *rootedDirectory) verifyParents(
	path string,
	expected map[string]rootedFileStat,
) error {
	if expected == nil {
		return nil
	}
	wantRoot, ok := expected[""]
	if !ok {
		return syscall.ESTALE
	}
	gotRoot, err := root.currentStat()
	if err != nil || !sameRootedObject(wantRoot, gotRoot) {
		if err != nil {
			return err
		}
		return syscall.ESTALE
	}
	components := strings.Split(path, "/")
	var relative string
	for _, component := range components[:max(len(components)-1, 0)] {
		if relative == "" {
			relative = component
		} else {
			relative += "/" + component
		}
		want, ok := expected[relative]
		if !ok {
			return syscall.ESTALE
		}
		info, err := root.root.Lstat(filepath.FromSlash(relative))
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			if err != nil {
				return err
			}
			return syscall.ESTALE
		}
		got, err := rootedFileStatFromInfo(info)
		if err != nil || !sameRootedObject(want, got) {
			if err != nil {
				return err
			}
			return syscall.ESTALE
		}
	}
	return nil
}

func (root *rootedDirectory) lstat(
	path string,
	expected map[string]rootedFileStat,
) (rootedFileStat, error) {
	native, err := portableNativePath(path)
	if err != nil {
		return rootedFileStat{}, err
	}
	if err := root.verifyParents(path, expected); err != nil {
		return rootedFileStat{}, err
	}
	info, err := root.root.Lstat(native)
	if err != nil {
		return rootedFileStat{}, err
	}
	return rootedFileStatFromInfo(info)
}

func (root *rootedDirectory) openRegular(
	path string,
	flags int,
	perm os.FileMode,
	expected map[string]rootedFileStat,
) (*os.File, rootedFileStat, error) {
	native, err := portableNativePath(path)
	if err != nil {
		return nil, rootedFileStat{}, err
	}
	if err := root.verifyParents(path, expected); err != nil {
		return nil, rootedFileStat{}, err
	}
	before, beforeErr := root.root.Lstat(native)
	file, err := root.root.OpenFile(native, flags, perm)
	if err != nil {
		return nil, rootedFileStat{}, err
	}
	stat, err := rootedFileStatFromFile(file)
	if err != nil || !stat.mode.IsRegular() {
		file.Close()
		if err != nil {
			return nil, rootedFileStat{}, err
		}
		return nil, rootedFileStat{}, syscall.EPERM
	}
	if beforeErr == nil {
		beforeStat, statErr := rootedFileStatFromInfo(before)
		if statErr != nil || before.Mode()&os.ModeSymlink != 0 ||
			!sameRootedObject(beforeStat, stat) {
			file.Close()
			return nil, rootedFileStat{}, syscall.ESTALE
		}
	}
	return file, stat, nil
}

func (root *rootedDirectory) openDirectory(
	path string,
	expected map[string]rootedFileStat,
) (*os.File, rootedFileStat, error) {
	native, err := portableNativePath(path)
	if err != nil {
		return nil, rootedFileStat{}, err
	}
	if err := root.verifyParents(path, expected); err != nil {
		return nil, rootedFileStat{}, err
	}
	before, err := root.root.Lstat(native)
	if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
		if err != nil {
			return nil, rootedFileStat{}, err
		}
		return nil, rootedFileStat{}, syscall.ENOTDIR
	}
	file, err := root.root.Open(native)
	if err != nil {
		return nil, rootedFileStat{}, err
	}
	opened, err := rootedFileStatFromFile(file)
	beforeStat, statErr := rootedFileStatFromInfo(before)
	if err != nil || statErr != nil || !sameRootedObject(beforeStat, opened) {
		file.Close()
		if err != nil {
			return nil, rootedFileStat{}, err
		}
		if statErr != nil {
			return nil, rootedFileStat{}, statErr
		}
		return nil, rootedFileStat{}, syscall.ESTALE
	}
	if expected != nil {
		want, ok := expected[path]
		if !ok || !sameRootedObject(want, opened) {
			file.Close()
			return nil, rootedFileStat{}, syscall.ESTALE
		}
	}
	return file, opened, nil
}

func (root *rootedDirectory) openDirectoryRoot(
	string,
	map[string]rootedFileStat,
) (*rootedDirectory, error) {
	return nil, errAtomicPullUnsupported
}

func (root *rootedDirectory) readlink(
	path string,
	expected map[string]rootedFileStat,
) (string, error) {
	native, err := portableNativePath(path)
	if err != nil {
		return "", err
	}
	if err := root.verifyParents(path, expected); err != nil {
		return "", err
	}
	return root.root.Readlink(native)
}

func (root *rootedDirectory) readDirNames(
	path string,
	expected map[string]rootedFileStat,
	maxEntries int,
) ([]string, error) {
	file, _, err := root.openDirectory(path, expected)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var names []string
	for {
		entries, err := file.ReadDir(128)
		for _, entry := range entries {
			name := entry.Name()
			if name == "." || name == ".." || !utf8.ValidString(name) {
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
	file, _, err := root.openDirectory(path, expected)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	entries, err := file.ReadDir(maxEntries)
	if errors.Is(err, io.EOF) {
		err = nil
	}
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if name == "." || name == ".." || !utf8.ValidString(name) {
			return nil, syscall.EINVAL
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func (root *rootedDirectory) mkdir(string, os.FileMode, map[string]rootedFileStat) error {
	return errAtomicPullUnsupported
}

func (root *rootedDirectory) symlink(string, string, map[string]rootedFileStat) error {
	return errAtomicPullUnsupported
}

func (root *rootedDirectory) remove(string, bool, map[string]rootedFileStat) error {
	return errAtomicPullUnsupported
}

func (root *rootedDirectory) chmodNoFollow(
	string,
	os.FileMode,
	map[string]rootedFileStat,
	rootedFileStat,
) (rootedFileStat, error) {
	return rootedFileStat{}, errAtomicPullUnsupported
}

func (root *rootedDirectory) rename(string, *rootedDirectory, string, bool) error {
	return errAtomicPullUnsupported
}

func (root *rootedDirectory) exchange(string, *rootedDirectory, string) error {
	return errAtomicPullUnsupported
}

func (root *rootedDirectory) entryMatches(path string, identity rootedFileStat) bool {
	stat, err := root.lstat(path, nil)
	return err == nil && sameRootedObject(stat, identity)
}

func (root *rootedDirectory) removeTree(string, rootedFileStat, int) error {
	return errAtomicPullUnsupported
}

func validateOwnedPrivateDirectory(rootedFileStat) error {
	return errAtomicPullUnsupported
}

func validateOwnedRegular(rootedFileStat, os.FileMode) error {
	return errAtomicPullUnsupported
}

func rootedOwnedByCurrentUser(rootedFileStat) bool {
	return true
}

func isNotExist(err error) bool {
	return errors.Is(err, fs.ErrNotExist)
}
