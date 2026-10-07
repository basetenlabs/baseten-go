package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/basetenlabs/baseten-go/client/managementapi"
)

const (
	defaultImageWaitTimeout  = 15 * time.Minute
	defaultImagePollInterval = 3 * time.Second

	imageLogsPageSize = 1000
	// The largest offset the build log endpoint accepts.
	imageLogsMaxOffset = 10_000
)

// ImageClient works with the images that sandboxes are created from: pushing
// them from a build context or a registry, and finding and deleting them. Get
// it from [Client.Images].
type ImageClient struct {
	client *Client
}

// Images works with the images that sandboxes are created from.
func (c *Client) Images() *ImageClient {
	return c.images
}

// ImageStatus is the status of an image. Other values may be added, so do not
// treat the constants as exhaustive.
type ImageStatus string

// Values for ImageStatus.
const (
	ImageStatusUploading ImageStatus = "UPLOADING"
	ImageStatusBuilding  ImageStatus = "BUILDING"
	ImageStatusBuilt     ImageStatus = "BUILT"
	ImageStatusFailed    ImageStatus = "FAILED"
)

// ImageInfo is an image's record.
type ImageInfo struct {
	// Name is the image's unique name.
	Name string

	// Status is the status of the image's latest version.
	Status ImageStatus

	// CreatedAt is when the image was created, zero when unknown.
	CreatedAt time.Time

	// UpdatedAt is when the image was last updated, zero when unknown.
	UpdatedAt time.Time

	// LastDeployedAt is when a sandbox last used the image, zero when none
	// has.
	LastDeployedAt time.Time

	// SizeBytes is the image's size, zero when unknown.
	SizeBytes int64

	// TagCount is how many versions the image has.
	TagCount int
}

// ImageSummary is an image's record as returned by [ImageClient.List].
type ImageSummary struct {
	// Name is the image's unique name.
	Name string

	// Status is the status of the image's latest version.
	Status ImageStatus

	// CreatedAt is when the image was created, zero when unknown.
	CreatedAt time.Time

	// UpdatedAt is when the image was last updated, zero when unknown.
	UpdatedAt time.Time

	// LastDeployedAt is when a sandbox last used the image, zero when none
	// has.
	LastDeployedAt time.Time

	// SizeBytes is the image's size, zero when unknown.
	SizeBytes int64

	// TagCount is how many versions the image has.
	TagCount int
}

// ImageTagInfo is one version of an image.
type ImageTagInfo struct {
	// Name is the version's tag.
	Name string

	// CreatedAt is when the version was created, zero when unknown.
	CreatedAt time.Time

	// UpdatedAt is when the version was last updated, zero when unknown.
	UpdatedAt time.Time

	// SizeBytes is the version's size, zero when unknown.
	SizeBytes int64
}

