package sandbox

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/basetenlabs/baseten-go/client/sandboxapi"
)

const (
	defaultCopyTimeout = 180 * time.Second
	copyPollInterval   = 100 * time.Millisecond

	multipartThreshold = 5 << 20
	multipartPartSize  = 5 << 20

	// Most upload parts in flight at once per sandbox, across all of its
	// uploads. Many at once on one HTTP/2 connection can trip the server's
	// rapid reset limit, so this holds on every transport rather than
	// guessing which is used.
	uploadPartsInFlight = 2
)

// FileSystemService reads and writes files and directories in a sandbox. Get
// it from [Sandbox.FS].
type FileSystemService struct {
	sandbox *Sandbox
	// Slots for upload parts in flight, one per sandbox, since a sandbox
	// builds its file system once.
	partSlots chan struct{}
}

func newFileSystemService(sandbox *Sandbox) *FileSystemService {
	return &FileSystemService{sandbox: sandbox, partSlots: make(chan struct{}, uploadPartsInFlight)}
}

// FileSystemEntryType is the kind of a filesystem entry. Other values may be
// added, so do not treat the constants as exhaustive.
type FileSystemEntryType string

// Values for FileSystemEntryType.
const (
	FileSystemEntryTypeFile      FileSystemEntryType = "file"
	FileSystemEntryTypeDirectory FileSystemEntryType = "directory"
)

// FileSystemDirectory is a directory's contents, one level deep, from
// [FileSystemService.List].
type FileSystemDirectory struct {
	Name           string
	Path           string
	Files          []FileSystemFileInfo
	Subdirectories []FileSystemSubdirectoryInfo
}

// FileSystemFileInfo is a file in a [FileSystemDirectory].
type FileSystemFileInfo struct {
	Name string
	Path string

	// Size is the file's size in bytes.
	Size int64

	// Permissions is the file's mode, as the sandbox reports it.
	Permissions string

	Owner        string
	Group        string
	LastModified time.Time
}

// FileSystemSubdirectoryInfo is a subdirectory in a [FileSystemDirectory].
type FileSystemSubdirectoryInfo struct {
	Name string
	Path string
}

// FileSystemFindResult is the result of [FileSystemService.Find].
type FileSystemFindResult struct {
	Matches []FileSystemFindMatch

	// Total is the number of entries found, which may exceed the matches
	// returned.
	Total int
}

// FileSystemFindMatch is an entry found by [FileSystemService.Find].
type FileSystemFindMatch struct {
	// Path is relative to the searched path.
	Path string

	Type FileSystemEntryType
}

// FileSystemGrepResult is the result of [FileSystemService.Grep].
type FileSystemGrepResult struct {
	Matches []FileSystemGrepMatch

	// Total is the number of matching lines, which may exceed the matches
	// returned.
	Total int
}

// FileSystemGrepMatch is a matching line found by [FileSystemService.Grep].
type FileSystemGrepMatch struct {
	// Path is relative to the searched path.
	Path string

	// Line is the line number, starting at 1.
	Line int

	Column int

	// Text is the matching line.
	Text string

	// Context is the lines around the match, empty when the sandbox includes
	// none.
	Context string
}

// FileSystemWatchOp is the kind of change in a [FileSystemWatchEvent]. Other
// values may be added, so do not treat the constants as exhaustive.
type FileSystemWatchOp string

// Values for FileSystemWatchOp.
const (
	FileSystemWatchOpCreate FileSystemWatchOp = "CREATE"
	FileSystemWatchOpWrite  FileSystemWatchOp = "WRITE"
	FileSystemWatchOpRemove FileSystemWatchOp = "REMOVE"
	FileSystemWatchOpRename FileSystemWatchOp = "RENAME"
	FileSystemWatchOpChmod  FileSystemWatchOp = "CHMOD"
)

// FileSystemWatchEvent is a change seen by [FileSystemService.Watch].
type FileSystemWatchEvent struct {
	Op FileSystemWatchOp

	// Path is the full path of the changed entry.
	Path string
}

