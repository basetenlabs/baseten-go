package sandbox

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/basetenlabs/baseten-go/client/managementapi"
)

// ImageStatus is the processing status of an image. Only
// ImageStatusBuilt images are ready to use. Other values may be added,
// so do not treat the constants as exhaustive.
type ImageStatus = string

// Values for ImageStatus.
const (
	ImageStatusUploading ImageStatus = "UPLOADING"
	ImageStatusBuilding  ImageStatus = "BUILDING"
	ImageStatusBuilt     ImageStatus = "BUILT"
	ImageStatusFailed    ImageStatus = "FAILED"
)

// ImageInfo is an image repository as last reported by the control plane.
type ImageInfo struct {
	// Name is the repository name given when pushing. To create a sandbox
	// from the image, pass "<name>:latest" as its image, or "<name>:<tag>"
	// for a specific version.
	Name string `json:"name"`

	// DisplayName is the human-readable name for display.
	DisplayName string `json:"display_name"`

	// Status is the processing status of the most recent version.
	Status ImageStatus `json:"status"`

	// CreatedAt is when the image was created.
	CreatedAt time.Time `json:"created_at"`

	// LastDeployedAt is the most recent time any version was used by a
	// sandbox. Zero means it was never used.
	LastDeployedAt time.Time `json:"last_deployed_at"`

	// SizeBytes is the total size of all versions.
	SizeBytes int64 `json:"size_bytes"`

	// TagCount is the number of versions, each with its own tag.
	TagCount int64 `json:"tag_count"`

	// UpdatedAt is when the image was last updated.
	UpdatedAt time.Time `json:"updated_at"`
}

// ImageTagInfo is one version of an image.
type ImageTagInfo struct {
	// Name is the tag, assigned by the service.
	Name string `json:"name"`

	// CreatedAt is when the tag was created.
	CreatedAt time.Time `json:"created_at"`

	// SizeBytes is the size of this version.
	SizeBytes int64 `json:"size_bytes"`
}

// ImageCleanupResult is the result of ImageClient.Cleanup.
type ImageCleanupResult struct {
	// Deleted is the number of image versions removed.
	Deleted int `json:"deleted"`

	// Message is the human-readable description of the result.
	Message string `json:"message"`
}

// ImageUploadError is a failed upload of an image's source archive. The
// upload goes to storage rather than to the API, so its error body has no
// fixed shape.
type ImageUploadError struct {
	// ImageName is the image whose source was being uploaded.
	ImageName string

	// Status is the HTTP status of the storage response.
	Status int

	// Body is the raw text of the storage response body.
	Body string
}

func (e *ImageUploadError) Error() string {
	return fmt.Sprintf(
		"uploading the source of image %s failed (HTTP %d); the image stays as it was left, so push it again or delete it",
		e.ImageName, e.Status)
}

// ImageBuildError is an image that did not become ready to use: either its
// build failed, or it was still processing when the wait ran out of time.
type ImageBuildError struct {
	// ImageName is the image.
	ImageName string

	// Status last seen, ImageStatusFailed when the build failed.
	Status ImageStatus

	// TimedOut reports whether the wait ran out of time. Processing
	// continues regardless.
	TimedOut bool

	// Logs are the recorded build log messages, newest last, fetched from
	// the build-log endpoint. Nil when the build never got there or the
	// log service did not answer; the build outcome is the primary fact,
	// so fetching logs is best-effort.
	Logs []string
}

func (e *ImageBuildError) Error() string {
	if e.TimedOut {
		return fmt.Sprintf("image %s was still %s when the wait timed out; it may still finish", e.ImageName, e.Status)
	}
	message := fmt.Sprintf("image %s failed to build (status %s)", e.ImageName, e.Status)
	if len(e.Logs) > 0 {
		const tail = 10
		logs := e.Logs
		if len(logs) > tail {
			logs = logs[len(logs)-tail:]
		}
		message += "\nlast build logs:\n" + strings.Join(logs, "\n")
	}
	return message
}

const imageWaitTimeout = 900 * time.Second
const imageWaitPollInterval = 3 * time.Second