// ImagePushOptions are the options for [ImageClient.Push].
type ImagePushOptions struct {
	// Name is the image's name. Required. To create a sandbox from the
	// image, pass "<name>:latest" as [CreateOptions.Image].
	Name string

	// Exactly one source must be set: a builder, a registry image, or a build
	// context as a zip, a local directory, or in-memory files. A build
	// context needs a Dockerfile at its root.

	// SourceBuilder is a builder whose Dockerfile and files make up the
	// build context. Its recorded errors, if any, fail the push before
	// anything is sent.
	SourceBuilder *ImageBuilder

	// SourceRegistry is a registry image to import.
	SourceRegistry *ImageRegistrySource

	// SourceZip is a build context, already zipped.
	SourceZip []byte

	// SourceDirectory is a local directory to zip as the build context.
	// Files keep their modes, a link to a file within the directory stores
	// a copy of the file, a link to a file outside it fails the push, a link
	// to a directory stores an empty directory, and a broken link is left
	// out.
	//
	// Paths are left out as a .dockerignore at the directory's root says,
	// read through IgnoreFileProcessor, or else by DefaultIgnoreFile. The
	// root Dockerfile and .dockerignore are always kept, as Docker keeps them.
	SourceDirectory string

	// SourceFiles are the build context's files, by forward-slashed path
	// relative to its root, each with mode 0644.
	SourceFiles map[string][]byte

	// IgnoreFileProcessor parses the .dockerignore at the root of
	// SourceDirectory into an [ImageIgnoreFileFunc], which should match
	// Docker's .dockerignore semantics. Required if SourceDirectory has a
	// .dockerignore; otherwise the push fails before anything is sent. This
	// package has no .dockerignore parser of its own.
	IgnoreFileProcessor func(context.Context, ImageIgnoreFileProcessorOptions) (ImageIgnoreFileFunc, error)

	// DefaultIgnoreFile filters SourceDirectory when it has no
	// .dockerignore. If nil, [DefaultImageIgnoreFile] is used. Pass a
	// function that always returns false to push everything.
	DefaultIgnoreFile ImageIgnoreFileFunc

	// NoTempFile zips a build context read from local files, from
	// SourceDirectory or a builder's local files and directories, in memory
	// instead of in a temporary file. Other build contexts are always zipped
	// in memory.
	NoTempFile bool

	// NoWait returns as soon as the image is pushed, instead of waiting for
	// it to be ready to use.
	NoWait bool

	// Timeout is how long to wait for the image to be ready to use. Zero
	// uses 15 minutes, and a negative value waits with no limit. The build
	// continues regardless.
	Timeout time.Duration

	// PollInterval is how often to check the image's status while waiting.
	// Zero uses 3 seconds.
	PollInterval time.Duration
}

// ImageRegistrySource is a registry image for [ImagePushOptions.SourceRegistry].
type ImageRegistrySource struct {
	// Image is the registry image reference to import. Required.
	Image string

	// DockerConfig is a Docker config.json with the credentials for pulling
	// Image from a private registry.
	DockerConfig string
}

// Push pushes an image, from a registry or from a build context that is built
// into one. Waits for the image to be ready to use unless NoWait is set,
// returning an [*ImageBuildError] if its build fails or the wait times out.
//
// A build context read from local files is zipped into a temporary file,
// removed when Push returns, unless NoTempFile is set; any other is zipped in
// memory. It is uploaded in one attempt. If the upload fails, with an
// [*ImageUploadError], the image is left as the service has it, so push again
// or delete it.
func (c *ImageClient) Push(ctx context.Context, opts ImagePushOptions) (*ImageInfo, error) {
	if opts.Name == "" {
		return nil, errors.New("Name is required")
	}
	// Zipped before anything is sent, so a bad source leaves nothing behind.
	archive, err := pushSourceZip(ctx, opts)
	if err != nil {
		return nil, err
	}
	defer archive.close()
	request := managementapi.PushSandboxImageRequest{Name: opts.Name}
	if registry := opts.SourceRegistry; registry != nil {
		request.Image = &registry.Image
		request.DockerConfig = optional(registry.DockerConfig)
	}
	response, err := c.client.api.PushImage(ctx, managementapi.PushImageParams{TeamId: c.client.teamID()}, request)
	if err != nil {
		return nil, controlPlaneError(err)
	}
	if archive.present() {
		if response.UploadUrl == nil {
			return nil, fmt.Errorf("pushing image %s returned no upload URL", opts.Name)
		}
		if err := c.upload(ctx, opts.Name, *response.UploadUrl, archive); err != nil {
			return nil, err
		}
	}
	if opts.NoWait {
		return &ImageInfo{Name: response.Name, Status: ImageStatus(response.Status)}, nil
	}
	return c.waitBuilt(ctx, opts.Name, opts.Timeout, opts.PollInterval)
}

// ImageWaitBuiltOptions are the options for [ImageClient.WaitBuilt].
type ImageWaitBuiltOptions struct {
	// Name is the image's name. Required.
	Name string

	// Timeout is how long to wait. Zero uses 15 minutes, and a negative
	// value waits with no limit. The build continues regardless.
	Timeout time.Duration

	// PollInterval is how often to check the image's status. Zero uses 3
	// seconds.
	PollInterval time.Duration
}

