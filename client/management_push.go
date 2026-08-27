package client

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/basetenlabs/baseten-go/client/managementapi"
	"github.com/basetenlabs/baseten-go/client/modelarchive"
)

// PushModelOptions configures [ManagementClient.PushModel].
type PushModelOptions struct {
	// ModelID is an existing model to add a deployment to. When empty, a new
	// model is created, named by the `model_name` key of Config.
	ModelID string

	// TeamID is the team the new model belongs to. Only valid when creating a
	// model (ModelID empty); defaults to the organization's default team.
	TeamID string

	// Config is the parsed model config. Required, and must carry a non-empty
	// `model_name` when creating a model.
	Config map[string]any

	// RawConfig is the original config.yaml text, persisted verbatim on the
	// deployment for display and download. The server never builds from it, so
	// it may keep comments and formatting the parsed Config has lost. Optional.
	RawConfig string

	// Archive describes the tar to upload. Archive.Dir is required, and at
	// minimum holds the model's config.yaml.
	//
	// This package does not parse or serialize YAML, so callers whose config
	// has `external_package_dirs` must handle the coupling between the config
	// and the archive themselves: copy the value into
	// Archive.ExternalPackageDirs, set Archive.BundledPackagesDir from the
	// config's `bundled_packages_dir` (canonically "packages"), remove
	// `external_package_dirs` from Config, and set Archive.ConfigYAMLOverride
	// to that cleared config. The external directories are inlined into the
	// archive under the bundled packages dir, so a config that still names
	// them fails server-side validation on relative paths the extracted
	// archive does not contain.
	Archive modelarchive.BuildModelArchiveOptions

	// ModelUploader uploads the built archive to the S3 location the server
	// issued credentials for. Required unless DryRun is set. Callers supply
	// this so this package takes no dependency on an AWS SDK.
	//
	// It is not called for model formats built without an archive (for example
	// BIS-LLM), where the server issues no upload target.
	ModelUploader ModelUploader

	// DryRun validates the push without creating anything: the config is
	// checked server-side and the archive is built and read to completion, but
	// never uploaded. Building it exercises the ignore rules, the external
	// package dirs, and the archive paths, none of which the server can see.
	// The returned result carries ArchiveBytes but no model or deployment.
	DryRun bool

	// DeploymentName is an optional human-readable name for the deployment.
	// Names are unique per model, so a literal name can be pushed only once;
	// leave empty to let the server assign `deployment-N`.
	DeploymentName string

	// EnvironmentName is the stable environment to push to (for example
	// "production"). The environment is created if it does not exist. When
	// empty, the deployment is created without environment selection.
	EnvironmentName string

	// Labels are user-provided key-value labels for the deployment. They can
	// only be set at creation.
	Labels map[string]any

	// DeployTimeoutMinutes overrides the server's deploy timeout. Allowed
	// range is 10 to 1440; zero leaves the server default.
	DeployTimeoutMinutes int

	// OverrideEnvInstanceType replaces the target environment's current
	// instance type with the one in Config, instead of retaining the
	// environment's. Only meaningful with EnvironmentName set.
	OverrideEnvInstanceType bool

	// IsDevelopment pushes to the model's single mutable development slot,
	// created if absent and overwritten in place otherwise. DeploymentName,
	// EnvironmentName, and OverrideEnvInstanceType must be left unset.
	IsDevelopment bool

	// UserEnv is client environment metadata (for example client version,
	// Python version), validated server-side.
	UserEnv map[string]any

	// DisableArchiveDownload prevents the uploaded archive from being
	// downloadable later. Only valid when creating a model.
	DisableArchiveDownload bool
}

// Validate reports whether the options describe a push that can be attempted,
// including the archive's own preconditions. [ManagementClient.PushModel] calls
// it before its first request, so a bad push fails before the server issues
// upload credentials.
func (o PushModelOptions) Validate() error {
	if o.Config == nil {
		return errors.New("Config is required")
	}
	if o.ModelID == "" {
		if name, _ := o.Config["model_name"].(string); name == "" {
			return errors.New("Config must have a non-empty model_name to create a model")
		}
	} else {
		if o.TeamID != "" {
			return errors.New("TeamID is only valid when creating a model")
		}
		if o.DisableArchiveDownload {
			return errors.New("DisableArchiveDownload is only valid when creating a model")
		}
	}
	if o.ModelUploader == nil && !o.DryRun {
		return errors.New("ModelUploader is required unless DryRun is set")
	}
	return o.Archive.Validate()
}