// FileSystemReadOptions are the options for [FileSystemService.Read].
type FileSystemReadOptions struct {
	// Path is the absolute path of the file. Required.
	Path string
}

// Read reads a text file.
func (f *FileSystemService) Read(ctx context.Context, opts FileSystemReadOptions) (string, error) {
	if opts.Path == "" {
		return "", errors.New("Path is required")
	}
	raw, err := f.get(ctx, opts.Path)
	if err != nil {
		return "", err
	}
	var file struct {
		Content *string `json:"content"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		return "", err
	}
	if file.Content == nil {
		return "", fmt.Errorf("%s is a directory, not a file", opts.Path)
	}
	return *file.Content, nil
}

// FileSystemReadBytesOptions are the options for [FileSystemService.ReadBytes].
type FileSystemReadBytesOptions struct {
	// Path is the absolute path of the file. Required.
	Path string
}

// ReadBytes reads a file as bytes.
func (f *FileSystemService) ReadBytes(ctx context.Context, opts FileSystemReadBytesOptions) ([]byte, error) {
	if opts.Path == "" {
		return nil, errors.New("Path is required")
	}
	// The body is read inside the retried call, so a connection dropped
	// mid-body is retried too.
	return retryIdempotent(ctx, f.sandbox.retries.ReadMaxRetries, f.sandbox.retries.GatewayMaxRetries,
		func() ([]byte, error) {
			response, err := f.sandbox.api.GetFilesystemRaw(ctx, opts.Path, sandboxapi.GetFilesystemPathParams{})
			if err != nil {
				return nil, sandboxError(err)
			}
			defer response.Body.Close()
			// A directory comes back as its JSON listing whatever was asked
			// for.
			if strings.Contains(response.Header.Get("Content-Type"), "application/json") {
				return nil, fmt.Errorf("%s is a directory, not a file", opts.Path)
			}
			return io.ReadAll(response.Body)
		})
}

// FileSystemWriteOptions are the options for [FileSystemService.Write].
type FileSystemWriteOptions struct {
	// Path is the absolute path of the file, created or replaced. Required.
	Path string

	// Content is the text content to write.
	Content string
}

// Write writes a text file, creating it or replacing its content. Content over
// 5MB is uploaded in 5MB parts through the multipart upload endpoints, at most
// 2 parts at a time per sandbox.
func (f *FileSystemService) Write(ctx context.Context, opts FileSystemWriteOptions) error {
	if opts.Path == "" {
		return errors.New("Path is required")
	}
	if len(opts.Content) > multipartThreshold {
		return f.writeMultipart(ctx, opts.Path, []byte(opts.Content), "")
	}
	// Retried, since a repeat writes the same content.
	_, err := retryIdempotent(ctx, f.sandbox.retries.UploadMaxRetries, f.sandbox.retries.GatewayMaxRetries,
		func() (*sandboxapi.SuccessResponse, error) {
			// Content is always sent, so empty content writes an empty file.
			response, err := f.sandbox.api.PutFilesystem(ctx, opts.Path, sandboxapi.FileRequest{Content: &opts.Content})
			return response, sandboxError(err)
		})
	return err
}

// FileSystemWriteBytesOptions are the options for
// [FileSystemService.WriteBytes].
type FileSystemWriteBytesOptions struct {
	// Path is the absolute path of the file, created or replaced. Required.
	Path string

	// Content is the content to write.
	Content []byte

	// Permissions is the octal file mode, such as "0755". Empty uses the
	// sandbox's default.
	Permissions string
}

// WriteBytes writes a file from bytes, creating it or replacing its content.
// Content over 5MB is uploaded in 5MB parts through the multipart upload
// endpoints, at most 2 parts at a time per sandbox.
func (f *FileSystemService) WriteBytes(ctx context.Context, opts FileSystemWriteBytesOptions) error {
	if opts.Path == "" {
		return errors.New("Path is required")
	}
	if len(opts.Content) > multipartThreshold {
		return f.writeMultipart(ctx, opts.Path, opts.Content, opts.Permissions)
	}
	fields := [][2]string{}
	if opts.Permissions != "" {
		fields = append(fields, [2]string{"permissions", opts.Permissions})
	}
	fields = append(fields, [2]string{"path", opts.Path})
	form, contentType, err := multipartForm(cmp.Or(path.Base(opts.Path), "file"), opts.Content, fields...)
	if err != nil {
		return err
	}
	// The generated client has no multipart form for this endpoint, since the
	// spec documents only a JSON body there. Retried, since a repeat writes
	// the same content.
	_, err = retryIdempotent(ctx, f.sandbox.retries.UploadMaxRetries, f.sandbox.retries.GatewayMaxRetries,
		func() (struct{}, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodPut,
				f.sandbox.api.BaseURL+"/filesystem/"+url.PathEscape(opts.Path), bytes.NewReader(form))
			if err != nil {
				return struct{}{}, err
			}
			for key, values := range f.sandbox.api.Headers {
				for _, value := range values {
					req.Header.Add(key, value)
				}
			}
			req.Header.Set("Content-Type", contentType)
			response, err := f.sandbox.api.HTTPClient.Do(req)
			if err != nil {
				return struct{}{}, err
			}
			defer response.Body.Close()
			body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
			if response.StatusCode < 200 || response.StatusCode > 299 {
				return struct{}{}, sandboxError(&sandboxapi.ResponseError{StatusCode: response.StatusCode, Body: string(body)})
			}
			return struct{}{}, nil
		})
	return err
}

// FileSystemMkdirOptions are the options for [FileSystemService.Mkdir].
type FileSystemMkdirOptions struct {
	// Path is the absolute path of the directory. Required.
	Path string

	// Permissions is the octal directory mode, such as "0755". Empty uses the
	// sandbox's default.
	Permissions string
}

// Mkdir creates a directory.
func (f *FileSystemService) Mkdir(ctx context.Context, opts FileSystemMkdirOptions) error {
	if opts.Path == "" {
		return errors.New("Path is required")
	}
	// Retried, since a repeat creates the same directory.
	_, err := retryIdempotent(ctx, f.sandbox.retries.UploadMaxRetries, f.sandbox.retries.GatewayMaxRetries,
		func() (*sandboxapi.SuccessResponse, error) {
			response, err := f.sandbox.api.PutFilesystem(ctx, opts.Path, sandboxapi.FileRequest{
				IsDirectory: optional(true),
				Permissions: optional(opts.Permissions),
			})
			return response, sandboxError(err)
		})
	return err
}

// FileSystemListOptions are the options for [FileSystemService.List].
type FileSystemListOptions struct {
	// Path is the absolute path of the directory. Required.
	Path string
}

// List lists a directory's files and subdirectories, one level deep.
func (f *FileSystemService) List(ctx context.Context, opts FileSystemListOptions) (*FileSystemDirectory, error) {
	if opts.Path == "" {
		return nil, errors.New("Path is required")
	}
	raw, err := f.get(ctx, opts.Path)
	if err != nil {
		return nil, err
	}
	var directory struct {
		sandboxapi.Directory
		// Present only on a directory, which tells it from a file.
		Files json.RawMessage `json:"files"`
	}
	if err := json.Unmarshal(raw, &directory); err != nil {
		return nil, err
	}
	if directory.Files == nil {
		return nil, fmt.Errorf("%s is a file, not a directory", opts.Path)
	}
	var files []sandboxapi.File
	if err := json.Unmarshal(directory.Files, &files); err != nil {
		return nil, err
	}
	result := &FileSystemDirectory{
		Name:           directory.Name,
		Path:           directory.Path,
		Files:          make([]FileSystemFileInfo, 0, len(files)),
		Subdirectories: make([]FileSystemSubdirectoryInfo, 0, len(directory.Subdirectories)),
	}
	for _, file := range files {
		lastModified, err := parseTimestamp(file.LastModified, "file modification time")
		if err != nil {
			return nil, err
		}
		result.Files = append(result.Files, FileSystemFileInfo{
			Name:         file.Name,
			Path:         file.Path,
			Size:         int64(file.Size),
			Permissions:  file.Permissions,
			Owner:        file.Owner,
			Group:        file.Group,
			LastModified: lastModified,
		})
	}
	for _, subdirectory := range directory.Subdirectories {
		result.Subdirectories = append(result.Subdirectories, FileSystemSubdirectoryInfo{
			Name: subdirectory.Name,
			Path: subdirectory.Path,
		})
	}
	return result, nil
}

// FileSystemRemoveOptions are the options for [FileSystemService.Remove].
type FileSystemRemoveOptions struct {
	// Path is the absolute path of the file or directory. Required.
	Path string

	// Recursive removes a directory and everything in it.
	Recursive bool
}

// Remove removes a file or directory.
func (f *FileSystemService) Remove(ctx context.Context, opts FileSystemRemoveOptions) error {
	if opts.Path == "" {
		return errors.New("Path is required")
	}
	// Not retried, since a repeat of a removal whose response was lost would
	// fail for a path that is already gone.
	_, err := f.sandbox.api.DeleteFilesystem(ctx, opts.Path, sandboxapi.DeleteFilesystemPathParams{
		Recursive: optional(opts.Recursive),
	})
	return sandboxError(err)
}

// FileSystemFindOptions are the options for [FileSystemService.Find].
type FileSystemFindOptions struct {
	// Path is the absolute path of the directory to search in. Required.
	Path string

	// Type finds only files or only directories. Empty finds both.
	Type FileSystemEntryType

	// Patterns are file patterns to include, such as "*.go".
	Patterns []string

	// MaxResults is the most results to return. Zero uses the sandbox's
	// default of 20, and a negative value returns all.
	MaxResults int

	// ExcludeDirs are directory names to skip. Empty uses the sandbox's
	// default of common dependency and build directories, such as
	// node_modules and .git.
	ExcludeDirs []string

	// IncludeHidden includes hidden files and directories, which the sandbox
	// skips by default.
	IncludeHidden bool
}

// Find finds files and directories by name under a directory.
func (f *FileSystemService) Find(ctx context.Context, opts FileSystemFindOptions) (*FileSystemFindResult, error) {
	if opts.Path == "" {
		return nil, errors.New("Path is required")
	}
	params := sandboxapi.GetFilesystemFindPathParams{
		Type:        optional(string(opts.Type)),
		Patterns:    optional(strings.Join(opts.Patterns, ",")),
		ExcludeDirs: optional(strings.Join(opts.ExcludeDirs, ",")),
	}
	if opts.MaxResults > 0 {
		params.MaxResults = optional(opts.MaxResults)
	} else if opts.MaxResults < 0 {
		// The sandbox treats 0 as no limit.
		params.MaxResults = new(int)
	}
	if opts.IncludeHidden {
		params.ExcludeHidden = new(bool)
	}
	result, err := retryIdempotent(ctx, f.sandbox.retries.ReadMaxRetries, f.sandbox.retries.GatewayMaxRetries,
		func() (*sandboxapi.FindResponse, error) {
			result, err := f.sandbox.api.GetFilesystemFind(ctx, opts.Path, params)
			return result, sandboxError(err)
		})
	if err != nil {
		return nil, err
	}
	// The server sends null rather than an empty list when nothing matches.
	matches := make([]FileSystemFindMatch, 0, len(result.Matches))
	for _, match := range result.Matches {
		matches = append(matches, FileSystemFindMatch{Path: match.Path, Type: FileSystemEntryType(match.Type)})
	}
	return &FileSystemFindResult{Matches: matches, Total: result.Total}, nil
}

// FileSystemGrepOptions are the options for [FileSystemService.Grep].
type FileSystemGrepOptions struct {
	// Path is the absolute path of the directory to search in. Required.
	Path string

	// Query is the text to search for. Required.
	Query string

	// CaseSensitive makes the search case sensitive.
	CaseSensitive bool

	// MaxResults is the most results to return. Zero uses the sandbox's
	// default of 100.
	MaxResults int

	// FilePattern is a file pattern to include, such as "*.go".
	FilePattern string

	// ExcludeDirs are directory names to skip. Empty uses the sandbox's
	// default of common dependency and build directories, such as
	// node_modules and .git.
	ExcludeDirs []string
}

// Grep searches the contents of files under a directory for text.
func (f *FileSystemService) Grep(ctx context.Context, opts FileSystemGrepOptions) (*FileSystemGrepResult, error) {
	if opts.Path == "" {
		return nil, errors.New("Path is required")
	}
	if opts.Query == "" {
		return nil, errors.New("Query is required")
	}
	params := sandboxapi.GetFilesystemContentSearchPathParams{
		Query:         opts.Query,
		CaseSensitive: optional(opts.CaseSensitive),
		MaxResults:    optional(opts.MaxResults),
		FilePattern:   optional(opts.FilePattern),
		ExcludeDirs:   optional(strings.Join(opts.ExcludeDirs, ",")),
	}
	result, err := retryIdempotent(ctx, f.sandbox.retries.ReadMaxRetries, f.sandbox.retries.GatewayMaxRetries,
		func() (*sandboxapi.ContentSearchResponse, error) {
			result, err := f.sandbox.api.GetFilesystemContentSearch(ctx, opts.Path, params)
			return result, sandboxError(err)
		})
	if err != nil {
		return nil, err
	}
	// The server sends null rather than an empty list when nothing matches.
	matches := make([]FileSystemGrepMatch, 0, len(result.Matches))
	for _, match := range result.Matches {
		matches = append(matches, FileSystemGrepMatch{
			Path:    match.Path,
			Line:    match.Line,
			Column:  match.Column,
			Text:    match.Text,
			Context: deref(match.Context),
		})
	}
	return &FileSystemGrepResult{Matches: matches, Total: result.Total}, nil
}

// FileSystemCopyOptions are the options for [FileSystemService.Copy].
type FileSystemCopyOptions struct {
	// Source is the absolute path of the file or directory to copy. Required.
	Source string

	// Destination is the absolute path to copy to. Required.
	Destination string

	// Timeout is how long to wait for the copy. Zero uses 180 seconds, and a
	// negative value waits with no limit. The copy keeps running regardless.
	Timeout time.Duration
}

// Copy copies a file or directory. It runs cp -r in the sandbox through
// [ProcessService.Exec], then waits for it with [ProcessService.Wait], so cp's
// rules apply, such as copying into a destination that is an existing
// directory.
//
// It returns a [*FileSystemCopyError] if the copy fails, and a
// [*ProcessWaitTimeoutError] if it outlasts the timeout, which does not stop
// it.
func (f *FileSystemService) Copy(ctx context.Context, opts FileSystemCopyOptions) error {
	if opts.Source == "" {
		return errors.New("Source is required")
	}
	if opts.Destination == "" {
		return errors.New("Destination is required")
	}
	started, err := f.sandbox.process.Exec(ctx, ProcessExecOptions{
		Command: "cp -r " + shellQuote(opts.Source) + " " + shellQuote(opts.Destination),
	})
	if err != nil {
		return err
	}
	finished, err := f.sandbox.process.Wait(ctx, ProcessWaitOptions{
		Identifier:   started.PID,
		Timeout:      cmp.Or(opts.Timeout, defaultCopyTimeout),
		PollInterval: copyPollInterval,
	})
	if err != nil {
		return err
	}
	if finished.Status != ProcessStatusCompleted || finished.ExitCode != 0 {
		return &FileSystemCopyError{Source: opts.Source, Destination: opts.Destination, Process: finished}
	}
	return nil
}

// FileSystemWriteTreeOptions are the options for [FileSystemService.WriteTree].
type FileSystemWriteTreeOptions struct {
	// Path is the absolute path of the directory to write under. Required.
	Path string

	// Files is the text content of each file, by path relative to Path.
	Files map[string]string
}

// WriteTree writes several text files under a directory in one call, creating
// or replacing each.
func (f *FileSystemService) WriteTree(ctx context.Context, opts FileSystemWriteTreeOptions) error {
	if opts.Path == "" {
		return errors.New("Path is required")
	}
	// Retried, since a repeat writes the same content.
	_, err := retryIdempotent(ctx, f.sandbox.retries.UploadMaxRetries, f.sandbox.retries.GatewayMaxRetries,
		func() (*sandboxapi.PutFilesystemTreeResponse, error) {
			response, err := f.sandbox.api.PutFilesystemTree(ctx, opts.Path, sandboxapi.TreeRequest{Files: optionalMap(opts.Files)})
			return response, sandboxError(err)
		})
	return err
}

// FileSystemWatchOptions are the options for [FileSystemService.Watch].
type FileSystemWatchOptions struct {
	// Path is the absolute path of the directory to watch. Required.
	Path string

	// Ignore are patterns of paths to ignore.
	Ignore []string
}

// Watch yields changes in a directory as they happen, until the iteration
// stops or ctx is done. Changes in its subdirectories are not included. An
// error is yielded as the second value and ends the iteration.
func (f *FileSystemService) Watch(ctx context.Context, opts FileSystemWatchOptions) iter.Seq2[FileSystemWatchEvent, error] {
	return func(yield func(FileSystemWatchEvent, error) bool) {
		if opts.Path == "" {
			yield(FileSystemWatchEvent{}, errors.New("Path is required"))
			return
		}
		// Not retried, since a resent watch would miss changes in between.
		response, err := f.sandbox.api.GetWatchFilesystemRaw(ctx, opts.Path, sandboxapi.GetWatchFilesystemPathParams{
			Ignore: optional(strings.Join(opts.Ignore, ",")),
		})
		if err != nil {
			yield(FileSystemWatchEvent{}, sandboxError(err))
			return
		}
		defer response.Body.Close()
		scanner := bufio.NewScanner(response.Body)
		scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "[keepalive]") {
				continue
			}
			var event struct {
				Op   string `json:"op"`
				Path string `json:"path"`
				Name string `json:"name"`
			}
			if err := json.Unmarshal([]byte(line), &event); err != nil {
				yield(FileSystemWatchEvent{}, fmt.Errorf("parsing watch event %q: %w", line, err))
				return
			}
			watchEvent := FileSystemWatchEvent{
				Op:   FileSystemWatchOp(event.Op),
				Path: strings.TrimSuffix(event.Path, "/") + "/" + event.Name,
			}
			if !yield(watchEvent, nil) {
				return
			}
		}
		if err := scanner.Err(); err != nil {
			yield(FileSystemWatchEvent{}, err)
		}
	}
}

// get fetches a path's JSON entry, a file with content or a directory listing.
func (f *FileSystemService) get(ctx context.Context, path string) (json.RawMessage, error) {
	result, err := retryIdempotent(ctx, f.sandbox.retries.ReadMaxRetries, f.sandbox.retries.GatewayMaxRetries,
		func() (*sandboxapi.GetFilesystemResponse, error) {
			result, err := f.sandbox.api.GetFilesystem(ctx, path, sandboxapi.GetFilesystemPathParams{})
			return result, sandboxError(err)
		})
	if err != nil {
		return nil, err
	}
	return result.MarshalJSON()
}

// writeMultipart uploads content in parts through the multipart upload
// endpoints.
func (f *FileSystemService) writeMultipart(ctx context.Context, path string, content []byte, permissions string) error {
	// Not retried, since a repeat would start a second upload.
	initiated, err := f.sandbox.api.PostFilesystemMultipartInitiate(ctx, path, sandboxapi.MultipartInitiateRequest{
		Permissions: optional(permissions),
	})
	if err != nil {
		return sandboxError(err)
	}
	uploadID := deref(initiated.UploadId)
	if uploadID == "" {
		return fmt.Errorf("multipart upload of %s returned no upload ID", path)
	}
	parts, err := f.sendParts(ctx, uploadID, content)
	if err == nil {
		// Not retried, since a repeat of a completion whose response was lost
		// would fail for an upload that is already complete.
		_, err = f.sandbox.api.PostFilesystemMultipartComplete(ctx, uploadID, sandboxapi.MultipartCompleteRequest{Parts: &parts})
		if err == nil {
			return nil
		}
		err = sandboxError(err)
	}
	// Sent even when ctx is done, which may be what ended the upload. Its own
	// failure is dropped in favor of the error that ended the upload.
	_, _ = f.sandbox.api.DeleteFilesystemMultipartAbort(context.WithoutCancel(ctx), uploadID)
	return err
}

// sendParts uploads every part of content. The first part to fail stops the
// rest.
func (f *FileSystemService) sendParts(ctx context.Context, uploadID string, content []byte) ([]sandboxapi.MultipartPartInfo, error) {
	partCount := (len(content) + multipartPartSize - 1) / multipartPartSize
	parts := make([]sandboxapi.MultipartPartInfo, partCount)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var next atomic.Int64
	var failOnce sync.Once
	var failure error
	var wg sync.WaitGroup
	// More workers than slots would only wait for one.
	for range min(uploadPartsInFlight, partCount) {
		wg.Go(func() {
			for {
				index := int(next.Add(1) - 1)
				if index >= partCount || ctx.Err() != nil {
					return
				}
				partNumber := index + 1
				etag, err := f.sendPart(ctx, uploadID, partNumber,
					content[index*multipartPartSize:min(partNumber*multipartPartSize, len(content))])
				if err != nil {
					failOnce.Do(func() {
						failure = err
						cancel()
					})
					return
				}
				parts[index] = sandboxapi.MultipartPartInfo{PartNumber: optional(partNumber), Etag: optional(etag)}
			}
		})
	}
	wg.Wait()
	if failure != nil {
		return nil, failure
	}
	// The caller's ctx ended between parts.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return parts, nil
}

// sendPart uploads one part and returns its ETag.
func (f *FileSystemService) sendPart(ctx context.Context, uploadID string, partNumber int, part []byte) (string, error) {
	// The filename FormData gives a part sent without one.
	form, contentType, err := multipartForm("blob", part)
	if err != nil {
		return "", err
	}
	// Retried, since a repeat sends the same part. The slot is taken per
	// attempt, so a part waiting to retry does not hold one.
	response, err := retryIdempotent(ctx, f.sandbox.retries.UploadMaxRetries, f.sandbox.retries.GatewayMaxRetries,
		func() (*sandboxapi.MultipartUploadPartResponse, error) {
			select {
			case f.partSlots <- struct{}{}:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			defer func() { <-f.partSlots }()
			response, err := f.sandbox.api.PutFilesystemMultipartPart(ctx, uploadID,
				sandboxapi.PutFilesystemMultipartUploadIdPartParams{PartNumber: partNumber},
				sandboxapi.AdvancedRequest{Body: bytes.NewReader(form), ContentType: contentType})
			return response, sandboxError(err)
		})
	if err != nil {
		return "", err
	}
	return deref(response.Etag), nil
}

// multipartForm encodes content as the form's file field, followed by the
// given name and value fields in order, and returns the form and its content
// type.
func multipartForm(filename string, content []byte, fields ...[2]string) ([]byte, string, error) {
	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	file, err := writer.CreateFormFile("file", filename)
	if err != nil {
		return nil, "", err
	}
	if _, err := file.Write(content); err != nil {
		return nil, "", err
	}
	for _, field := range fields {
		if err := writer.WriteField(field[0], field[1]); err != nil {
			return nil, "", err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return form.Bytes(), writer.FormDataContentType(), nil
}

// shellQuote quotes a string as one literal POSIX shell argument.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
