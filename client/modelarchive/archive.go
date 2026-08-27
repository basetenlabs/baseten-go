// Package modelarchive builds uncompressed tar archives of model
// directories for upload to Baseten.
//
// Archive layout: files are stored at the archive root with paths relative to
// the input directory. Symlinks are stored as symlinks rather than followed,
// and only if they resolve within the archive; one that does not is rejected
// (see [InvalidSymlinkError]). Other irregular files, such as sockets and
// devices, are skipped.
//
// [BuildModelArchive] produces the tar stream. [WalkModelArchive] exposes the
// same enumeration without the tar framing, so callers that need to summarize
// an archive's contents (for example, hashing them to detect changes) see
// exactly the entries the upload would carry.
//
// Ignore handling is driven by a caller-supplied [IgnoreFileFunc]. If a
// .truss_ignore file is present at the root of the input directory, callers
// must supply an IgnoreFileProcessor to parse it; otherwise [DefaultIgnoreFile]
// (or a caller-provided default) is applied. Note the underscore in
// .truss_ignore.
//
// This package does not parse config.yaml. Callers that need to inline
// external package directories or substitute a different config.yaml into
// the archive must extract those values themselves and pass them via
// [BuildModelArchiveOptions].
package modelarchive

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const (
	ignoreFileName = ".truss_ignore"
	configFileName = "config.yaml"
)

// IgnoreFileOptions is passed to an [IgnoreFileFunc] for each candidate path
// encountered during the walk.
type IgnoreFileOptions struct {
	// RelPath is the path relative to the archive root, using forward
	// slashes on all platforms.
	RelPath string
	// Entry is the directory entry as returned by [filepath.WalkDir].
	Entry fs.DirEntry
}

// IgnoreFileFunc reports whether a given path should be excluded from the
// archive. Returning an error aborts the archive build immediately and
// propagates the error to the reader.
//
// When the function returns true for a directory, the walker prunes the
// entire subtree.
type IgnoreFileFunc func(context.Context, IgnoreFileOptions) (ignore bool, err error)

// IgnoreFileProcessorOptions is passed to an IgnoreFileProcessor when a
// .truss_ignore file is found at the root of the input directory.
type IgnoreFileProcessorOptions struct {
	// Path is the absolute path to the .truss_ignore file.
	Path string
	// Contents is the raw bytes of the .truss_ignore file.
	Contents []byte
}

// BuildModelArchiveOptions configures [BuildModelArchive].
type BuildModelArchiveOptions struct {
	// Dir is the absolute or relative path to the model directory to
	// archive. Required.
	Dir string

	// ConfigYAMLOverride, if non-nil, replaces the contents of the root
	// config.yaml entry in the archive. If nil, any config.yaml on disk
	// at Dir is archived verbatim.
	ConfigYAMLOverride []byte

	// ExternalPackageDirs are extra directories whose contents are inlined
	// under BundledPackagesDir in the archive. Paths may be absolute or
	// relative to Dir. The basename of each entry is not preserved; its
	// children land directly under BundledPackagesDir.
	//
	// Read from the `external_package_dirs` field of the model's config.yaml.
	ExternalPackageDirs []string

	// BundledPackagesDir is the directory inside the archive that receives
	// inlined ExternalPackageDirs contents. Required when ExternalPackageDirs
	// is non-empty.
	//
	// Read from the `bundled_packages_dir` field of the model's config.yaml
	// (the canonical default is "packages").
	BundledPackagesDir string

	// IgnoreFileProcessor parses the contents of a .truss_ignore file
	// found at the root of Dir into an [IgnoreFileFunc]. Required if a
	// .truss_ignore file is present; otherwise [BuildModelArchive] returns
	// an error. When nil and no .truss_ignore exists, DefaultIgnoreFile
	// is used.
	IgnoreFileProcessor func(context.Context, IgnoreFileProcessorOptions) (IgnoreFileFunc, error)

	// DefaultIgnoreFile is applied when no .truss_ignore is present in
	// Dir. If nil, the package-level [DefaultIgnoreFile] function is used.
	// Pass a no-op function to disable default ignoring entirely.
	DefaultIgnoreFile IgnoreFileFunc

	// IncludeDirsInWalk makes [WalkModelArchive] report directory entries in
	// addition to the archive's members. An archive never contains directory
	// entries, so this affects walking only and [BuildModelArchive] ignores
	// it. Callers that mirror a source tree rather than an archive, and so
	// need to see empty directories, set this.
	IncludeDirsInWalk bool
}