// WaitBuilt waits for an image to be ready to use, returning an
// [*ImageBuildError] if its build fails or the wait times out. It checks the
// image's current status, so right after pushing a new version of an image
// that was already built, it may return before the new version is processed;
// push without NoWait to avoid that.
func (c *ImageClient) WaitBuilt(ctx context.Context, opts ImageWaitBuiltOptions) (*ImageInfo, error) {
	if opts.Name == "" {
		return nil, errors.New("Name is required")
	}
	return c.waitBuilt(ctx, opts.Name, opts.Timeout, opts.PollInterval)
}

// ImageGetInfoOptions are the options for [ImageClient.GetInfo].
type ImageGetInfoOptions struct {
	// Name is the image's name. Required.
	Name string
}

// GetInfo gets an image's record.
func (c *ImageClient) GetInfo(ctx context.Context, opts ImageGetInfoOptions) (*ImageInfo, error) {
	if opts.Name == "" {
		return nil, errors.New("Name is required")
	}
	image, err := c.client.api.GetImage(ctx, opts.Name, managementapi.GetImageParams{TeamId: c.client.teamID()})
	if err != nil {
		return nil, controlPlaneError(err)
	}
	info := imageInfoFromAPI(image)
	return &info, nil
}

// ImageSort is the sort order of an image listing. Other values may be added,
// so do not treat the constants as exhaustive.
type ImageSort string

// Values for ImageSort.
const (
	ImageSortNameAsc       ImageSort = "name:asc"
	ImageSortNameDesc      ImageSort = "name:desc"
	ImageSortCreatedAtAsc  ImageSort = "createdAt:asc"
	ImageSortCreatedAtDesc ImageSort = "createdAt:desc"
)

// ImageListOptions are the options for [ImageClient.List].
type ImageListOptions struct {
	// NamePrefix keeps only images whose names start with this,
	// case-sensitively.
	NamePrefix string

	// Sort is the order of the listing. Empty uses [ImageSortCreatedAtDesc].
	Sort ImageSort

	// PageSize is how many images each underlying request fetches. Zero uses
	// the server's default.
	PageSize int
}

// List lists image summaries, fetching further pages as iteration reaches
// them. An error is yielded as the second value and ends the iteration.
func (c *ImageClient) List(ctx context.Context, opts ImageListOptions) iter.Seq2[*ImageSummary, error] {
	return func(yield func(*ImageSummary, error) bool) {
		params := managementapi.ListImagesParams{
			TeamId: c.client.teamID(),
			Limit:  optional(opts.PageSize),
			Sort:   optional(string(opts.Sort)),
			Q:      optional(opts.NamePrefix),
		}
		paginate(yield, "image list", func(cursor *string) ([]managementapi.SandboxImageSummary, managementapi.SandboxApiPagination, error) {
			params.Cursor = cursor
			page, err := c.client.api.ListImages(ctx, params)
			if err != nil {
				return nil, managementapi.SandboxApiPagination{}, err
			}
			return page.Items, page.Pagination, nil
		}, func(image *managementapi.SandboxImageSummary) (ImageSummary, error) {
			return ImageSummary{
				Name:           image.Name,
				Status:         ImageStatus(image.Status),
				CreatedAt:      deref(image.CreatedAt),
				UpdatedAt:      deref(image.UpdatedAt),
				LastDeployedAt: deref(image.LastDeployedAt),
				SizeBytes:      deref(image.Size),
				TagCount:       int(deref(image.TagCount)),
			}, nil
		})
	}
}

// ImageDeleteOptions are the options for [ImageClient.Delete].
type ImageDeleteOptions struct {
	// Name is the image's name. Required.
	Name string
}

// Delete deletes an image and all its versions, and returns its record. Fails
// while a sandbox uses any version.
func (c *ImageClient) Delete(ctx context.Context, opts ImageDeleteOptions) (*ImageInfo, error) {
	if opts.Name == "" {
		return nil, errors.New("Name is required")
	}
	image, err := c.client.api.DeleteImage(ctx, opts.Name, managementapi.DeleteImageParams{TeamId: c.client.teamID()})
	if err != nil {
		return nil, controlPlaneError(err)
	}
	info := imageInfoFromAPI(image)
	return &info, nil
}

// ImageTagSort is the sort order of an image's version listing. Other values
// may be added, so do not treat the constants as exhaustive.
type ImageTagSort string