// ModelUploader uploads a model archive to the location the server issued
// credentials for.
type ModelUploader func(context.Context, ModelUpload) error

// ModelUpload is the archive and destination handed to a [ModelUploader].
type ModelUpload struct {
	// Bucket is the destination S3 bucket.
	Bucket string
	// Key is the destination S3 key.
	Key string
	// Region is the AWS region the bucket resides in.
	Region string

	// AccessKeyID, SecretAccessKey, and SessionToken are short-lived STS
	// credentials scoped to Bucket and Key.
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string

	// Body streams the tar archive. It is produced lazily as it is read, so
	// read errors may surface from the walk of the model directory.
	Body io.Reader
}

// PushModelResult is the outcome of [ManagementClient.PushModel].
type PushModelResult struct {
	// Model is the model the deployment belongs to, whether newly created or
	// already existing. Nil for a dry run.
	Model *managementapi.Model

	// Deployment is the created deployment. It has only just been created, so
	// its status is an in-progress one; callers that need it live must poll.
	// Nil for a dry run.
	Deployment *managementapi.Deployment

	// ModelCreated reports whether the push created the model rather than
	// adding a deployment to an existing one.
	ModelCreated bool

	// ArchiveBytes is the size of the built archive: the bytes read by the
	// uploader, or by the dry run that discarded it. Zero for a push of a
	// format built without an archive, which never builds one.
	ArchiveBytes int64
}

// PushModel deploys a model from a local directory: it validates the push and
// obtains upload credentials via `POST /v1/prepare_model_upload`, builds and
// uploads the archive through opts.ModelUploader, then creates the model (`POST
// /v1/models`) or adds a deployment to an existing one (`POST
// /v1/models/{model_id}/deployments`).
//
// The returned deployment is not yet live. Poll it to wait for a terminal
// status.
//
// Set opts.DryRun to validate a push without creating anything.
func (c *ManagementClient) PushModel(ctx context.Context, opts PushModelOptions) (*PushModelResult, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}

	// The archive is enumerated before the push is prepared, so a source tree
	// the archive cannot carry fails before the server has created anything.
	// Its contents are still read lazily, so building it up front costs a
	// directory walk and no more.
	archive, err := modelarchive.BuildModelArchive(ctx, opts.Archive)
	if err != nil {
		return nil, fmt.Errorf("build model archive: %w", err)
	}
	defer archive.Close()

	prepare, err := c.prepareModelUpload(ctx, opts)
	if err != nil {
		return nil, err
	}
	result := &PushModelResult{ModelCreated: opts.ModelID == ""}
	if opts.DryRun {
		// A dry-run prepare nulls the upload target unconditionally, so there
		// is no way to tell an archive-less format from a normal one here and
		// the archive is read either way. Reading it to nowhere is the point:
		// it opens every file the push would upload, so a dry run catches the
		// archive errors a server-side config check cannot see.
		if result.ArchiveBytes, err = io.Copy(io.Discard, archive); err != nil {
			return nil, fmt.Errorf("build model archive: %w", err)
		}
		return result, nil
	}

	// Model formats not built from an uploaded archive (for example BIS-LLM)
	// get no upload target back from prepare, and the server builds them from
	// the config alone. Everything else uploads before committing, since the
	// commit references the key.
	if prepare.S3Key != nil {
		uploaded, err := c.uploadModelArchive(ctx, opts, prepare, archive)
		if err != nil {
			return nil, err
		}
		result.ArchiveBytes = uploaded
	}

	created, err := c.commitModelPush(ctx, opts, prepare.S3Key)
	if err != nil {
		return nil, err
	}
	result.Model, result.Deployment = &created.Model, &created.Deployment
	return result, nil
}

// prepareModelUpload validates the push server-side and, unless this is a dry
// run, obtains the credentials for the archive upload. The response routes the
// push: a nil S3Key means the format is built without an archive.
func (c *ManagementClient) prepareModelUpload(
	ctx context.Context,
	opts PushModelOptions,
) (*managementapi.PrepareModelUploadResponse, error) {
	req := managementapi.PrepareModelUploadRequest{
		Deployment: pushModelPayload(opts),
		DryRun:     &opts.DryRun,
	}
	if opts.ModelID != "" {
		req.ModelId = &opts.ModelID
	} else {
		// Validate has already established a non-empty model_name.
		name, _ := opts.Config["model_name"].(string)
		req.Name = &name
		if opts.TeamID != "" {
			req.TeamId = &opts.TeamID
		}
	}

	prepare, err := c.api.PostPrepareModelUpload(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("prepare model upload: %w", err)
	}
	return prepare, nil
}

