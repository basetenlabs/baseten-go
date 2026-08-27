package client_test

import (
	"archive/tar"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/basetenlabs/baseten-go/client"
	"github.com/basetenlabs/baseten-go/client/modelarchive"
	"github.com/basetenlabs/baseten-go/internal/require"
)

// pushTestServer records every request it receives and answers the two push
// endpoints from route-keyed canned responses, since a push always calls
// prepare and then a commit endpoint.
type pushTestServer struct {
	requests  []requestCapture
	responses map[string]any
}

func newPushTestServer(t *testing.T, srv *pushTestServer) *client.ManagementClient {
	t.Helper()
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		srv.requests = append(srv.requests, requestCapture{
			Method: r.Method,
			Path:   r.URL.Path,
			Query:  r.URL.Query(),
			Header: r.Header,
			Body:   string(body),
		})

		response, ok := srv.responses[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))
	t.Cleanup(httpSrv.Close)

	cl, err := client.NewManagementClient(client.ManagementClientOptions{
		APIKey:     "test-key",
		BaseURL:    httpSrv.URL,
		HTTPClient: httpSrv.Client(),
	})
	require.NoError(t, err)
	return cl
}

// pushPrepareResponse is the prepare response for a normal archive push.
var pushPrepareResponse = map[string]any{
	"s3_bucket": "test-bucket",
	"s3_key":    "models/abc123.tar",
	"s3_region": "us-west-2",
	"creds": map[string]any{
		"aws_access_key_id":     "AKIA",
		"aws_secret_access_key": "secret",
		"aws_session_token":     "token",
	},
}

// pushCommitResponse is the created model and deployment both commit endpoints
// return.
var pushCommitResponse = map[string]any{
	"model": map[string]any{
		"id": "model-1", "name": "my-model",
		"created_at": "2024-01-01T00:00:00Z", "deployments_count": 1,
	},
	"deployment": map[string]any{
		"id": "deployment-1", "name": "deployment-1", "model_id": "model-1",
		"created_at": "2024-01-01T00:00:00Z", "status": "BUILDING",
	},
}

// newPushModelDir writes a minimal model directory: a config.yaml plus one
// source file, so a built archive has something beyond the config in it.
func newPushModelDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"),
		[]byte("model_name: my-model\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "model.py"),
		[]byte("class Model: pass\n"), 0o644))
	return dir
}

// requestByPath returns the single captured request for path, failing if the
// path was not requested exactly once.
func requestByPath(t *testing.T, srv *pushTestServer, path string) requestCapture {
	t.Helper()
	var found []requestCapture
	for _, req := range srv.requests {
		if req.Path == path {
			found = append(found, req)
		}
	}
	require.Len(t, found, 1)
	return found[0]
}

// pushRequestBody decodes a captured request body as JSON.
func pushRequestBody(t *testing.T, req requestCapture) map[string]any {
	t.Helper()
	var body map[string]any
	require.NoError(t, json.Unmarshal([]byte(req.Body), &body))
	return body
}

// collectUpload drains an upload body into the tar entry names it carries, so
// tests can assert on archive contents without holding files in memory.
func collectUpload(t *testing.T, upload client.ModelUpload) (names []string, size int64) {
	t.Helper()
	counted := &countingWriter{}
	reader := tar.NewReader(io.TeeReader(upload.Body, counted))
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		names = append(names, header.Name)
	}
	return names, counted.n
}

type countingWriter struct{ n int64 }