// Values for ImageTagSort.
const (
	ImageTagSortNameAsc  ImageTagSort = "name:asc"
	ImageTagSortNameDesc ImageTagSort = "name:desc"
)

// ImageListTagsOptions are the options for [ImageClient.ListTags].
type ImageListTagsOptions struct {
	// Name is the image's name. Required.
	Name string

	// NamePrefix keeps only tags that start with this.
	NamePrefix string

	// Sort is the order of the listing.
	Sort ImageTagSort

	// PageSize is how many versions each underlying request fetches. Zero
	// uses the server's default.
	PageSize int
}

// ListTags lists an image's versions, fetching further pages as iteration
// reaches them. An error is yielded as the second value and ends the
// iteration.
func (c *ImageClient) ListTags(ctx context.Context, opts ImageListTagsOptions) iter.Seq2[*ImageTagInfo, error] {
	return func(yield func(*ImageTagInfo, error) bool) {
		if opts.Name == "" {
			yield(nil, errors.New("Name is required"))
			return
		}
		params := managementapi.ListImageTagsParams{
			TeamId: c.client.teamID(),
			Limit:  optional(opts.PageSize),
			Sort:   optional(string(opts.Sort)),
			Q:      optional(opts.NamePrefix),
		}
		paginate(yield, "image tag list", func(cursor *string) ([]managementapi.SandboxImageTag, managementapi.SandboxApiPagination, error) {
			params.Cursor = cursor
			page, err := c.client.api.ListImageTags(ctx, opts.Name, params)
			if err != nil {
				return nil, managementapi.SandboxApiPagination{}, err
			}
			return page.Items, page.Pagination, nil
		}, func(tag *managementapi.SandboxImageTag) (ImageTagInfo, error) { return imageTagInfoFromAPI(tag), nil })
	}
}

// ImageDeleteTagOptions are the options for [ImageClient.DeleteTag].
type ImageDeleteTagOptions struct {
	// Name is the image's name. Required.
	Name string

	// Tag is the version to delete. Required.
	Tag string
}

// DeleteTag deletes one version of an image and returns the image's record as
// it is afterwards. Fails while a sandbox uses the version.
func (c *ImageClient) DeleteTag(ctx context.Context, opts ImageDeleteTagOptions) (*ImageInfo, error) {
	if opts.Name == "" {
		return nil, errors.New("Name is required")
	} else if opts.Tag == "" {
		return nil, errors.New("Tag is required")
	}
	image, err := c.client.api.DeleteImageTag(ctx, opts.Name, opts.Tag, managementapi.DeleteImageTagParams{TeamId: c.client.teamID()})
	if err != nil {
		return nil, controlPlaneError(err)
	}
	info := imageInfoFromAPI(image)
	return &info, nil
}

// ImageCleanupOptions are the options for [ImageClient.Cleanup].
type ImageCleanupOptions struct{}

// ImageCleanupResult is the outcome of [ImageClient.Cleanup].
type ImageCleanupResult struct {
	// Deleted is how many image versions were removed.
	Deleted int

	// Message describes the outcome.
	Message string
}

// Cleanup removes image versions that no sandbox uses.
func (c *ImageClient) Cleanup(ctx context.Context, opts ImageCleanupOptions) (*ImageCleanupResult, error) {
	result, err := c.client.api.CleanupImages(ctx, managementapi.CleanupImagesParams{TeamId: c.client.teamID()})
	if err != nil {
		return nil, controlPlaneError(err)
	}
	return &ImageCleanupResult{Deleted: result.Deleted, Message: result.Message}, nil
}

// ImageLogsOptions are the options for [ImageClient.Logs].
type ImageLogsOptions struct {
	// Name is the image's name. Required.
	Name string

	// StartTime keeps only lines from this time on. Zero means no lower
	// bound.
	StartTime time.Time

	// EndTime keeps only lines up to this time. Zero means now.
	EndTime time.Time
}

// ImageLogLine is one line of an image's build log.
type ImageLogLine struct {
	// Timestamp is when the line was written.
	Timestamp time.Time

	// Severity is the line's severity level.
	Severity int

	// Text is the line's text.
	Text string
}