// ImagePushOptions pushes one image. Exactly one source is given:
// Directory to zip and upload, or RegistryImage to import from a registry.
type ImagePushOptions struct {
	// Name of the image repository. Reusing a name pushes a new version to
	// the existing repository.
	Name string

	// Directory whose contents are zipped and uploaded as the image source.
	// It must hold a Dockerfile at its root.
	Directory string

	// RegistryImage is a source registry image reference including a
	// registry hostname, imported instead of building from a directory.
	RegistryImage string

	// DockerConfig is optional serialized registry authentication for
	// importing a private RegistryImage.
	DockerConfig string

	// WaitForBuilt waits for the image to become ready before returning.
	// Without it, the push returns the record as of the push.
	WaitForBuilt bool

	// TimeoutSeconds bounds WaitForBuilt. Zero uses the default of 900.
	TimeoutSeconds int

	// PollIntervalSeconds is how often WaitForBuilt checks. Zero uses the
	// default of 3.
	PollIntervalSeconds int
}

// ImageWaitOptions waits for an image to become ready.
type ImageWaitOptions struct {
	// Name of the image.
	Name string

	// TimeoutSeconds bounds the wait. Zero uses the default of 900.
	TimeoutSeconds int

	// PollIntervalSeconds is how often to check. Zero uses the default of 3.
	PollIntervalSeconds int
}

// ImageListOptions filters an image listing.
type ImageListOptions struct {
	// PageSize is how many images to fetch per underlying request. Zero
	// uses the server default.
	PageSize int
}

// ImageClient manages the images sandboxes are created from, on the control
// plane. Get one from SandboxesClient.Images.
type ImageClient struct {
	api            *managementapi.Client
	teamID         func() *string
	toControlError func(err error) error
}

// Images returns the client for managing images.
func (c *SandboxesClient) Images() *ImageClient {
	return &ImageClient{
		api:            c.api,
		teamID:         c.teamID,
		toControlError: func(err error) error { return toSandboxAPIError(err, "control") },
	}
}

// Push pushes one image: it zips Directory (or takes RegistryImage), asks the
// API for an upload URL, uploads, and with WaitForBuilt waits for the build.
func (c *ImageClient) Push(ctx context.Context, opts *ImagePushOptions) (*ImageInfo, error) {
	if (opts.Directory == "") == (opts.RegistryImage == "") {
		return nil, errors.New("an image push needs exactly one of Directory or RegistryImage")
	}
	if opts.DockerConfig != "" && opts.RegistryImage == "" {
		return nil, errors.New("DockerConfig applies only to RegistryImage")
	}
	var sourceZip []byte
	if opts.Directory != "" {
		var err error
		sourceZip, err = zipImageSource(opts.Directory)
		if err != nil {
			return nil, err
		}
	}
	pushed, err := c.api.PushImage(ctx, managementapi.PushImageParams{TeamId: c.teamID()},
		managementapi.PushImageRequest{
			Name:         opts.Name,
			Image:        optionalString(opts.RegistryImage),
			DockerConfig: optionalString(opts.DockerConfig),
		})
	if err != nil {
		return nil, c.toControlError(err)
	}
	if sourceZip != nil {
		if pushed.UploadUrl == nil || *pushed.UploadUrl == "" {
			return nil, fmt.Errorf("pushing image %s returned no upload URL", opts.Name)
		}
		if err := c.uploadSource(ctx, opts.Name, *pushed.UploadUrl, sourceZip); err != nil {
			return nil, err
		}
	}
	if !opts.WaitForBuilt {
		return &ImageInfo{Name: pushed.Name, Status: string(pushed.Status)}, nil
	}
	return c.waitBuilt(ctx, opts.Name, opts.TimeoutSeconds, opts.PollIntervalSeconds, true)
}

// WaitBuilt waits until the image is ready, failing on a failed build or a
// wait that runs out of time. It checks the current status, so right after
// pushing a new version of an already built image it may return before the
// new version is processed; Push with WaitForBuilt avoids that.
func (c *ImageClient) WaitBuilt(ctx context.Context, opts *ImageWaitOptions) (*ImageInfo, error) {
	return c.waitBuilt(ctx, opts.Name, opts.TimeoutSeconds, opts.PollIntervalSeconds, false)
}

