// Package modelarchive builds uncompressed tar archives of Truss model
// directories for upload to Baseten.
//
// Archive layout matches the canonical Python implementation in baseten-truss
// (truss/remote/baseten/utils/tar.py + truss/util/path.py): files are stored
// at the archive root with paths relative to the input directory, symlinks
// are not followed, and only regular files are included.
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

const ignoreFileName = ".truss_ignore"

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
	// Read from the `external_package_dirs` field of the Truss config.yaml.
	ExternalPackageDirs []string

	// BundledPackagesDir is the directory inside the archive that receives
	// inlined ExternalPackageDirs contents. Required when ExternalPackageDirs
	// is non-empty.
	//
	// Read from the `bundled_packages_dir` field of the Truss config.yaml
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
}

// BuildModelArchive returns a [io.ReadCloser] that streams an uncompressed
// tar archive of the model directory described by opts. The archive is
// produced lazily as the reader is consumed; callers must Close it to
// release the underlying walk goroutine.
//
// Errors encountered during the walk surface from the next Read call.
// Cancelling ctx also aborts the build.
func BuildModelArchive(ctx context.Context, opts BuildModelArchiveOptions) (io.ReadCloser, error) {
	if opts.Dir == "" {
		return nil, errors.New("modelarchive: Dir is required")
	}
	info, err := os.Stat(opts.Dir)
	if err != nil {
		return nil, fmt.Errorf("modelarchive: stat %s: %w", opts.Dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("modelarchive: %s is not a directory", opts.Dir)
	}
	if len(opts.ExternalPackageDirs) > 0 && opts.BundledPackagesDir == "" {
		return nil, errors.New("modelarchive: BundledPackagesDir is required when ExternalPackageDirs is non-empty")
	}

	ignoreFn, err := resolveIgnore(ctx, opts)
	if err != nil {
		return nil, err
	}

	pr, pw := io.Pipe()
	go func() {
		err := writeArchive(ctx, pw, opts, ignoreFn)
		_ = pw.CloseWithError(err)
	}()
	return pr, nil
}

// resolveIgnore determines the IgnoreFileFunc to use for the walk: if a
// .truss_ignore file exists at the root of opts.Dir, it is parsed via
// opts.IgnoreFileProcessor (which must be non-nil); otherwise
// opts.DefaultIgnoreFile or the package default is used.
func resolveIgnore(ctx context.Context, opts BuildModelArchiveOptions) (IgnoreFileFunc, error) {
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

// writeArchive walks opts.Dir and writes a tar stream to w. The ignoreFn (which
// may be nil) is consulted for every entry except the root. After Dir is
// walked, each entry in opts.ExternalPackageDirs is walked and emitted under
// opts.BundledPackagesDir, mirroring the Python gather() step.
func writeArchive(
	ctx context.Context,
	w io.Writer,
	opts BuildModelArchiveOptions,
	ignoreFn IgnoreFileFunc,
) error {
	tw := tar.NewWriter(w)
	defer tw.Close()

	emitted := map[string]struct{}{}
	configWritten := false

	walkErr := filepath.WalkDir(opts.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		} else if err := ctx.Err(); err != nil {
			return err
		} else if p == opts.Dir {
			return nil
		}

		rel, err := filepath.Rel(opts.Dir, p)
		if err != nil {
			return err
		}
		relSlash := filepath.ToSlash(rel)

		if ignoreFn != nil {
			ignore, ierr := ignoreFn(ctx, IgnoreFileOptions{RelPath: relSlash, Entry: d})
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

		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("modelarchive: stat %s: %w", p, err)
		}
		if relSlash == "config.yaml" && opts.ConfigYAMLOverride != nil {
			if err := emitBytes(tw, "config.yaml", opts.ConfigYAMLOverride, emitted); err != nil {
				return err
			}
			configWritten = true
			return nil
		}
		return emitFile(tw, relSlash, p, info, emitted)
	})
	if walkErr != nil {
		return walkErr
	}

	if opts.ConfigYAMLOverride != nil && !configWritten {
		if err := emitBytes(tw, "config.yaml", opts.ConfigYAMLOverride, emitted); err != nil {
			return err
		}
	}

	for _, extDir := range opts.ExternalPackageDirs {
		absExt := extDir
		if !filepath.IsAbs(absExt) {
			absExt = filepath.Join(opts.Dir, absExt)
		}
		info, err := os.Stat(absExt)
		if err != nil {
			return fmt.Errorf("modelarchive: stat external package dir %s: %w", absExt, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("modelarchive: external package dir %s is not a directory", absExt)
		}
		if err := walkExternalDir(ctx, tw, absExt, opts.BundledPackagesDir, ignoreFn, emitted); err != nil {
			return err
		}
	}
	return nil
}

// walkExternalDir walks an external package directory and emits its contents
// into the archive under bundledPackagesDir (matching Python's gather:
// children of the external dir land directly under bundled_packages_dir, the
// external dir's own basename is not preserved).
func walkExternalDir(ctx context.Context, tw *tar.Writer, extDir, bundledPackagesDir string, ignoreFn IgnoreFileFunc, emitted map[string]struct{}) error {
	return filepath.WalkDir(extDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		} else if err := ctx.Err(); err != nil {
			return err
		} else if p == extDir {
			return nil
		}

		rel, err := filepath.Rel(extDir, p)
		if err != nil {
			return err
		}
		archivePath := path.Join(bundledPackagesDir, filepath.ToSlash(rel))

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

		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("modelarchive: stat %s: %w", p, err)
		}
		return emitFile(tw, archivePath, p, info, emitted)
	})
}