func (c *countingWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

func TestPushModelValidation(t *testing.T) {
	dir := newPushModelDir(t)
	uploader := func(context.Context, client.ModelUpload) error { return nil }
	valid := client.PushModelOptions{
		Config:        map[string]any{"model_name": "my-model"},
		Archive:       modelarchive.BuildModelArchiveOptions{Dir: dir},
		ModelUploader: uploader,
	}

	// Each case mutates a copy of the valid options into an invalid one. None
	// of these reach the network, so the server has no routes.
	cases := map[string]func(*client.PushModelOptions){
		"NoConfig":                    func(o *client.PushModelOptions) { o.Config = nil },
		"NoDir":                       func(o *client.PushModelOptions) { o.Archive.Dir = "" },
		"NoUploader":                  func(o *client.PushModelOptions) { o.ModelUploader = nil },
		"NoModelName":                 func(o *client.PushModelOptions) { o.Config = map[string]any{} },
		"EmptyModelName":              func(o *client.PushModelOptions) { o.Config = map[string]any{"model_name": ""} },
		"TeamIDWithModelID":           func(o *client.PushModelOptions) { o.ModelID, o.TeamID = "model-1", "team-1" },
		"ArchiveDownloadWithModelID":  func(o *client.PushModelOptions) { o.ModelID, o.DisableArchiveDownload = "model-1", true },
		"MissingDir":                  func(o *client.PushModelOptions) { o.Archive.Dir = filepath.Join(dir, "nope") },
		"ExternalDirsNoBundledPkgDir": func(o *client.PushModelOptions) { o.Archive.ExternalPackageDirs = []string{"lib"} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			srv := &pushTestServer{responses: map[string]any{}}
			cl := newPushTestServer(t, srv)
			opts := valid
			mutate(&opts)
			_, err := cl.PushModel(context.Background(), opts)
			require.Error(t, err)
			require.Len(t, srv.requests, 0)
		})
	}
}

func TestPushModelCreate(t *testing.T) {
	srv := &pushTestServer{responses: map[string]any{
		"/v1/prepare_model_upload": pushPrepareResponse,
		"/v1/models":               pushCommitResponse,
	}}
	cl := newPushTestServer(t, srv)

	var uploads []client.ModelUpload
	var uploadedNames []string
	var uploadedSize int64
	result, err := cl.PushModel(context.Background(), client.PushModelOptions{
		Config:    map[string]any{"model_name": "my-model"},
		RawConfig: "model_name: my-model\n",
		Archive:   modelarchive.BuildModelArchiveOptions{Dir: newPushModelDir(t)},
		ModelUploader: func(_ context.Context, upload client.ModelUpload) error {
			uploads = append(uploads, upload)
			uploadedNames, uploadedSize = collectUpload(t, upload)
			return nil
		},
	})
	require.NoError(t, err)
	require.True(t, result.ModelCreated, "expected a created model")
	require.NotNil(t, result.Model)
	require.Equal(t, "model-1", result.Model.Id)
	require.NotNil(t, result.Deployment)
	require.Equal(t, "deployment-1", result.Deployment.Id)
	require.Equal(t, uploadedSize, result.ArchiveBytes)

	// The prepare call routes by name, not ID, and is not a dry run.
	prepare := pushRequestBody(t, requestByPath(t, srv, "/v1/prepare_model_upload"))
	require.Equal(t, "my-model", prepare["name"])
	require.Equal(t, false, prepare["dry_run"])
	require.Nil(t, prepare["model_id"])
	deployment, ok := prepare["deployment"].(map[string]any)
	require.True(t, ok, "expected a deployment payload, got %T", prepare["deployment"])
	require.Equal(t, "model_name: my-model\n", deployment["raw_config"])

	// The uploader receives the credentials prepare issued.
	require.Len(t, uploads, 1)
	require.Equal(t, "test-bucket", uploads[0].Bucket)
	require.Equal(t, "models/abc123.tar", uploads[0].Key)
	require.Equal(t, "us-west-2", uploads[0].Region)
	require.Equal(t, "AKIA", uploads[0].AccessKeyID)
	require.Equal(t, "secret", uploads[0].SecretAccessKey)
	require.Equal(t, "token", uploads[0].SessionToken)
	require.Len(t, uploadedNames, 2)

	// The commit references the key prepare issued.
	commit := pushRequestBody(t, requestByPath(t, srv, "/v1/models"))
	source, ok := commit["source"].(map[string]any)
	require.True(t, ok, "expected a source, got %T", commit["source"])
	require.Equal(t, "my-model", source["name"])
	require.Equal(t, "models/abc123.tar", source["s3_key"])
	require.Nil(t, source["disable_archive_download"])
}