// GetInfo gets an image's current record.
func (c *ImageClient) GetInfo(ctx context.Context, name string) (*ImageInfo, error) {
	image, err := c.api.GetImage(ctx, name, managementapi.GetImageParams{TeamId: c.teamID()})
	if err != nil {
		return nil, c.toControlError(err)
	}
	info := imageInfoFromAPI(image)
	return &info, nil
}

// List lists images, fetching further pages as iteration reaches them. An
// error yields as the pair's second value and ends the iteration.
func (c *ImageClient) List(ctx context.Context, opts *ImageListOptions) iter.Seq2[*ImageInfo, error] {
	params := managementapi.ListImagesParams{TeamId: c.teamID()}
	if opts.PageSize > 0 {
		params.Limit = &opts.PageSize
	}
	return func(yield func(*ImageInfo, error) bool) {
		// A server repeating a cursor would otherwise page forever.
		seenCursors := make(map[string]bool)
		for {
			page, err := c.api.ListImages(ctx, params)
			if err != nil {
				yield(nil, c.toControlError(err))
				return
			}
			for i := range page.Items {
				info := imageInfoFromAPI(&page.Items[i])
				if !yield(&info, nil) {
					return
				}
			}
			if !page.Pagination.HasMore || page.Pagination.Cursor == nil {
				return
			}
			if seenCursors[*page.Pagination.Cursor] {
				yield(nil, errors.New("image list returned a repeated cursor"))
				return
			}
			seenCursors[*page.Pagination.Cursor] = true
			params.Cursor = page.Pagination.Cursor
		}
	}
}

// Delete deletes an image repository and every version in it.
func (c *ImageClient) Delete(ctx context.Context, name string) (*ImageInfo, error) {
	image, err := c.api.DeleteImage(ctx, name, managementapi.DeleteImageParams{TeamId: c.teamID()})
	if err != nil {
		return nil, c.toControlError(err)
	}
	info := imageInfoFromAPI(image)
	return &info, nil
}

// Cleanup removes image versions that are no longer referenced.
func (c *ImageClient) Cleanup(ctx context.Context) (*ImageCleanupResult, error) {
	result, err := c.api.CleanupImages(ctx, managementapi.CleanupImagesParams{TeamId: c.teamID()})
	if err != nil {
		return nil, c.toControlError(err)
	}
	return &ImageCleanupResult{Deleted: result.Deleted, Message: result.Message}, nil
}

// uploadSource PUTs the zipped source to the URL the API signed for storage,
// so none of the API's headers or credentials go with it.
func (c *ImageClient) uploadSource(ctx context.Context, name, url string, sourceZip []byte) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(sourceZip))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/zip")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode >= 300 {
		return &ImageUploadError{ImageName: name, Status: response.StatusCode, Body: string(body)}
	}
	return nil
}

