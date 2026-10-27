package sandbox

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-go/internal/require"
)

// storage is a fake signed-URL upload target.
type storage struct {
	*httptest.Server
	mu            sync.Mutex
	contentType   string
	authorization string
	contentLength int64
	header        http.Header
	body          []byte
}

func newStorage(t *testing.T, status int) *storage {
	t.Helper()
	s := &storage{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.contentType = r.Header.Get("Content-Type")
		s.authorization = r.Header.Get("Authorization")
		s.contentLength = r.ContentLength
		s.header = r.Header.Clone()
		s.body = body
		s.mu.Unlock()
		w.WriteHeader(status)
		w.Write([]byte("<Error>denied</Error>"))
	}))
	t.Cleanup(s.Close)
	return s
}

func image(status string) string {
	return `{"name":"img","status":"` + status + `"}`
}

var dockerfileOnly = map[string][]byte{"Dockerfile": []byte("FROM debian\n")}

func TestImagePush(t *testing.T) {
	fast := ImagePushOptions{Name: "img", SourceFiles: dockerfileOnly, PollInterval: time.Millisecond}

	t.Run("UploadsThenWaits", func(t *testing.T) {
		cp := newControlPlane(t)
		store := newStorage(t, http.StatusOK)
		cp.respond("POST", "/v1/sandboxes/images", 202, `{"name":"img","status":"UPLOADING","upload_url":"`+store.URL+`/put"}`)
		// A new image can be missing right after its push.
		cp.respond("GET", "/v1/sandboxes/images/img", 404, `{"code":"NOT_FOUND","message":"image not found"}`)
		cp.respond("GET", "/v1/sandboxes/images/img", 200, image("BUILDING"))
		cp.respond("GET", "/v1/sandboxes/images/img", 200, image("BUILT"))
		info, err := cp.client(t, ClientOptions{}).Images().Push(t.Context(), fast)
		require.NoError(t, err)
		require.Equal(t, ImageStatusBuilt, info.Status)
		require.Len(t, cp.requests, 4)
		require.Equal(t, `{"name":"img"}`, cp.requests[0].body)
		require.Equal(t, "application/zip", store.contentType)
		require.Equal(t, "", store.authorization)
		contents, _ := readZip(t, store.body)
		require.MapEqual(t, contents, "Dockerfile", "FROM debian\n")
	})

	t.Run("BuiltAtFirstPoll", func(t *testing.T) {
		cp := newControlPlane(t)
		store := newStorage(t, http.StatusOK)
		cp.respond("POST", "/v1/sandboxes/images", 202, `{"name":"img","status":"UPLOADING","upload_url":"`+store.URL+`/put"}`)
		// A build that finished before the first poll, such as one reusing an
		// identical context's build.
		cp.respond("GET", "/v1/sandboxes/images/img", 200, image("BUILT"))
		info, err := cp.client(t, ClientOptions{}).Images().Push(t.Context(), fast)
		require.NoError(t, err)
		require.Equal(t, ImageStatusBuilt, info.Status)
	})

	t.Run("RegistryHasNoUpload", func(t *testing.T) {
		cp := newControlPlane(t)
		cp.respond("POST", "/v1/sandboxes/images", 202, `{"name":"img","status":"UPLOADING"}`)
		info, err := cp.client(t, ClientOptions{}).Images().Push(t.Context(), ImagePushOptions{
			Name:           "img",
			SourceRegistry: &ImageRegistrySource{Image: "debian:bookworm", DockerConfig: "{}"},
			NoWait:         true,
		})
		require.NoError(t, err)
		require.Equal(t, ImageStatusUploading, info.Status)
		require.Equal(t, `{"docker_config":"{}","image":"debian:bookworm","name":"img"}`, cp.lastRequest(t).body)
	})

	t.Run("SendsBuildSizesOnlyWhenSet", func(t *testing.T) {
		for _, c := range []struct {
			opts ImagePushOptions
			body string
		}{
			{ImagePushOptions{SourceFiles: dockerfileOnly}, `{"name":"img"}`},
			{ImagePushOptions{SourceFiles: dockerfileOnly, Memory: 2048, Volume: 8192}, `{"memory":2048,"name":"img","volume":8192}`},
			{
				ImagePushOptions{SourceRegistry: &ImageRegistrySource{Image: "debian:bookworm"}, Memory: 2048, Volume: 8192},
				`{"image":"debian:bookworm","memory":2048,"name":"img","volume":8192}`,
			},
		} {
			cp := newControlPlane(t)
			store := newStorage(t, http.StatusOK)
			cp.respond("POST", "/v1/sandboxes/images", 202, `{"name":"img","status":"UPLOADING","upload_url":"`+store.URL+`"}`)
			c.opts.Name, c.opts.NoWait = "img", true
			_, err := cp.client(t, ClientOptions{}).Images().Push(t.Context(), c.opts)
			require.NoError(t, err)
			require.Equal(t, c.body, cp.lastRequest(t).body)
		}
	})

	t.Run("UploadSendsReturnedHeaders", func(t *testing.T) {
		cp := newControlPlane(t)
		store := newStorage(t, http.StatusOK)
		images := cp.client(t, ClientOptions{}).Images()
		cp.respond("POST", "/v1/sandboxes/images", 202, `{"name":"img","status":"UPLOADING","upload_url":"`+store.URL+`"}`)
		_, err := images.Push(t.Context(), ImagePushOptions{Name: "img", SourceFiles: dockerfileOnly, NoWait: true})
		require.NoError(t, err)
		plain := store.header
		cp.respond("POST", "/v1/sandboxes/images", 202, `{"name":"img","status":"UPLOADING","upload_url":"`+store.URL+`",
			"upload_headers":{"x-signed-memory":"2048","X-Signed-Other":"a, b"}}`)
		_, err = images.Push(t.Context(), ImagePushOptions{Name: "img", SourceFiles: dockerfileOnly, Memory: 2048, NoWait: true})
		require.NoError(t, err)
		// Exactly the returned headers are added to the upload, with their
		// values as returned, and Content-Type is kept.
		added := map[string][]string{}
		for key, values := range store.header {
			if _, ok := plain[key]; !ok {
				added[key] = values
			}
		}
		require.Equal(t, 2, len(added))
		require.Equal(t, "2048", store.header.Get("X-Signed-Memory"))
		require.Equal(t, "a, b", store.header.Get("X-Signed-Other"))
		require.Equal(t, "application/zip", store.header.Get("Content-Type"))
	})

	t.Run("ExactlyOneSource", func(t *testing.T) {
		cp := newControlPlane(t)
		images := cp.client(t, ClientOptions{}).Images()
		for _, opts := range []ImagePushOptions{
			{Name: "img"},
			{Name: "img", SourceFiles: dockerfileOnly, SourceDirectory: "."},
			{Name: "img", SourceRegistry: &ImageRegistrySource{}},
		} {
			_, err := images.Push(t.Context(), opts)
			require.Error(t, err)
		}
		// Nothing was sent for a bad source.
		require.Len(t, cp.requests, 0)
	})

	t.Run("UploadError", func(t *testing.T) {
		cp := newControlPlane(t)
		store := newStorage(t, http.StatusForbidden)
		cp.respond("POST", "/v1/sandboxes/images", 202, `{"name":"img","status":"UPLOADING","upload_url":"`+store.URL+`"}`)
		_, err := cp.client(t, ClientOptions{}).Images().Push(t.Context(), fast)
		uploadErr := require.ErrorAs[*ImageUploadError](t, err)
		require.Equal(t, http.StatusForbidden, uploadErr.Status)
		require.Equal(t, "<Error>denied</Error>", uploadErr.Body)
	})

	t.Run("BuildFailed", func(t *testing.T) {
		cp := newControlPlane(t)
		store := newStorage(t, http.StatusOK)
		cp.respond("POST", "/v1/sandboxes/images", 202, `{"name":"img","status":"UPLOADING","upload_url":"`+store.URL+`"}`)
		cp.respond("GET", "/v1/sandboxes/images/img", 200, image("BUILDING"))
		cp.respond("GET", "/v1/sandboxes/images/img", 200, image("FAILED"))
		_, err := cp.client(t, ClientOptions{}).Images().Push(t.Context(), fast)
		buildErr := require.ErrorAs[*ImageBuildError](t, err)
		require.Equal(t, ImageStatusFailed, buildErr.Status)
		require.False(t, buildErr.TimedOut, "a failed build is not a timeout")
	})

	t.Run("DirectorySpoolsToRemovedTempFile", func(t *testing.T) {
		source := dockerfileDir(t)
		tempDir := setTempDir(t, t.TempDir())
		for _, status := range []int{http.StatusOK, http.StatusForbidden} {
			cp := newControlPlane(t)
			store := newStorage(t, status)
			cp.respond("POST", "/v1/sandboxes/images", 202, `{"name":"img","status":"UPLOADING","upload_url":"`+store.URL+`"}`)
			_, err := cp.client(t, ClientOptions{}).Images().Push(t.Context(), ImagePushOptions{Name: "img", SourceDirectory: source, NoWait: true})
			if status == http.StatusOK {
				require.NoError(t, err)
			} else {
				require.ErrorAs[*ImageUploadError](t, err)
			}
			// Storage needs the length up front, and the temporary file is
			// gone whether or not the upload worked.
			require.Equal(t, int64(len(store.body)), store.contentLength)
			contents, _ := readZip(t, store.body)
			require.MapEqual(t, contents, "Dockerfile", "FROM debian\n")
			left, err := os.ReadDir(tempDir)
			require.NoError(t, err)
			require.Len(t, left, 0)
		}
	})

	t.Run("NoTempFileZipsInMemory", func(t *testing.T) {
		// A temporary directory that does not exist fails a spooled push, so
		// a push that works did not spool.
		source := dockerfileDir(t)
		setTempDir(t, filepath.Join(t.TempDir(), "missing"))
		cp := newControlPlane(t)
		images := cp.client(t, ClientOptions{}).Images()
		_, err := images.Push(t.Context(), ImagePushOptions{Name: "img", SourceDirectory: source, NoWait: true})
		require.Error(t, err)
		require.Len(t, cp.requests, 0)
		store := newStorage(t, http.StatusOK)
		cp.respond("POST", "/v1/sandboxes/images", 202, `{"name":"img","status":"UPLOADING","upload_url":"`+store.URL+`"}`)
		_, err = images.Push(t.Context(), ImagePushOptions{Name: "img", SourceDirectory: source, NoWait: true, NoTempFile: true})
		require.NoError(t, err)
		require.Equal(t, int64(len(store.body)), store.contentLength)
	})
}