// BuildModelArchive returns a [io.ReadCloser] that streams an uncompressed
// tar archive of the model directory described by opts. File contents are read
// lazily as the reader is consumed; callers must Close it to release the
// underlying goroutine.
//
// The entries themselves are enumerated before this returns, so everything
// that enumeration can reject (a symlink the archive cannot carry, two sources
// colliding on one archive path) is reported here rather than from a Read once
// an upload is already underway. Errors reading file contents still surface
// from Read. Cancelling ctx aborts either stage.
func BuildModelArchive(ctx context.Context, opts BuildModelArchiveOptions) (io.ReadCloser, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	ignoreFn, err := resolveIgnoreFunc(ctx, opts)
	if err != nil {
		return nil, err
	}

	// An archive has no directory entries, so the walk-only knob never applies
	// here. opts is a value copy, so this does not affect the caller.
	opts.IncludeDirsInWalk = false

	var files []File
	if err := walkFiles(ctx, opts, ignoreFn, func(f File) error {
		files = append(files, f)
		return nil
	}); err != nil {
		return nil, err
	}

	pr, pw := io.Pipe()
	go func() {
		_ = pw.CloseWithError(writeArchive(ctx, pw, files))
	}()
	return pr, nil
}

// File is a single entry in a model archive, as reported by
// [WalkModelArchive].
type File struct {
	// ArchivePath is the entry's path inside the archive, relative to the
	// archive root and using forward slashes on all platforms.
	ArchivePath string

	// SourcePath is the path of the file on disk. It is empty for synthesized
	// entries, which is currently only the config.yaml written from
	// [BuildModelArchiveOptions.ConfigYAMLOverride].
	SourcePath string

	// Info is the on-disk file info for SourcePath, not following symlinks. It
	// is nil for synthesized entries, which are always regular files.
	// Directory entries, reported only when
	// [BuildModelArchiveOptions.IncludeDirsInWalk] is set, are the ones for
	// which Info.IsDir reports true.
	Info fs.FileInfo

	// LinkTarget is the symlink's target, exactly as stored on disk, and is
	// non-empty for symlink entries and only those. It has been validated to
	// resolve within the archive, so consumers can use it without re-reading
	// the link and getting an answer the archive did not accept.
	LinkTarget string

	// Size is the entry's content length in bytes. Symlink and directory
	// entries carry no content, so theirs is zero.
	Size int64

	// Open opens the entry's contents. The caller must Close the result. It is
	// never nil: for entries that carry no content it yields no bytes.
	Open func() (io.ReadCloser, error)
}

// openNothing is the [File.Open] of an entry with no contents.
func openNothing() (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(nil)), nil
}

// InvalidSymlinkError reports a symlink a model archive cannot carry, because
// its target is absolute or reaches outside the archive.
type InvalidSymlinkError struct {
	// ArchivePath is the symlink's path inside the archive.
	ArchivePath string
	// Target is the link target exactly as stored on disk.
	Target string
}

func (e *InvalidSymlinkError) Error() string {
	return fmt.Sprintf("modelarchive: %s is a symlink to %s, which is outside the model "+
		"directory. Only symlinks resolving within the pushed directory can be uploaded; "+
		"remove it or add it to %s", e.ArchivePath, e.Target, ignoreFileName)
}