// waitBuilt polls until the image is BUILT or FAILED. A terminal status
// counts only once processing was observed, since the status is per
// repository and can still show the previous version's outcome right after a
// push; 404 and gateway statuses are ridden out, since the image may not be
// visible yet or the edge may briefly fail.
func (c *ImageClient) waitBuilt(ctx context.Context, name string, timeoutSeconds, pollIntervalSeconds int, requireProgress bool) (*ImageInfo, error) {
	deadline := time.Now().Add(imageWaitTimeout)
	if timeoutSeconds > 0 {
		deadline = time.Now().Add(time.Duration(timeoutSeconds) * time.Second)
	}
	interval := imageWaitPollInterval
	if pollIntervalSeconds > 0 {
		interval = time.Duration(pollIntervalSeconds) * time.Second
	}
	progressed := !requireProgress
	lastStatus := ImageStatus("unknown")
	for {
		image, err := c.api.GetImage(ctx, name, managementapi.GetImageParams{TeamId: c.teamID()})
		if err != nil {
			// A not-yet-visible image and a briefly failing edge both
			// answer with statuses worth riding out.
			converted := toSandboxAPIError(err, "control")
			var apiError *SandboxAPIError
			if !errors.As(converted, &apiError) || (apiError.Status != http.StatusNotFound &&
				!gatewayStatuses[apiError.Status]) {
				return nil, converted
			}
			lastStatus = fmt.Sprintf("HTTP %d", apiError.Status)
		} else {
			lastStatus = string(image.Status)
			switch string(image.Status) {
			case ImageStatusBuilt:
				if progressed {
					info := imageInfoFromAPI(image)
					return &info, nil
				}
			case ImageStatusFailed:
				if progressed {
					return nil, &ImageBuildError{ImageName: name, Status: ImageStatusFailed,
						Logs: c.buildFailureLogs(ctx, name)}
				}
			default:
				// UPLOADING, BUILDING, or a status added later: all still
				// processing.
				progressed = true
			}
		}
		if time.Now().After(deadline) {
			return nil, &ImageBuildError{ImageName: name, Status: lastStatus, TimedOut: true}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
	}
}

func imageInfoFromAPI(image *managementapi.Image) ImageInfo {
	info := ImageInfo{Name: image.Name, Status: string(image.Status)}
	if image.DisplayName != nil {
		info.DisplayName = *image.DisplayName
	}
	if image.CreatedAt != nil {
		info.CreatedAt = *image.CreatedAt
	}
	if image.LastDeployedAt != nil {
		info.LastDeployedAt = *image.LastDeployedAt
	}
	if image.UpdatedAt != nil {
		info.UpdatedAt = *image.UpdatedAt
	}
	if image.Size != nil {
		info.SizeBytes = *image.Size
	}
	if image.TagCount != nil {
		info.TagCount = *image.TagCount
	}
	return info
}

// zipImageSource zips a directory as the image source: relative paths with
// forward slashes, directories as entries, Unix permission bits kept, and a
// Dockerfile required at the root.
func zipImageSource(directory string) ([]byte, error) {
	entries, err := imageSourceEntries(directory)
	if err != nil {
		return nil, err
	}
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	for _, entry := range entries {
		header := &zip.FileHeader{
			Name:     entry.path,
			Modified: time.Unix(0, 0),
		}
		header.SetMode(entry.mode)
		file, err := writer.CreateHeader(header)
		if err != nil {
			return nil, err
		}
		if entry.source != "" {
			opened, err := os.Open(entry.source)
			if err != nil {
				return nil, err
			}
			_, copyErr := io.Copy(file, opened)
			closeErr := opened.Close()
			if copyErr != nil {
				return nil, copyErr
			}
			if closeErr != nil {
				return nil, closeErr
			}
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return archive.Bytes(), nil
}

type imageSourceEntry struct {
	path string
	mode fs.FileMode
	// source is the file to stream into the archive, empty for a directory.
	source string
}

func imageSourceEntries(directory string) ([]imageSourceEntry, error) {
	hasDockerfile := false
	var entries []imageSourceEntry
	err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(directory, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		archivePath := filepath.ToSlash(relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			entries = append(entries, imageSourceEntry{path: archivePath + "/", mode: info.Mode()})
			return nil
		}
		if archivePath == "Dockerfile" {
			hasDockerfile = true
		}
		entries = append(entries, imageSourceEntry{path: archivePath, mode: info.Mode(), source: path})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("zipping %s: %w", directory, err)
	}
	if !hasDockerfile {
		return nil, fmt.Errorf("the image source %s has no Dockerfile at its root", directory)
	}
	// Deterministic order, so the same source makes the same archive.
	slices.SortFunc(entries, func(a, b imageSourceEntry) int {
		return strings.Compare(a.path, b.path)
	})
	return entries, nil
}

// buildFailureLogs reads the recorded build logs so a failed build can say
// why. Best-effort: the failed status is already established, so a log service
// that does not answer must not mask it.
func (c *ImageClient) buildFailureLogs(ctx context.Context, name string) []string {
	logs, err := c.api.GetImageBuildLogs(ctx, name, managementapi.GetImageBuildLogsParams{TeamId: c.teamID()})
	if err != nil {
		return nil
	}
	messages := make([]string, 0, len(logs.Logs))
	for _, entry := range logs.Logs {
		messages = append(messages, entry.Message)
	}
	return messages
}