// setTempDir points os.TempDir at dir for the test, returning dir.
func setTempDir(t *testing.T, dir string) string {
	t.Helper()
	// TMPDIR on Unix, TMP and TEMP on Windows.
	t.Setenv("TMPDIR", dir)
	t.Setenv("TMP", dir)
	t.Setenv("TEMP", dir)
	return dir
}

// dockerfileDir creates a directory with only a Dockerfile.
func dockerfileDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM debian\n"), 0o644))
	return dir
}

func TestImageWaitBuilt(t *testing.T) {
	t.Run("AcceptsCurrentStatus", func(t *testing.T) {
		cp := newControlPlane(t)
		cp.respond("GET", "/v1/sandboxes/images/img", 200, image("BUILT"))
		info, err := cp.client(t, ClientOptions{}).Images().WaitBuilt(t.Context(), ImageWaitBuiltOptions{Name: "img"})
		require.NoError(t, err)
		require.Equal(t, ImageStatusBuilt, info.Status)
	})

	t.Run("TimesOut", func(t *testing.T) {
		cp := newControlPlane(t)
		for range 1000 {
			cp.respond("GET", "/v1/sandboxes/images/img", 200, image("BUILDING"))
		}
		_, err := cp.client(t, ClientOptions{}).Images().WaitBuilt(t.Context(), ImageWaitBuiltOptions{
			Name:         "img",
			Timeout:      50 * time.Millisecond,
			PollInterval: time.Millisecond,
		})
		buildErr := require.ErrorAs[*ImageBuildError](t, err)
		require.True(t, buildErr.TimedOut, "expected a timeout")
		require.Equal(t, ImageStatusBuilding, buildErr.Status)
	})

	t.Run("NegativeTimeoutWaitsWithNoLimit", func(t *testing.T) {
		cp := newControlPlane(t)
		cp.respond("GET", "/v1/sandboxes/images/img", 200, image("BUILDING"))
		cp.respond("GET", "/v1/sandboxes/images/img", 200, image("BUILT"))
		info, err := cp.client(t, ClientOptions{}).Images().WaitBuilt(t.Context(), ImageWaitBuiltOptions{
			Name:         "img",
			Timeout:      -1,
			PollInterval: time.Millisecond,
		})
		require.NoError(t, err)
		require.Equal(t, ImageStatusBuilt, info.Status)
	})

	t.Run("CallerCancelIsNotTimeout", func(t *testing.T) {
		cp := newControlPlane(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := cp.client(t, ClientOptions{}).Images().WaitBuilt(ctx, ImageWaitBuiltOptions{Name: "img"})
		require.True(t, errors.Is(err, context.Canceled), "expected context.Canceled, got %v", err)
	})

	t.Run("OtherErrorsEndWait", func(t *testing.T) {
		cp := newControlPlane(t)
		cp.respond("GET", "/v1/sandboxes/images/img", 403, `{"code":"FORBIDDEN","message":"no"}`)
		_, err := cp.client(t, ClientOptions{}).Images().WaitBuilt(t.Context(), ImageWaitBuiltOptions{Name: "img"})
		apiErr := require.ErrorAs[*APIError](t, err)
		require.Equal(t, 403, apiErr.Status)
	})
}

func TestImageListLibrary(t *testing.T) {
	cp := newControlPlane(t)
	cp.respond("GET", "/v1/sandboxes/library_images", 200, `{"items":[
		{"name":"base-image","image":"baseten/base-image:latest","memory":4096,
		 "ports":[{"name":"sandbox-api","protocol":"HTTP","target":8080}],
		 "creation_options":{"extra_args":{"kernel":"x"},"volumes":[{"name":"scratch","mount_path":"/mnt/data","type":"ephemeral","size":10240}]}},
		{"name":"plain","image":"baseten/plain:latest"}
	]}`)
	images, err := cp.client(t, ClientOptions{}).Images().ListLibrary(t.Context(), ImageListLibraryOptions{})
	require.NoError(t, err)
	require.Len(t, images, 2)
	base := images[0]
	require.Equal(t, "baseten/base-image:latest", base.Image)
	require.Equal(t, Port{Target: 8080, Name: "sandbox-api", Protocol: PortProtocolHTTP}, base.Ports[0])
	require.MapEqual(t, base.CreationExtraArgs, "kernel", "x")
	require.Equal(t, ImageLibraryVolume{Name: "scratch", MountPath: "/mnt/data", Type: "ephemeral", SizeMegabytes: 10240}, base.CreationVolumes[0])
	require.Len(t, images[1].CreationVolumes, 0)
}

func TestImageCleanup(t *testing.T) {
	t.Run("CallsUntilNoneRemain", func(t *testing.T) {
		cp := newControlPlane(t)
		cp.respond("POST", "/v1/sandboxes/cleanup_images", 200, `{"deleted":3,"message":"first","remaining":4,"has_more":true}`)
		cp.respond("POST", "/v1/sandboxes/cleanup_images", 200, `{"deleted":2,"message":"second","remaining":2,"has_more":true}`)
		cp.respond("POST", "/v1/sandboxes/cleanup_images", 200, `{"deleted":2,"message":"last","remaining":0,"has_more":false}`)
		var progress []ImageCleanupProgress
		result, err := cp.client(t, ClientOptions{}).Images().Cleanup(t.Context(), ImageCleanupOptions{
			Progress: func(p ImageCleanupProgress) { progress = append(progress, p) },
		})
		require.NoError(t, err)
		require.Equal(t, ImageCleanupResult{Deleted: 7, Message: "last"}, *result)
		// Progress is reported between calls only, not after the last.
		require.Len(t, progress, 2)
		require.Equal(t, ImageCleanupProgress{Deleted: 3, Remaining: 4, Message: "first"}, progress[0])
		require.Equal(t, ImageCleanupProgress{Deleted: 5, Remaining: 2, Message: "second"}, progress[1])
	})

	t.Run("SingleCallWithoutHasMore", func(t *testing.T) {
		cp := newControlPlane(t)
		cp.respond("POST", "/v1/sandboxes/cleanup_images", 200, `{"deleted":1,"message":"done"}`)
		result, err := cp.client(t, ClientOptions{}).Images().Cleanup(t.Context(), ImageCleanupOptions{
			Progress: func(ImageCleanupProgress) { t.Error("progress called without a stop") },
		})
		require.NoError(t, err)
		require.Equal(t, ImageCleanupResult{Deleted: 1, Message: "done"}, *result)
		require.Len(t, cp.requests, 1)
	})

	t.Run("ErrorMidwayFails", func(t *testing.T) {
		cp := newControlPlane(t)
		cp.respond("POST", "/v1/sandboxes/cleanup_images", 200, `{"deleted":3,"message":"first","remaining":4,"has_more":true}`)
		cp.respond("POST", "/v1/sandboxes/cleanup_images", 500, `{"code":"INTERNAL_ERROR","message":"boom"}`)
		_, err := cp.client(t, ClientOptions{}).Images().Cleanup(t.Context(), ImageCleanupOptions{})
		apiErr := require.ErrorAs[*APIError](t, err)
		require.Equal(t, 500, apiErr.Status)
	})
}

func TestImageLogs(t *testing.T) {
	cp := newControlPlane(t)
	page := func(from, count int) string {
		body := `{"total_count":1500,"logs":[`
		for i := range count {
			if i > 0 {
				body += ","
			}
			body += `{"timestamp":"2026-10-01T00:00:00Z","severity":1,"message":"line ` + strconv.Itoa(from-i) + `"}`
		}
		return body + `]}`
	}
	// Newest first, as the endpoint sends them.
	cp.respond("GET", "/v1/sandboxes/images/img/logs", 200, page(1500, 1000))
	cp.respond("GET", "/v1/sandboxes/images/img/logs", 200, page(500, 500))
	lines, err := cp.client(t, ClientOptions{}).Images().Logs(t.Context(), ImageLogsOptions{Name: "img"})
	require.NoError(t, err)
	require.Len(t, lines, 1500)
	require.Equal(t, "line 1", lines[0].Text)
	require.Equal(t, "line 1500", lines[1499].Text)
	require.Len(t, cp.requests, 2)
	first, err := url.ParseQuery(cp.requests[0].query)
	require.NoError(t, err)
	second, err := url.ParseQuery(cp.requests[1].query)
	require.NoError(t, err)
	require.Equal(t, "1000", second.Get("offset"))
	// The end of the range is pinned across pages.
	require.NotEqual(t, "", first.Get("end_time"))
	require.Equal(t, first.Get("end_time"), second.Get("end_time"))
}
