package sandbox

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/basetenlabs/baseten-go/internal/require"
)

func TestDefaultImageIgnoreFile(t *testing.T) {
	for path, ignored := range map[string]bool{
		".git":                    true,
		".git/config":             true,
		"app/node_modules":        true,
		"app/node_modules/x/y.js": true,
		"a/b/__pycache__":         true,
		"dist":                    true,
		"src/dist":                true,
		".env":                    true,
		"app/.env":                true,
		".env.local":              true,
		".envrc":                  true,
		".blaxel":                 true,
		".env.build":              true,
		"app/.env.build":          true,
		// .env* matches at the root only.
		"app/.env.local": false,
		"distribution":   false,
		"main.py":        false,
		"src/app.go":     false,
		"Dockerfile":     false,
		"git":            false,
	} {
		got, err := DefaultImageIgnoreFile(t.Context(), ImageIgnoreFileOptions{RelPath: path})
		require.NoError(t, err)
		require.True(t, got == ignored, "%s: got ignored %t", path, got)
	}
}

func TestImagePushDockerignore(t *testing.T) {
	// writeTree creates a directory of files by forward-slashed path.
	writeTree := func(t *testing.T, files map[string]string) string {
		t.Helper()
		dir := t.TempDir()
		for path, content := range files {
			full := filepath.Join(dir, filepath.FromSlash(path))
			require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
			require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
		}
		return dir
	}
	// push pushes dir and returns the uploaded archive's contents.
	push := func(t *testing.T, opts ImagePushOptions) map[string]string {
		t.Helper()
		cp := newControlPlane(t)
		store := newStorage(t, http.StatusOK)
		cp.respond("POST", "/v1/sandboxes/images", 202, `{"name":"img","status":"UPLOADING","upload_url":"`+store.URL+`"}`)
		opts.Name, opts.NoWait = "img", true
		_, err := cp.client(t, ClientOptions{}).Images().Push(t.Context(), opts)
		require.NoError(t, err)
		contents, _ := readZip(t, store.body)
		return contents
	}

	t.Run("DefaultsWithoutDockerignore", func(t *testing.T) {
		dir := writeTree(t, map[string]string{
			"Dockerfile":         "FROM debian\n",
			"main.py":            "print()",
			".env":               "SECRET=1",
			".git/config":        "x",
			"web/node_modules/a": "x",
			"web/.env.local":     "nested",
		})
		contents := push(t, ImagePushOptions{SourceDirectory: dir})
		require.MapEqual(t, contents, "main.py", "print()")
		require.MapEqual(t, contents, "web/.env.local", "nested")
		for _, name := range []string{".env", ".git/", ".git/config", "web/node_modules/", "web/node_modules/a"} {
			_, ok := contents[name]
			require.False(t, ok, "%s was not left out", name)
		}
	})

	t.Run("DefaultIgnoreFileReplacesDefaults", func(t *testing.T) {
		dir := writeTree(t, map[string]string{"Dockerfile": "FROM debian\n", ".env": "SECRET=1"})
		contents := push(t, ImagePushOptions{
			SourceDirectory:   dir,
			DefaultIgnoreFile: func(context.Context, ImageIgnoreFileOptions) (bool, error) { return false, nil },
		})
		require.MapEqual(t, contents, ".env", "SECRET=1")
	})

	t.Run("DockerignoreThroughProcessor", func(t *testing.T) {
		dir := writeTree(t, map[string]string{
			"Dockerfile":    "FROM debian\n",
			".dockerignore": "secret.txt\n",
			"secret.txt":    "x",
			// Not left out: a .dockerignore replaces the defaults.
			".env": "SECRET=1",
		})
		var got ImageIgnoreFileProcessorOptions
		contents := push(t, ImagePushOptions{
			SourceDirectory: dir,
			IgnoreFileProcessor: func(_ context.Context, opts ImageIgnoreFileProcessorOptions) (ImageIgnoreFileFunc, error) {
				got = opts
				return func(_ context.Context, opts ImageIgnoreFileOptions) (bool, error) {
					return opts.RelPath == "secret.txt", nil
				}, nil
			},
		})
		require.True(t, filepath.IsAbs(got.Path), "processor path %s is not absolute", got.Path)
		require.Equal(t, "secret.txt\n", string(got.Contents))
		require.MapEqual(t, contents, ".dockerignore", "secret.txt\n")
		require.MapEqual(t, contents, ".env", "SECRET=1")
		_, ok := contents["secret.txt"]
		require.False(t, ok, "secret.txt was not left out")
	})

	t.Run("DockerignoreWithoutProcessorFailsBeforeSending", func(t *testing.T) {
		dir := writeTree(t, map[string]string{"Dockerfile": "FROM debian\n", ".dockerignore": "x\n"})
		cp := newControlPlane(t)
		_, err := cp.client(t, ClientOptions{}).Images().Push(t.Context(), ImagePushOptions{Name: "img", SourceDirectory: dir})
		require.Error(t, err)
		require.Contains(t, err.Error(), "IgnoreFileProcessor is not set")
		require.Len(t, cp.requests, 0)
	})
}