// Logs gets the build log of an image's latest build, including a failed one,
// oldest line first. Logs may take a short time to appear. Returns at most
// the 11,000 most recent lines in the range; narrow the range to see earlier
// ones.
func (c *ImageClient) Logs(ctx context.Context, opts ImageLogsOptions) ([]ImageLogLine, error) {
	if opts.Name == "" {
		return nil, errors.New("Name is required")
	}
	// Pinned on the first page, so the default range does not move while
	// paging through it.
	endTime := opts.EndTime
	if endTime.IsZero() {
		endTime = time.Now()
	}
	params := managementapi.GetImageBuildLogsParams{
		TeamId:  c.client.teamID(),
		EndTime: &endTime,
		Limit:   optional(imageLogsPageSize),
	}
	if !opts.StartTime.IsZero() {
		params.StartTime = &opts.StartTime
	}
	var lines []ImageLogLine
	for offset := 0; offset <= imageLogsMaxOffset; offset += imageLogsPageSize {
		params.Offset = &offset
		page, err := c.client.api.GetImageBuildLogs(ctx, opts.Name, params)
		if err != nil {
			return nil, controlPlaneError(err)
		}
		for _, log := range page.Logs {
			lines = append(lines, ImageLogLine{Timestamp: log.Timestamp, Severity: log.Severity, Text: log.Message})
		}
		if len(page.Logs) < imageLogsPageSize || int64(offset+imageLogsPageSize) >= page.TotalCount {
			break
		}
	}
	// Pages come newest first.
	slices.Reverse(lines)
	return lines, nil
}

// ImageListLibraryOptions are the options for [ImageClient.ListLibrary].
type ImageListLibraryOptions struct{}

// ImageLibraryInfo is a built-in image available to every team, usable
// directly as a sandbox's image without pushing it.
type ImageLibraryInfo struct {
	// Name is the image's stable identifier.
	Name string

	// DisplayName is the image's human-readable name.
	DisplayName string

	// Description is a short description.
	Description string

	// LongDescription is a detailed description.
	LongDescription string

	// Image is the image reference, including its tag, to pass as
	// [CreateOptions.Image].
	Image string

	// Memory is the recommended memory allocation in megabytes, zero when
	// there is none.
	Memory int

	// Ports are the ports the image exposes.
	Ports []Port

	// Categories are the image's categories.
	Categories []string

	// Tags are the image's tags.
	Tags []string

	// URL is the image's documentation URL.
	URL string

	// Icon is the URL of the image's icon.
	Icon string

	// IconLight is the URL of the image's light-mode icon.
	IconLight string

	// IconDark is the URL of the image's dark-mode icon.
	IconDark string

	// Enterprise is whether the image requires an enterprise plan.
	Enterprise bool

	// CreationExtraArgs are kernel selection arguments suggested when
	// creating a sandbox from this image.
	CreationExtraArgs map[string]string

	// CreationVolumes are volume attachments suggested when creating a
	// sandbox from this image.
	CreationVolumes []ImageLibraryVolume
}

// ImageLibraryVolume is a volume attachment a built-in image suggests.
type ImageLibraryVolume struct {
	// Name is the volume's name, or an internal identifier for an ephemeral
	// volume.
	Name string

	// MountPath is the absolute path the volume is mounted at.
	MountPath string

	// Type is the volume's type, empty for a persistent volume.
	Type string

	// SizeMB is the storage capacity in megabytes of an ephemeral volume.
	SizeMB int

	// ReadOnly is whether the volume is mounted read-only.
	ReadOnly bool
}