// validateSymlink reads the symlink at sourcePath and returns its target
// unchanged, given archivePath as where the symlink lands in the archive. A
// target that is absolute, or that reaches outside the archive root from
// there, is rejected with an [InvalidSymlinkError].
//
// The check is lexical rather than resolved against the local filesystem,
// matching the one applied when the archive is extracted, so a symlink
// accepted here is a symlink that extracts.
func validateSymlink(archivePath, sourcePath string) (string, error) {
	target, err := os.Readlink(sourcePath)
	if err != nil {
		return "", fmt.Errorf("modelarchive: read symlink %s: %w", sourcePath, err)
	}
	// A rooted Windows target without a drive letter ("\foo") is one filepath
	// calls relative, hence the second test.
	slashed := filepath.ToSlash(target)
	if filepath.IsAbs(target) || strings.HasPrefix(slashed, "/") {
		return "", &InvalidSymlinkError{ArchivePath: archivePath, Target: target}
	}
	if dest := path.Join(path.Dir(archivePath), slashed); dest == ".." || strings.HasPrefix(dest, "../") {
		return "", &InvalidSymlinkError{ArchivePath: archivePath, Target: target}
	}
	return target, nil
}

// WalkModelArchive calls fn once for each entry that [BuildModelArchive] would
// place in the archive described by opts, in the order the archive would store
// them: the config.yaml override (if any), then the contents of Dir, then each
// of ExternalPackageDirs. Within a directory, entries are visited in lexical
// order, so a given directory tree always produces the same sequence. Setting
// [BuildModelArchiveOptions.IncludeDirsInWalk] adds the directory entries the
// archive itself omits.
//
// An entry the archive cannot carry aborts the walk with an error rather than
// being reported to fn, so a walk is also a validation of the source tree.
//
// This is the enumeration [BuildModelArchive] itself runs on, so the two never
// disagree about which paths an archive contains. Callers hashing an archive's
// contents should ignore [File.Info] and the walk order, since neither the
// modification times nor the tar framing are part of the model's source.
//
// Returning an error from fn aborts the walk and propagates the error.
func WalkModelArchive(ctx context.Context, opts BuildModelArchiveOptions, fn func(File) error) error {
	if err := opts.Validate(); err != nil {
		return err
	}
	ignoreFn, err := resolveIgnoreFunc(ctx, opts)
	if err != nil {
		return err
	}
	return walkFiles(ctx, opts, ignoreFn, fn)
}

// Validate reports whether the options describe a buildable archive, checking
// every precondition that does not require walking the directories: the source
// directories exist, and the bundled packages dir is a usable archive path.
//
// [BuildModelArchive] and [WalkModelArchive] call this themselves. Callers can
// call it earlier to reject an unbuildable archive before doing other work.
// Errors in the contents, such as two files colliding on one archive path,
// still surface only from the build.
func (o BuildModelArchiveOptions) Validate() error {
	if o.Dir == "" {
		return errors.New("modelarchive: Dir is required")
	}
	if err := statDir(o.Dir, "model dir"); err != nil {
		return err
	}
	if len(o.ExternalPackageDirs) > 0 && o.BundledPackagesDir == "" {
		return errors.New("modelarchive: BundledPackagesDir is required when ExternalPackageDirs is non-empty")
	}
	if o.BundledPackagesDir != "" {
		clean := path.Clean(filepath.ToSlash(o.BundledPackagesDir))
		if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
			return fmt.Errorf("modelarchive: BundledPackagesDir must be a relative path within the archive, got %q", o.BundledPackagesDir)
		}
	}
	for _, extDir := range o.ExternalPackageDirs {
		if err := statDir(o.resolveExternalDir(extDir), "external package dir"); err != nil {
			return err
		}
	}
	return nil
}

// resolveExternalDir resolves an external package dir, which may be absolute
// or relative to Dir.
func (o BuildModelArchiveOptions) resolveExternalDir(extDir string) string {
	if filepath.IsAbs(extDir) {
		return extDir
	}
	return filepath.Join(o.Dir, extDir)
}

// statDir confirms dir exists and is a directory, naming it as what in any
// error so a failure says which of the archive's directories is at fault.
func statDir(dir, what string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("modelarchive: stat %s %s: %w", what, dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("modelarchive: %s %s is not a directory", what, dir)
	}
	return nil
}