func TestPushModelCreateInTeam(t *testing.T) {
	srv := &pushTestServer{responses: map[string]any{
		"/v1/prepare_model_upload": pushPrepareResponse,
		"/v1/teams/team-1/models":  pushCommitResponse,
	}}
	cl := newPushTestServer(t, srv)

	result, err := cl.PushModel(context.Background(), client.PushModelOptions{
		TeamID:                 "team-1",
		Config:                 map[string]any{"model_name": "my-model"},
		Archive:                modelarchive.BuildModelArchiveOptions{Dir: newPushModelDir(t)},
		DisableArchiveDownload: true,
		ModelUploader:          func(_ context.Context, u client.ModelUpload) error { _, _ = collectUpload(t, u); return nil },
	})
	require.NoError(t, err)
	require.True(t, result.ModelCreated, "expected a created model")

	prepare := pushRequestBody(t, requestByPath(t, srv, "/v1/prepare_model_upload"))
	require.Equal(t, "team-1", prepare["team_id"])

	commit := pushRequestBody(t, requestByPath(t, srv, "/v1/teams/team-1/models"))
	source, ok := commit["source"].(map[string]any)
	require.True(t, ok, "expected a source, got %T", commit["source"])
	require.Equal(t, true, source["disable_archive_download"])
}

func TestPushModelExisting(t *testing.T) {
	srv := &pushTestServer{responses: map[string]any{
		"/v1/prepare_model_upload":       pushPrepareResponse,
		"/v1/models/model-1/deployments": pushCommitResponse,
	}}
	cl := newPushTestServer(t, srv)

	result, err := cl.PushModel(context.Background(), client.PushModelOptions{
		ModelID:        "model-1",
		Config:         map[string]any{"model_name": "my-model"},
		Archive:        modelarchive.BuildModelArchiveOptions{Dir: newPushModelDir(t)},
		DeploymentName: "v2",
		Region:         "us-west-2",
		Labels:         map[string]any{"team": "core"},
		ModelUploader:  func(_ context.Context, u client.ModelUpload) error { _, _ = collectUpload(t, u); return nil },
	})
	require.NoError(t, err)
	require.False(t, result.ModelCreated, "expected an adopted model")

	// Routing by ID must not also send a name or a team.
	prepare := pushRequestBody(t, requestByPath(t, srv, "/v1/prepare_model_upload"))
	require.Equal(t, "model-1", prepare["model_id"])
	require.Nil(t, prepare["name"])
	require.Nil(t, prepare["team_id"])

	commit := pushRequestBody(t, requestByPath(t, srv, "/v1/models/model-1/deployments"))
	source, ok := commit["source"].(map[string]any)
	require.True(t, ok, "expected a source, got %T", commit["source"])
	require.Equal(t, "models/abc123.tar", source["s3_key"])
	deployment, ok := source["deployment"].(map[string]any)
	require.True(t, ok, "expected a deployment payload, got %T", source["deployment"])
	require.Equal(t, "v2", deployment["deployment_name"])
	require.Equal(t, "us-west-2", deployment["region"])
	labels, ok := deployment["labels"].(map[string]any)
	require.True(t, ok, "expected labels, got %T", deployment["labels"])
	require.MapEqual(t, labels, "team", any("core"))
}

func TestPushModelEnvironment(t *testing.T) {
	// preserve_env_instance_type is only meaningful alongside an environment,
	// and is sent inverted from the caller's override intent.
	cases := map[string]struct {
		environment string
		override    bool
		expected    any
	}{
		"NoEnvironmentOmitsPreserve": {environment: "", override: false, expected: nil},
		"EnvironmentPreserves":       {environment: "production", override: false, expected: true},
		"EnvironmentOverrides":       {environment: "production", override: true, expected: false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv := &pushTestServer{responses: map[string]any{
				"/v1/prepare_model_upload": pushPrepareResponse,
				"/v1/models":               pushCommitResponse,
			}}
			cl := newPushTestServer(t, srv)

			_, err := cl.PushModel(context.Background(), client.PushModelOptions{
				Config:                  map[string]any{"model_name": "my-model"},
				Archive:                 modelarchive.BuildModelArchiveOptions{Dir: newPushModelDir(t)},
				EnvironmentName:         tc.environment,
				OverrideEnvInstanceType: tc.override,
				ModelUploader:           func(_ context.Context, u client.ModelUpload) error { _, _ = collectUpload(t, u); return nil },
			})
			require.NoError(t, err)

			prepare := pushRequestBody(t, requestByPath(t, srv, "/v1/prepare_model_upload"))
			deployment, ok := prepare["deployment"].(map[string]any)
			require.True(t, ok, "expected a deployment payload, got %T", prepare["deployment"])
			require.Equal(t, tc.expected, deployment["preserve_env_instance_type"])
			if tc.environment == "" {
				require.Nil(t, deployment["environment_name"])
			} else {
				require.Equal(t, any(tc.environment), deployment["environment_name"])
			}
		})
	}
}