func emitFile(tw *tar.Writer, archivePath, srcPath string, info fs.FileInfo, emitted map[string]struct{}) error {
	if _, dup := emitted[archivePath]; dup {
		return fmt.Errorf("modelarchive: duplicate archive entry %s (from %s)", archivePath, srcPath)
	}
	f, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("modelarchive: open %s: %w", srcPath, err)
	}
	err = writeTarEntry(tw, archivePath, info, f, info.Size())
	_ = f.Close()
	if err != nil {
		return err
	}
	emitted[archivePath] = struct{}{}
	return nil
}

func emitBytes(tw *tar.Writer, archivePath string, data []byte, emitted map[string]struct{}) error {
	if _, dup := emitted[archivePath]; dup {
		return fmt.Errorf("modelarchive: duplicate archive entry %s", archivePath)
	}
	if err := writeTarEntry(tw, archivePath, nil, bytes.NewReader(data), int64(len(data))); err != nil {
		return err
	}
	emitted[archivePath] = struct{}{}
	return nil
}

// writeTarEntry writes a single regular file entry to tw. If info is non-nil,
// the tar header is derived from it (preserving mode/mtime); otherwise a
// synthesized header is used. size is the byte count to read from r.
func writeTarEntry(tw *tar.Writer, archivePath string, info fs.FileInfo, r io.Reader, size int64) error {
	var hdr *tar.Header
	if info != nil {
		var err error
		hdr, err = tar.FileInfoHeader(info, "")
		if err != nil {
			return fmt.Errorf("modelarchive: header for %s: %w", archivePath, err)
		}
		hdr.Name = archivePath
		hdr.Size = size
	} else {
		hdr = &tar.Header{
			Name:     archivePath,
			Mode:     0o644,
			Size:     size,
			ModTime:  time.Now(),
			Typeflag: tar.TypeReg,
		}
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("modelarchive: write header %s: %w", archivePath, err)
	}
	if _, err := io.CopyN(tw, r, size); err != nil {
		return fmt.Errorf("modelarchive: copy %s: %w", archivePath, err)
	}
	return nil
}

// DefaultIgnoreFile is the package-level default [IgnoreFileFunc] applied
// when no .truss_ignore is present in the input directory. It mirrors the
// canonical bundled patterns from baseten-truss (truss/util/.truss_ignore).
//
// Matching is basename-only: callers should not rely on path-prefix or glob
// semantics beyond what is encoded here.
func DefaultIgnoreFile(_ context.Context, opts IgnoreFileOptions) (bool, error) {
	base := path.Base(opts.RelPath)
	if isDefaultIgnoredName(base) {
		return true, nil
	}
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
	return false, nil
}

// Suffix patterns from the bundled .truss_ignore. Includes the *.py[cod]
// expansion and *$py.class / *.py,cover / *.sage.py as plain suffixes.
var defaultIgnoreSuffixes = []string{
	".pyc", ".pyo", ".pyd", "$py.class", ".so", ".egg", ".egg-info", ".manifest", ".spec", ".cover", ".py,cover", ".mo",
	".pot", ".log", ".sage.py", ".tmp", ".swp",
}

// Prefix patterns: ".coverage.*" matches anything starting with ".coverage.".
var defaultIgnorePrefixes = []string{
	".coverage.",
}

// isDefaultIgnoredName reports whether basename matches one of the exact-name
// patterns from the bundled baseten-truss/truss/util/.truss_ignore.
func isDefaultIgnoredName(base string) bool {
	switch base {
	case "__pycache__", ".Python", "build", "develop-eggs", "dist", "downloads",
		"eggs", ".eggs", "lib", "lib64", "parts", "sdist", "var", "wheels",
		".installed.cfg", "MANIFEST", ".DS_Store", "pip-log.txt",
		"pip-delete-this-directory.txt", "htmlcov", ".tox", ".nox", ".coverage",
		".cache", "nosetests.xml", "coverage.xml", "instance", ".webassets-cache",
		".scrapy", ".pybuilder", "target", ".ipynb_checkpoints", "profile_default",
		"ipython_config.py", ".pdm.toml", "__pypackages__", "celerybeat-schedule",
		"celerybeat.pid", ".env", ".venv", "env", "venv", "ENV", "env.bak",
		"venv.bak", ".spyderproject", ".spyproject", ".ropeproject", ".mypy_cache",
		".dmypy.json", "dmypy.json", ".ruff_cache", ".pyre", ".pytype",
		"cython_debug", ".git", "local_settings.py", "db.sqlite3",
		"db.sqlite3-journal", ".pytest_cache", "cover":
		return true
	}
	return false
}