// ListLibrary lists the built-in images available to every team.
func (c *ImageClient) ListLibrary(ctx context.Context, opts ImageListLibraryOptions) ([]ImageLibraryInfo, error) {
	response, err := c.client.api.ListSandboxLibraryImages(ctx)
	if err != nil {
		return nil, controlPlaneError(err)
	}
	images := make([]ImageLibraryInfo, 0, len(response.Items))
	for _, image := range response.Items {
		info := ImageLibraryInfo{
			Name:            image.Name,
			DisplayName:     deref(image.DisplayName),
			Description:     deref(image.Description),
			LongDescription: deref(image.LongDescription),
			Image:           image.Image,
			Memory:          deref(image.Memory),
			Ports:           portsFromAPI(deref(image.Ports)),
			Categories:      deref(image.Categories),
			Tags:            deref(image.Tags),
			URL:             deref(image.Url),
			Icon:            deref(image.Icon),
			IconLight:       deref(image.IconLight),
			IconDark:        deref(image.IconDark),
			Enterprise:      deref(image.Enterprise),
		}
		if options := image.CreationOptions; options != nil {
			info.CreationExtraArgs = deref(options.ExtraArgs)
			for _, volume := range deref(options.Volumes) {
				info.CreationVolumes = append(info.CreationVolumes, ImageLibraryVolume{
					Name:      volume.Name,
					MountPath: volume.MountPath,
					Type:      deref(volume.Type),
					SizeMB:    deref(volume.SizeMb),
					ReadOnly:  deref(volume.ReadOnly),
				})
			}
		}
		images = append(images, info)
	}
	return images, nil
}

// upload puts a build context to the signed storage URL a push returned.
func (c *ImageClient) upload(ctx context.Context, name, url string, archive *pushArchive) error {
	// The URL is signed for storage, so none of the API's headers or
	// credentials go with it.
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, archive.newBody())
	if err != nil {
		return err
	}
	// Set explicitly, since the request cannot tell a file's length, and
	// storage rejects an upload without one.
	req.ContentLength = archive.size
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(archive.newBody()), nil }
	req.Header.Set("Content-Type", "application/zip")
	resp, err := c.client.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &ImageUploadError{ImageName: name, Status: resp.StatusCode, Body: string(body)}
	}
	return nil
}

// waitBuilt polls an image until it is built or failed. A push resets the
// image's status, so after one, BUILT or FAILED is that push's outcome.
func (c *ImageClient) waitBuilt(ctx context.Context, name string, timeout, pollInterval time.Duration) (*ImageInfo, error) {
	if timeout == 0 {
		timeout = defaultImageWaitTimeout
	}
	if pollInterval == 0 {
		pollInterval = defaultImagePollInterval
	}
	// A negative timeout waits with no limit.
	waitCtx, cancel := ctx, context.CancelFunc(func() {})
	if timeout > 0 {
		waitCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()
	lastStatus := ImageStatus("unknown")
	for {
		image, err := c.client.api.GetImage(waitCtx, name, managementapi.GetImageParams{TeamId: c.client.teamID()})
		if err == nil {
			// Anything else, such as UPLOADING, BUILDING, or a status added
			// later, is still processing.
			switch lastStatus = ImageStatus(image.Status); lastStatus {
			case ImageStatusFailed:
				return nil, &ImageBuildError{ImageName: name, Status: lastStatus}
			case ImageStatusBuilt:
				info := imageInfoFromAPI(image)
				return &info, nil
			}
		} else if err = controlPlaneError(err); waitCtx.Err() == nil && !isRetryableWaitError(err) {
			return nil, err
		}
		timer := time.NewTimer(pollInterval)
		select {
		case <-waitCtx.Done():
			timer.Stop()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, &ImageBuildError{ImageName: name, Status: lastStatus, TimedOut: true}
		case <-timer.C:
		}
	}
}

// isRetryableWaitError reports whether a wait should ride out an error: a
// dropped connection, a gateway error, or a not-found while a push is still
// becoming visible.
func isRetryableWaitError(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		switch apiErr.Status {
		case http.StatusNotFound, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		}
		return false
	}
	return isTransientResetError(err)
}

// pushArchive is a zipped build context, in memory or in a temporary file. A
// nil pushArchive is none, for a registry image.
type pushArchive struct {
	data []byte
	file *os.File
	size int64
}

func (a *pushArchive) present() bool {
	return a != nil
}

// newBody returns a reader over the whole archive, a new one per call so a
// request can be resent.
func (a *pushArchive) newBody() io.Reader {
	if a.file != nil {
		// A section reader, not the file, so the request never closes it.
		return io.NewSectionReader(a.file, 0, a.size)
	}
	return bytes.NewReader(a.data)
}