// resolveIgnoreFunc determines the IgnoreFileFunc to use for the walk: if a
// .truss_ignore file exists at the root of opts.Dir, it is parsed via
// opts.IgnoreFileProcessor (which must be non-nil); otherwise
// opts.DefaultIgnoreFile or the package default is used.
func resolveIgnoreFunc(ctx context.Context, opts BuildModelArchiveOptions) (IgnoreFileFunc, error) {
	ignorePath := filepath.Join(opts.Dir, ignoreFileName)
	contents, err := os.ReadFile(ignorePath)
	if errors.Is(err, fs.ErrNotExist) {
		if opts.DefaultIgnoreFile != nil {
			return opts.DefaultIgnoreFile, nil
		}
		return DefaultIgnoreFile, nil
	}
	if err != nil {
		return nil, fmt.Errorf("modelarchive: read %s: %w", ignorePath, err)
	}
	if opts.IgnoreFileProcessor == nil {
		return nil, fmt.Errorf("modelarchive: %s present but IgnoreFileProcessor is nil", ignorePath)
	}
	absPath, absErr := filepath.Abs(ignorePath)
	if absErr != nil {
		absPath = ignorePath
	}
	fn, err := opts.IgnoreFileProcessor(ctx, IgnoreFileProcessorOptions{
		Path:     absPath,
		Contents: contents,
	})
	if err != nil {
		return nil, fmt.Errorf("modelarchive: ignore file processor: %w", err)
	}
	return fn, nil
}

// writeArchive writes a tar stream of the already-enumerated files to w.
func writeArchive(ctx context.Context, w io.Writer, files []File) error {
	tw := tar.NewWriter(w)
	defer tw.Close()

	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := writeTarEntry(tw, f); err != nil {
			return err
		}
	}
	return nil
}

// walkFiles enumerates the archive's entries and calls fn for each. The
// ignoreFn (which may be nil) is consulted for every entry except the roots.
// After Dir is walked, each entry in opts.ExternalPackageDirs is walked and
// reported under opts.BundledPackagesDir, mirroring the Python gather() step.
func walkFiles(
	ctx context.Context,
	opts BuildModelArchiveOptions,
	ignoreFn IgnoreFileFunc,
	fn func(File) error,
) error {
	// Tracks archive path -> source path so two source files that resolve to
	// the same archive path are reported rather than silently shadowing.
	emitted := map[string]string{}
	emit := func(f File) error {
		// Two external package dirs legitimately contribute the same directory
		// to one archive path, so only their contents have to be unique.
		if f.Info != nil && f.Info.IsDir() {
			return fn(f)
		}
		if prev, dup := emitted[f.ArchivePath]; dup {
			return fmt.Errorf("modelarchive: duplicate archive entry %q: both %s and %s map to it. "+
				"Two source files resolve to the same archive path, commonly because multiple external_package_dirs "+
				"(or an external_package_dir and the model directory) contain a file with the same relative path. "+
				"Rename or remove one so each archive path is unique", f.ArchivePath, prev, f.SourcePath)
		}
		if err := fn(f); err != nil {
			return err
		}
		emitted[f.ArchivePath] = f.SourcePath
		return nil
	}

	if opts.ConfigYAMLOverride != nil {
		data := opts.ConfigYAMLOverride
		if err := emit(File{
			ArchivePath: configFileName,
			Size:        int64(len(data)),
			Open:        func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil },
		}); err != nil {
			return err
		}
	}

	walkErr := walkDir(ctx, opts.Dir, "", ignoreFn, opts.IncludeDirsInWalk, func(f File) error {
		if f.ArchivePath == configFileName && opts.ConfigYAMLOverride != nil {
			return nil
		}
		return emit(f)
	})
	if walkErr != nil {
		return walkErr
	}

	for _, extDir := range opts.ExternalPackageDirs {
		if err := walkDir(ctx, opts.resolveExternalDir(extDir), opts.BundledPackagesDir, ignoreFn, opts.IncludeDirsInWalk, emit); err != nil {
			return err
		}
	}
	return nil
}

