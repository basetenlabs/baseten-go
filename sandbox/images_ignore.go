package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const dockerignoreFileName = ".dockerignore"

// ImageIgnoreFileOptions is passed to an [ImageIgnoreFileFunc] for each
// candidate path in a directory being pushed.
type ImageIgnoreFileOptions struct {
	// RelPath is the path relative to the directory's root, using forward
	// slashes on all platforms.
	RelPath string

	// Entry is the directory entry as returned by [os.ReadDir].
	Entry fs.DirEntry
}

// ImageIgnoreFileFunc reports whether a path should be left out of a pushed
// build context. Returning an error fails the push before anything is sent.
//
// When the function returns true for a directory, its entire subtree is left
// out.
type ImageIgnoreFileFunc func(context.Context, ImageIgnoreFileOptions) (ignore bool, err error)

// ImageIgnoreFileProcessorOptions is passed to
// [ImagePushOptions.IgnoreFileProcessor] when a .dockerignore file is found at
// the root of [ImagePushOptions.SourceDirectory].
type ImageIgnoreFileProcessorOptions struct {
	// Path is the absolute path to the .dockerignore file.
	Path string

	// Contents is the raw bytes of the .dockerignore file.
	Contents []byte
}

// defaultIgnoredNames are left out at any depth by [DefaultImageIgnoreFile].
var defaultIgnoredNames = []string{
	".blaxel", ".env.build", ".docker", ".git", "dist", ".venv", "venv",
	"node_modules", ".env", ".next", "__pycache__",
}

// DefaultImageIgnoreFile reports whether a path is left out of a pushed build
// context by the default rules, applied when the directory has no
// .dockerignore. Its result is the same as this .dockerignore:
//
//	**/.blaxel
//	**/.env.build
//	**/.docker
//	**/.git
//	**/dist
//	**/.venv
//	**/venv
//	**/node_modules
//	**/.env
//	.env*
//	**/.next
//	**/__pycache__
//
// So each of those names is left out at any depth, as a file or as a
// directory with everything in it, and a name starting with .env is left out
// at the root. To extend the defaults, copy them into a .dockerignore.
func DefaultImageIgnoreFile(_ context.Context, opts ImageIgnoreFileOptions) (bool, error) {
	components := strings.Split(opts.RelPath, "/")
	// Any component, so a path under an ignored directory is ignored even
	// when its directory was not pruned first.
	for _, component := range components {
		if slices.Contains(defaultIgnoredNames, component) {
			return true, nil
		}
	}
	// .env* has no **/, so it matches at the root only, as Docker reads it.
	return strings.HasPrefix(components[0], ".env"), nil
}

// resolveImageIgnoreFile determines how a pushed directory is filtered: by
// its .dockerignore through opts.IgnoreFileProcessor, which must then be set,
// or else by opts.DefaultIgnoreFile or [DefaultImageIgnoreFile].
func resolveImageIgnoreFile(ctx context.Context, opts ImagePushOptions) (ImageIgnoreFileFunc, error) {
	ignorePath := filepath.Join(opts.SourceDirectory, dockerignoreFileName)
	contents, err := os.ReadFile(ignorePath)
	if errors.Is(err, fs.ErrNotExist) {
		if opts.DefaultIgnoreFile != nil {
			return opts.DefaultIgnoreFile, nil
		}
		return DefaultImageIgnoreFile, nil
	} else if err != nil {
		return nil, fmt.Errorf("reading %s: %w", ignorePath, err)
	}
	// Failing, rather than pushing everything, keeps files the caller meant
	// to leave out, such as secrets, from being uploaded.
	if opts.IgnoreFileProcessor == nil {
		return nil, fmt.Errorf("%s exists but IgnoreFileProcessor is not set", ignorePath)
	}
	absPath, err := filepath.Abs(ignorePath)
	if err != nil {
		return nil, err
	}
	return opts.IgnoreFileProcessor(ctx, ImageIgnoreFileProcessorOptions{Path: absPath, Contents: contents})
}