func TestPushModelDryRun(t *testing.T) {
	srv := &pushTestServer{responses: map[string]any{
		// A dry-run prepare nulls the upload target. No commit route exists, so
		// a commit attempt would surface as a 404.
		"/v1/prepare_model_upload": map[string]any{},
	}}
	cl := newPushTestServer(t, srv)

	// No uploader: a dry run must not need one, and must not call one.
	result, err := cl.PushModel(context.Background(), client.PushModelOptions{
		Config:  map[string]any{"model_name": "my-model"},
		Archive: modelarchive.BuildModelArchiveOptions{Dir: newPushModelDir(t)},
		DryRun:  true,
	})
	require.NoError(t, err)
	require.Nil(t, result.Model)
	require.Nil(t, result.Deployment)
	require.Len(t, srv.requests, 1)

	// The archive is still built and read, so its size is reported.
	require.True(t, result.ArchiveBytes > 0, "expected a built archive, got %d bytes", result.ArchiveBytes)

	prepare := pushRequestBody(t, requestByPath(t, srv, "/v1/prepare_model_upload"))
	require.Equal(t, true, prepare["dry_run"])
}

func TestPushModelDryRunArchiveError(t *testing.T) {
	srv := &pushTestServer{responses: map[string]any{
		"/v1/prepare_model_upload": map[string]any{},
	}}
	cl := newPushTestServer(t, srv)

	// Two sources colliding on one archive path is an error only the build can
	// find, since it emerges from the walk rather than from the options. It is
	// exactly the class of failure a dry run builds the archive to catch.
	dir := newPushModelDir(t)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "packages"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "packages", "util.py"), []byte("x\n"), 0o644))
	external := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(external, "util.py"), []byte("y\n"), 0o644))

	_, err := cl.PushModel(context.Background(), client.PushModelOptions{
		Config: map[string]any{"model_name": "my-model"},
		Archive: modelarchive.BuildModelArchiveOptions{
			Dir:                 dir,
			ExternalPackageDirs: []string{external},
			BundledPackagesDir:  "packages",
		},
		DryRun: true,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "duplicate archive entry")
}

func TestPushModelWithoutArchive(t *testing.T) {
	srv := &pushTestServer{responses: map[string]any{
		// Formats built without an archive (for example BIS-LLM) get no upload
		// target back from prepare, and the server builds from the config.
		"/v1/prepare_model_upload": map[string]any{},
		"/v1/models":               pushCommitResponse,
	}}
	cl := newPushTestServer(t, srv)

	uploaded := false
	result, err := cl.PushModel(context.Background(), client.PushModelOptions{
		Config:        map[string]any{"model_name": "my-model"},
		Archive:       modelarchive.BuildModelArchiveOptions{Dir: newPushModelDir(t)},
		ModelUploader: func(context.Context, client.ModelUpload) error { uploaded = true; return nil },
	})
	require.NoError(t, err)
	require.False(t, uploaded, "expected no upload without an upload target")
	require.Equal(t, int64(0), result.ArchiveBytes)
	require.NotNil(t, result.Deployment)

	commit := pushRequestBody(t, requestByPath(t, srv, "/v1/models"))
	source, ok := commit["source"].(map[string]any)
	require.True(t, ok, "expected a source, got %T", commit["source"])
	require.Nil(t, source["s3_key"])
}

func TestPushModelUploadError(t *testing.T) {
	srv := &pushTestServer{responses: map[string]any{
		// No commit route: a failed upload must not go on to commit.
		"/v1/prepare_model_upload": pushPrepareResponse,
	}}
	cl := newPushTestServer(t, srv)

	_, err := cl.PushModel(context.Background(), client.PushModelOptions{
		Config:  map[string]any{"model_name": "my-model"},
		Archive: modelarchive.BuildModelArchiveOptions{Dir: newPushModelDir(t)},
		ModelUploader: func(context.Context, client.ModelUpload) error {
			return io.ErrUnexpectedEOF
		},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "upload model archive")
	require.Len(t, srv.requests, 1)
}