// walkDir walks root and calls fn for each entry the archive carries, plus the
// directories themselves when includeDirs is set, reporting archive paths under
// prefix. An external package dir's own basename is not preserved: its children
// land directly under prefix, matching Python's gather.
func walkDir(
	ctx context.Context,
	root, prefix string,
	ignoreFn IgnoreFileFunc,
	includeDirs bool,
	fn func(File) error,
) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		} else if err := ctx.Err(); err != nil {
			return err
		} else if p == root {
			return nil
		}

		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		archivePath := filepath.ToSlash(rel)
		if prefix != "" {
			archivePath = path.Join(prefix, archivePath)
		}

		if ignoreFn != nil {
			ignore, ierr := ignoreFn(ctx, IgnoreFileOptions{RelPath: archivePath, Entry: d})
			if ierr != nil {
				return ierr
			}
			if ignore {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}

		if d.IsDir() && !includeDirs {
			return nil
		}
		// Sockets, devices and the like cannot be represented in a tar the
		// extractor will accept, and carry no contents, so they are dropped.
		if !d.IsDir() && d.Type()&fs.ModeSymlink == 0 && !d.Type().IsRegular() {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("modelarchive: stat %s: %w", p, err)
		}

		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, err := validateSymlink(archivePath, p)
			if err != nil {
				return err
			}
			return fn(File{
				ArchivePath: archivePath,
				SourcePath:  p,
				Info:        info,
				LinkTarget:  target,
				Open:        openNothing,
			})
		case d.IsDir():
			return fn(File{
				ArchivePath: archivePath,
				SourcePath:  p,
				Info:        info,
				Open:        openNothing,
			})
		default:
			return fn(File{
				ArchivePath: archivePath,
				SourcePath:  p,
				Info:        info,
				Size:        info.Size(),
				Open: func() (io.ReadCloser, error) {
					f, err := os.Open(p)
					if err != nil {
						return nil, fmt.Errorf("modelarchive: open %s: %w", p, err)
					}
					return f, nil
				},
			})
		}
	})
}

// writeTarEntry writes a single entry to tw. If f.Info is non-nil, the tar
// header is derived from it (preserving mode/mtime); otherwise a synthesized
// header is used.
func writeTarEntry(tw *tar.Writer, f File) error {
	var hdr *tar.Header
	if f.Info != nil {
		var err error
		hdr, err = tar.FileInfoHeader(f.Info, f.LinkTarget)
		if err != nil {
			return fmt.Errorf("modelarchive: header for %s: %w", f.ArchivePath, err)
		}
		hdr.Name = f.ArchivePath
		hdr.Size = f.Size
	} else {
		hdr = &tar.Header{
			Name:     f.ArchivePath,
			Mode:     0o644,
			Size:     f.Size,
			ModTime:  time.Now(),
			Typeflag: tar.TypeReg,
		}
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("modelarchive: write header %s: %w", f.ArchivePath, err)
	}

	// A symlink is a header alone: its target is in the header and it has no
	// body to copy.
	if f.LinkTarget != "" {
		return nil
	}
	r, err := f.Open()
	if err != nil {
		return err
	}
	defer r.Close()
	if _, err := io.CopyN(tw, r, f.Size); err != nil {
		return fmt.Errorf("modelarchive: copy %s: %w", f.ArchivePath, err)
	}
	return nil
}