// close removes a temporary file, if any.
func (a *pushArchive) close() {
	if a == nil || a.file == nil {
		return
	}
	_ = a.file.Close()
	_ = os.Remove(a.file.Name())
}

// zipPushArchive zips entries into a temporary file when toFile is set, or
// else in memory.
func zipPushArchive(ctx context.Context, entries []zipEntry, toFile bool) (*pushArchive, error) {
	if !toFile {
		var buf bytes.Buffer
		if err := writeZip(ctx, &buf, entries); err != nil {
			return nil, err
		}
		return &pushArchive{data: buf.Bytes(), size: int64(buf.Len())}, nil
	}
	file, err := os.CreateTemp("", "baseten-image-*.zip")
	if err != nil {
		return nil, err
	}
	archive := &pushArchive{file: file}
	if err := writeZip(ctx, file, entries); err != nil {
		archive.close()
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		archive.close()
		return nil, err
	}
	archive.size = info.Size()
	return archive, nil
}

// pushSourceZip checks that a push has exactly one source, and zips it unless
// it is a registry image.
func pushSourceZip(ctx context.Context, opts ImagePushOptions) (*pushArchive, error) {
	var sources []string
	for name, set := range map[string]bool{
		"SourceBuilder":   opts.SourceBuilder != nil,
		"SourceRegistry":  opts.SourceRegistry != nil,
		"SourceZip":       opts.SourceZip != nil,
		"SourceDirectory": opts.SourceDirectory != "",
		"SourceFiles":     opts.SourceFiles != nil,
	} {
		if set {
			sources = append(sources, name)
		}
	}
	if len(sources) != 1 {
		slices.Sort(sources)
		got := "none"
		if len(sources) > 1 {
			got = strings.Join(sources, ", ")
		}
		return nil, fmt.Errorf("an image push needs exactly one of SourceBuilder, SourceRegistry, SourceZip, SourceDirectory, or SourceFiles, got %s", got)
	}
	switch {
	case opts.SourceBuilder != nil:
		entries, err := opts.SourceBuilder.zipEntries(ctx)
		if err != nil {
			return nil, err
		}
		return zipPushArchive(ctx, entries, !opts.NoTempFile && slices.ContainsFunc(entries, func(e zipEntry) bool { return e.sourcePath != "" }))
	case opts.SourceRegistry != nil:
		if opts.SourceRegistry.Image == "" {
			return nil, errors.New("SourceRegistry.Image is required")
		}
		return nil, nil
	case opts.SourceZip != nil:
		if err := checkZipDockerfile(opts.SourceZip); err != nil {
			return nil, err
		}
		return &pushArchive{data: opts.SourceZip, size: int64(len(opts.SourceZip))}, nil
	case opts.SourceDirectory != "":
		ignore, err := resolveImageIgnoreFile(ctx, opts)
		if err != nil {
			return nil, err
		}
		entries, err := directoryZipEntries(ctx, opts.SourceDirectory, ignore)
		if err != nil {
			return nil, err
		}
		return zipPushArchive(ctx, entries, !opts.NoTempFile)
	case opts.SourceFiles != nil:
		entries, err := filesZipEntries(opts.SourceFiles)
		if err != nil {
			return nil, err
		}
		return zipPushArchive(ctx, entries, false)
	}
	return nil, nil
}

func imageInfoFromAPI(image *managementapi.SandboxImage) ImageInfo {
	return ImageInfo{
		Name:           image.Name,
		Status:         ImageStatus(image.Status),
		CreatedAt:      deref(image.CreatedAt),
		UpdatedAt:      deref(image.UpdatedAt),
		LastDeployedAt: deref(image.LastDeployedAt),
		SizeBytes:      deref(image.Size),
		TagCount:       int(deref(image.TagCount)),
	}
}

func imageTagInfoFromAPI(tag *managementapi.SandboxImageTag) ImageTagInfo {
	return ImageTagInfo{
		Name:      tag.Name,
		CreatedAt: deref(tag.CreatedAt),
		UpdatedAt: deref(tag.UpdatedAt),
		SizeBytes: deref(tag.Size),
	}
}