// pushModelPayload builds the deployment payload shared by the prepare and
// commit requests.
func pushModelPayload(opts PushModelOptions) managementapi.DeploymentArchivePayload {
	payload := managementapi.DeploymentArchivePayload{Config: opts.Config}
	if opts.RawConfig != "" {
		payload.RawConfig = &opts.RawConfig
	}
	if opts.DeploymentName != "" {
		payload.DeploymentName = &opts.DeploymentName
	}
	if opts.EnvironmentName != "" {
		payload.EnvironmentName = &opts.EnvironmentName
		// The server rejects the field without an environment, and defaults it
		// to true, so it is only sent to opt out.
		preserve := !opts.OverrideEnvInstanceType
		payload.PreserveEnvInstanceType = &preserve
	}
	if opts.Labels != nil {
		payload.Labels = &opts.Labels
	}
	if opts.DeployTimeoutMinutes != 0 {
		payload.DeployTimeoutMinutes = &opts.DeployTimeoutMinutes
	}
	if opts.IsDevelopment {
		payload.IsDevelopment = &opts.IsDevelopment
	}
	if opts.UserEnv != nil {
		payload.UserEnv = &opts.UserEnv
	}
	return payload
}

// uploadModelArchive hands the archive to the caller's uploader, returning the
// number of bytes the uploader read.
func (c *ManagementClient) uploadModelArchive(
	ctx context.Context,
	opts PushModelOptions,
	prepare *managementapi.PrepareModelUploadResponse,
	archive io.Reader,
) (int64, error) {
	if prepare.Creds == nil || prepare.S3Bucket == nil || prepare.S3Region == nil {
		return 0, errors.New("prepare model upload: server issued an upload key without credentials")
	}

	// Counting on the way through reports the archive size without buffering
	// the stream, whose contents are read lazily as the uploader consumes it.
	counted := &countingReader{r: archive}
	if err := opts.ModelUploader(ctx, ModelUpload{
		Bucket:          *prepare.S3Bucket,
		Key:             *prepare.S3Key,
		Region:          *prepare.S3Region,
		AccessKeyID:     prepare.Creds.AwsAccessKeyId,
		SecretAccessKey: prepare.Creds.AwsSecretAccessKey,
		SessionToken:    prepare.Creds.AwsSessionToken,
		Body:            counted,
	}); err != nil {
		return 0, fmt.Errorf("upload model archive: %w", err)
	}
	return counted.n, nil
}

// commitModelPush creates the model, or adds a deployment to the existing one,
// from the archive at s3Key.
func (c *ManagementClient) commitModelPush(
	ctx context.Context,
	opts PushModelOptions,
	s3Key *string,
) (*managementapi.CreatedModelDeployment, error) {
	payload := pushModelPayload(opts)

	if opts.ModelID != "" {
		var source managementapi.CreateModelDeploymentRequest_Source
		if err := source.FromDeploymentArchiveSource(managementapi.DeploymentArchiveSource{
			S3Key:      s3Key,
			Deployment: payload,
		}); err != nil {
			return nil, err
		}
		created, err := c.api.PostModelsDeployments(ctx, opts.ModelID,
			managementapi.CreateModelDeploymentRequest{Source: source})
		if err != nil {
			return nil, fmt.Errorf("create deployment: %w", err)
		}
		return created, nil
	}

	name, _ := opts.Config["model_name"].(string)
	archiveSource := managementapi.ModelArchiveSource{
		Name:       name,
		S3Key:      s3Key,
		Deployment: payload,
	}
	if opts.DisableArchiveDownload {
		archiveSource.DisableArchiveDownload = &opts.DisableArchiveDownload
	}
	var source managementapi.CreateModelRequest_Source
	if err := source.FromModelArchiveSource(archiveSource); err != nil {
		return nil, err
	}
	req := managementapi.CreateModelRequest{Source: source}
	if opts.TeamID != "" {
		created, err := c.api.PostTeamsModels(ctx, opts.TeamID, req)
		if err != nil {
			return nil, fmt.Errorf("create model: %w", err)
		}
		return created, nil
	}
	created, err := c.api.PostModels(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("create model: %w", err)
	}
	return created, nil
}

// countingReader counts the bytes read through it.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