// DefaultIgnoreFile reports whether a path should be excluded using the
// default ignore rules, applied when no .truss_ignore file is present. It
// excludes the usual Python build, cache, and environment cruft (__pycache__,
// build/dist directories, virtualenvs, *.pyc, .DS_Store, .git, and so on).
//
// A directory named by a directory-only rule (such as __pycache__) is itself
// kept while its contents are excluded: the bare directory still appears in a
// model's signature even when everything inside it is ignored. So directory
// rules ([isDefaultIgnoredDirName] and *.egg-info) match only an ancestor
// component, while bare-name rules ([isDefaultIgnoredName]) match the entry
// itself or any ancestor.
func DefaultIgnoreFile(_ context.Context, opts IgnoreFileOptions) (bool, error) {
	components := strings.Split(opts.RelPath, "/")
	base := components[len(components)-1]

	// Basename suffix/prefix globs (*.pyc, .coverage.*, ...) match the final
	// path component only.
	for _, suffix := range defaultIgnoreSuffixes {
		if strings.HasSuffix(base, suffix) {
			return true, nil
		}
	}
	for _, prefix := range defaultIgnorePrefixes {
		if strings.HasPrefix(base, prefix) {
			return true, nil
		}
	}

	// Bare names (no trailing slash, e.g. .env, .DS_Store, .git): gitignore
	// matches them at any depth, as the entry itself or as an ancestor
	// directory, so a match on any component wins.
	for _, c := range components {
		if isDefaultIgnoredName(c) {
			return true, nil
		}
	}

	// Dir-only patterns ("__pycache__/", "*.egg-info/", ...) match an entry's
	// contents but not the bare directory, so only ancestor components count.
	for _, c := range components[:len(components)-1] {
		if isDefaultIgnoredDirName(c) || strings.HasSuffix(c, ".egg-info") {
			return true, nil
		}
	}

	// Root-anchored dir patterns match strictly under the anchored path, never
	// the bare directory (which Truss keeps as a null entry).
	for _, anchored := range defaultIgnoreAnchored {
		if strings.HasPrefix(opts.RelPath, anchored+"/") {
			return true, nil
		}
	}
	return false, nil
}

// Root-anchored dir patterns from the bundled .truss_ignore. Matched against
// the full RelPath, not the basename, so e.g. "docs/_build" only triggers
// under a top-level docs/ directory.
var defaultIgnoreAnchored = []string{
	"docs/_build",
	"share/python-wheels",
}

// Suffix patterns from the bundled .truss_ignore. Includes the *.py[cod]
// expansion and *$py.class / *.py,cover / *.sage.py as plain suffixes.
// "*.egg-info/" is NOT here: it is dir-only and handled as an ancestor match.
var defaultIgnoreSuffixes = []string{
	".pyc", ".pyo", ".pyd", "$py.class", ".so", ".egg", ".manifest", ".spec", ".cover", ".py,cover", ".mo",
	".pot", ".log", ".sage.py", ".tmp", ".swp",
}

// Prefix patterns: ".coverage.*" matches anything starting with ".coverage.".
var defaultIgnorePrefixes = []string{
	".coverage.",
}

// isDefaultIgnoredName reports whether a path component matches one of the
// bare-name patterns (no trailing slash) from the bundled
// baseten-truss/truss/util/.truss_ignore. These match the entry itself.
func isDefaultIgnoredName(component string) bool {
	switch component {
	case ".Python", ".installed.cfg", "MANIFEST", ".DS_Store", "pip-log.txt",
		"pip-delete-this-directory.txt", ".coverage", ".cache", "nosetests.xml",
		"coverage.xml", "local_settings.py", "db.sqlite3", "db.sqlite3-journal",
		".webassets-cache", ".scrapy", ".ipynb_checkpoints", "ipython_config.py",
		".pdm.toml", "celerybeat-schedule", "celerybeat.pid", ".env", ".venv",
		".spyderproject", ".spyproject", ".ropeproject", ".dmypy.json",
		"dmypy.json", ".git":
		return true
	}
	return false
}

// isDefaultIgnoredDirName reports whether a path component matches one of the
// dir-only patterns (trailing slash) from the bundled
// baseten-truss/truss/util/.truss_ignore. These match a directory's contents
// but not the bare directory entry, so callers test ancestor components only.
func isDefaultIgnoredDirName(component string) bool {
	switch component {
	case "__pycache__", "build", "develop-eggs", "dist", "downloads", "eggs",
		".eggs", "lib", "lib64", "parts", "sdist", "var", "wheels", "htmlcov",
		".tox", ".nox", ".hypothesis", ".pytest_cache", "cover", "instance",
		".pybuilder", "target", "profile_default", "__pypackages__", "env",
		"venv", "ENV", "env.bak", "venv.bak", ".mypy_cache", ".ruff_cache",
		".pyre", ".pytype", "cython_debug":
		return true
	}
	return false
}
