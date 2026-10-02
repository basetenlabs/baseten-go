package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/basetenlabs/baseten-go/client/managementapi"
	"github.com/basetenlabs/baseten-go/client/sandboxapi"
)

// APIError is an error response from the sandbox control plane or from a
// sandbox.
type APIError struct {
	// Status is the HTTP status of the response.
	Status int

	// Code is the machine-readable error code, empty when the response has
	// none.
	Code string

	// Details are additional error details, nil when the response has none.
	Details any

	// Body is the response body, parsed as JSON when possible, otherwise the
	// raw text.
	Body any

	description string
}

func (e *APIError) Error() string {
	message := fmt.Sprintf("sandbox API error (HTTP %d)", e.Status)
	if e.Code != "" {
		message += " " + e.Code
	}
	if e.description != "" {
		message += ": " + e.description
	}
	return message
}

// GatewayError is a 502, 503, or 504 from the edge in front of a sandbox
// rather than from the sandbox itself. Usually transient, for example while a
// sandbox wakes from standby or when a request outlasts the edge's timeout.
type GatewayError struct {
	APIError
}

// Unwrap returns the embedded APIError, so errors.As for *APIError matches
// gateway errors too.
func (e *GatewayError) Unwrap() error {
	return &e.APIError
}

// ImageUploadError is a failed upload of an image's source archive. The
// upload goes to storage rather than to the API, so its error body has no
// fixed shape.
type ImageUploadError struct {
	// ImageName is the name of the image whose source was being uploaded.
	ImageName string

	// Status is the HTTP status of the storage response.
	Status int

	// Body is the raw text of the storage response body.
	Body string
}

func (e *ImageUploadError) Error() string {
	return fmt.Sprintf("uploading the source of image %s failed (HTTP %d); "+
		"the image stays as it was left, so push it again or delete it", e.ImageName, e.Status)
}

// ImageBuildError is an image that did not become ready to use: either its
// build failed, or it was still processing when the wait ran out of time.
// [ImageClient.Logs] shows the build's output.
type ImageBuildError struct {
	// ImageName is the name of the image.
	ImageName string

	// Status is the status last seen, [ImageStatusFailed] when the build
	// failed.
	Status ImageStatus

	// TimedOut is whether the wait ran out of time. Processing continues
	// regardless.
	TimedOut bool
}

func (e *ImageBuildError) Error() string {
	if e.TimedOut {
		return fmt.Sprintf("image %s was still %s when the wait timed out; it may still finish", e.ImageName, e.Status)
	}
	return fmt.Sprintf("image %s failed to build (status %s)", e.ImageName, e.Status)
}

// ProcessWaitTimeoutError is a process still running when a wait for it ran
// out of time. The process keeps running regardless.
type ProcessWaitTimeoutError struct {
	// Identifier is the PID or name of the process waited on.
	Identifier string

	// Err is the last error seen while polling, nil when the last poll
	// succeeded.
	Err error
}

func (e *ProcessWaitTimeoutError) Error() string {
	return fmt.Sprintf("process %s had not finished when the wait timed out; it may still be running", e.Identifier)
}

// Unwrap returns the last error seen while polling.
func (e *ProcessWaitTimeoutError) Unwrap() error {
	return e.Err
}

// FileSystemCopyError is a [FileSystemService.Copy] whose cp process did not
// complete successfully.
type FileSystemCopyError struct {
	// Source is the path copied from.
	Source string

	// Destination is the path copied to.
	Destination string

	// Process is the cp process as it ended, whose Stderr holds its error
	// output.
	Process *ProcessInfo
}

func (e *FileSystemCopyError) Error() string {
	detail := strings.TrimSpace(e.Process.Stderr)
	if detail == "" {
		detail = fmt.Sprintf("exit code %d", e.Process.ExitCode)
	}
	return fmt.Sprintf("copying %s to %s ended %s: %s", e.Source, e.Destination, e.Process.Status, detail)
}

func newAPIError(status int, body any) APIError {
	fields, _ := body.(map[string]any)
	bodyMessage, hasMessage := fields["message"].(string)
	bodyError, _ := fields["error"].(string)
	// The control plane puts the code in error and the description in
	// message. A sandbox puts the description in error.
	code, _ := fields["code"].(string)
	if code == "" && hasMessage {
		code = bodyError
	}
	description := bodyError
	if hasMessage {
		description = bodyMessage
	}
	return APIError{
		Status:      status,
		Code:        code,
		Details:     fields["details"],
		Body:        body,
		description: description,
	}
}

// controlPlaneError converts an error from the generated management client
// into an *APIError. Anything else, such as a network failure, is returned
// unchanged.
func controlPlaneError(err error) error {
	var responseErr *managementapi.ResponseError
	if !errors.As(err, &responseErr) {
		return err
	}
	apiErr := newAPIError(responseErr.StatusCode, parseErrorBody(responseErr.Body))
	return &apiErr
}

// sandboxError converts an error from the generated sandbox client into an
// *APIError, or a *GatewayError for the edge's gateway statuses. Anything
// else, such as a network failure, is returned unchanged.
func sandboxError(err error) error {
	var status int
	var body any
	var responseErr *sandboxapi.ResponseError
	var typedErr *sandboxapi.ResponseErrorResponse
	switch {
	case errors.As(err, &responseErr):
		status = responseErr.StatusCode
		body = parseErrorBody(responseErr.Body)
	case errors.As(err, &typedErr):
		status = typedErr.StatusCode
		// Round-tripped so the typed body has the same shape as a parsed
		// one, giving code extraction one contract.
		raw, _ := json.Marshal(typedErr.ErrorResponse)
		body = parseErrorBody(string(raw))
	default:
		return err
	}
	apiErr := newAPIError(status, body)
	switch status {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return &GatewayError{APIError: apiErr}
	}
	return &apiErr
}

func parseErrorBody(body string) any {
	var parsed any
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		return body
	}
	return parsed
}
