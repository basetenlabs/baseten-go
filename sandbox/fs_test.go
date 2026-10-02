package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-go/internal/require"
)

// fsRequest is what the fake execution API saw of one request.
type fsRequest struct {
	method      string
	path        string
	query       string
	contentType string
	body        []byte
}

// fsSandbox returns a Sandbox whose execution API is handler, and a func
// returning every request handler got, in order. Assertions belong in the
// test, not in handler, which runs on the server's goroutine.
func fsSandbox(t *testing.T, handler http.HandlerFunc) (*Sandbox, func() []fsRequest) {
	t.Helper()
	var mu sync.Mutex
	var requests []fsRequest
	sb, _ := execSandbox(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		requests = append(requests, fsRequest{
			method:      r.Method,
			path:        r.URL.EscapedPath(),
			query:       r.URL.RawQuery,
			contentType: r.Header.Get("Content-Type"),
			body:        body,
		})
		mu.Unlock()
		handler(w, r)
	})
	return sb, func() []fsRequest {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(requests)
	}
}

// formFile returns a multipart request's file field and its other fields.
func formFile(t *testing.T, request fsRequest) (filename string, content []byte, fields map[string]string) {
	t.Helper()
	_, params, err := mime.ParseMediaType(request.contentType)
	require.NoError(t, err)
	reader := multipart.NewReader(bytes.NewReader(request.body), params["boundary"])
	fields = map[string]string{}
	var order []string
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		value, err := io.ReadAll(part)
		require.NoError(t, err)
		order = append(order, part.FormName())
		if part.FormName() == "file" {
			filename, content = part.FileName(), value
		} else {
			fields[part.FormName()] = string(value)
		}
	}
	require.Equal(t, "file", order[0])
	return filename, content, fields
}

func TestFileSystemRead(t *testing.T) {
	t.Run("ReadsText", func(t *testing.T) {
		sb, requests := fsSandbox(t, func(w http.ResponseWriter, _ *http.Request) {
			respondJSON(w, http.StatusOK, `{"name":"a b%.txt","path":"/tmp/a b%.txt","content":"hello"}`)
		})
		content, err := sb.FS().Read(t.Context(), FileSystemReadOptions{Path: "/tmp/a b%.txt"})
		require.NoError(t, err)
		require.Equal(t, "hello", content)
		require.Equal(t, "/filesystem/%2Ftmp%2Fa%20b%25.txt", requests()[0].path)
	})

	t.Run("DirectoryFails", func(t *testing.T) {
		sb, _ := fsSandbox(t, func(w http.ResponseWriter, _ *http.Request) {
			respondJSON(w, http.StatusOK, `{"name":"tmp","path":"/tmp","files":[],"subdirectories":[]}`)
		})
		_, err := sb.FS().Read(t.Context(), FileSystemReadOptions{Path: "/tmp"})
		require.Error(t, err)
		require.Contains(t, err.Error(), "is a directory")
	})
}

func TestFileSystemReadBytes(t *testing.T) {
	t.Run("ReadsOctets", func(t *testing.T) {
		var accept atomic.Value
		sb, _ := fsSandbox(t, func(w http.ResponseWriter, r *http.Request) {
			accept.Store(r.Header.Get("Accept"))
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write([]byte{0, 1, 0xff})
		})
		content, err := sb.FS().ReadBytes(t.Context(), FileSystemReadBytesOptions{Path: "/tmp/bin"})
		require.NoError(t, err)
		require.True(t, bytes.Equal([]byte{0, 1, 0xff}, content), "content %v", content)
		require.Equal(t, "application/octet-stream", accept.Load().(string))
	})

	t.Run("DirectoryFails", func(t *testing.T) {
		sb, _ := fsSandbox(t, func(w http.ResponseWriter, _ *http.Request) {
			respondJSON(w, http.StatusOK, `{"name":"tmp","path":"/tmp","files":[],"subdirectories":[]}`)
		})
		_, err := sb.FS().ReadBytes(t.Context(), FileSystemReadBytesOptions{Path: "/tmp"})
		require.Error(t, err)
		require.Contains(t, err.Error(), "is a directory")
	})
}

func TestFileSystemWrite(t *testing.T) {
	t.Run("RetriesGatewayError", func(t *testing.T) {
		shortBackoff(t)
		var calls atomic.Int32
		sb, requests := fsSandbox(t, func(w http.ResponseWriter, _ *http.Request) {
			if calls.Add(1) == 1 {
				respondJSON(w, http.StatusBadGateway, `{}`)
				return
			}
			respondJSON(w, http.StatusOK, `{"message":"ok"}`)
		})
		require.NoError(t, sb.FS().Write(t.Context(), FileSystemWriteOptions{Path: "/tmp/a", Content: ""}))
		got := requests()
		require.Len(t, got, 2)
		require.Equal(t, http.MethodPut, got[1].method)
		// Empty content is still sent, so the file ends up empty.
		require.Equal(t, `{"content":""}`, string(got[1].body))
	})

	t.Run("UpToThresholdInOneRequest", func(t *testing.T) {
		sb, requests := fsSandbox(t, func(w http.ResponseWriter, _ *http.Request) {
			respondJSON(w, http.StatusOK, `{"message":"ok"}`)
		})
		content := strings.Repeat("a", multipartThreshold)
		require.NoError(t, sb.FS().Write(t.Context(), FileSystemWriteOptions{Path: "/tmp/a", Content: content}))
		require.Len(t, requests(), 1)
	})

	t.Run("OverThresholdInPartsWithoutPermissions", func(t *testing.T) {
		sb, requests := fsSandbox(t, multipartHandler(nil))
		content := strings.Repeat("é", multipartThreshold/2+1)
		require.NoError(t, sb.FS().Write(t.Context(), FileSystemWriteOptions{Path: "/tmp/a", Content: content}))
		got := requests()
		require.Equal(t, "/filesystem-multipart/initiate/%2Ftmp%2Fa", got[0].path)
		require.Equal(t, `{}`, string(got[0].body))
		require.True(t, bytes.Equal([]byte(content), uploadedParts(t, got)), "uploaded content differs")
	})
}

func TestFileSystemWriteBytes(t *testing.T) {
	t.Run("SendsFormRebuiltPerAttempt", func(t *testing.T) {
		shortBackoff(t)
		var calls atomic.Int32
		sb, requests := fsSandbox(t, func(w http.ResponseWriter, _ *http.Request) {
			if calls.Add(1) == 1 {
				respondJSON(w, http.StatusServiceUnavailable, `{}`)
				return
			}
			respondJSON(w, http.StatusOK, `{"message":"ok"}`)
		})
		require.NoError(t, sb.FS().WriteBytes(t.Context(), FileSystemWriteBytesOptions{
			Path:        "/tmp/run.sh",
			Content:     []byte{1, 2, 3},
			Permissions: "0755",
		}))
		got := requests()
		require.Len(t, got, 2)
		for _, request := range got {
			require.Equal(t, http.MethodPut, request.method)
			require.Equal(t, "/filesystem/%2Ftmp%2Frun.sh", request.path)
			filename, content, fields := formFile(t, request)
			require.Equal(t, "run.sh", filename)
			require.True(t, bytes.Equal([]byte{1, 2, 3}, content), "content %v", content)
			require.MapEqual(t, fields, "permissions", "0755")
			require.MapEqual(t, fields, "path", "/tmp/run.sh")
		}
	})

	t.Run("NoPermissionsWhenUnset", func(t *testing.T) {
		sb, requests := fsSandbox(t, func(w http.ResponseWriter, _ *http.Request) {
			respondJSON(w, http.StatusOK, `{"message":"ok"}`)
		})
		require.NoError(t, sb.FS().WriteBytes(t.Context(), FileSystemWriteBytesOptions{Path: "/tmp/a", Content: []byte("x")}))
		_, _, fields := formFile(t, requests()[0])
		_, hasPermissions := fields["permissions"]
		require.False(t, hasPermissions, "permissions sent")
	})

	t.Run("SandboxErrorFails", func(t *testing.T) {
		sb, _ := fsSandbox(t, func(w http.ResponseWriter, _ *http.Request) {
			respondJSON(w, http.StatusUnprocessableEntity, `{"error":"bad path"}`)
		})
		err := sb.FS().WriteBytes(t.Context(), FileSystemWriteBytesOptions{Path: "/tmp/a", Content: []byte("x")})
		apiErr := require.ErrorAs[*APIError](t, err)
		require.Equal(t, http.StatusUnprocessableEntity, apiErr.Status)
		require.Contains(t, apiErr.Error(), "bad path")
	})

	t.Run("OverThresholdInPartsEveryByteOnce", func(t *testing.T) {
		sb, requests := fsSandbox(t, multipartHandler(nil))
		content := make([]byte, 2*multipartPartSize+17)
		for i := range content {
			content[i] = byte(i % 251)
		}
		require.NoError(t, sb.FS().WriteBytes(t.Context(), FileSystemWriteBytesOptions{
			Path:        "/tmp/big",
			Content:     content,
			Permissions: "0755",
		}))
		got := requests()
		require.Equal(t, `{"permissions":"0755"}`, string(got[0].body))
		require.True(t, bytes.Equal(content, uploadedParts(t, got)), "uploaded content differs")
		complete := got[len(got)-1]
		require.Equal(t, "/filesystem-multipart/upload-1/complete", complete.path)
		require.Equal(t, `{"parts":[{"etag":"etag-1","partNumber":1},{"etag":"etag-2","partNumber":2},{"etag":"etag-3","partNumber":3}]}`,
			string(complete.body))
	})

	t.Run("ExactlyThresholdInOneRequest", func(t *testing.T) {
		sb, requests := fsSandbox(t, func(w http.ResponseWriter, _ *http.Request) {
			respondJSON(w, http.StatusOK, `{"message":"ok"}`)
		})
		require.NoError(t, sb.FS().WriteBytes(t.Context(), FileSystemWriteBytesOptions{
			Path:    "/tmp/a",
			Content: make([]byte, multipartThreshold),
		}))
		require.Len(t, requests(), 1)
	})

	t.Run("RetriesPartAfterGatewayError", func(t *testing.T) {
		shortBackoff(t)
		var failed atomic.Bool
		sb, requests := fsSandbox(t, multipartHandler(func(w http.ResponseWriter, partNumber string) bool {
			if partNumber == "2" && !failed.Swap(true) {
				respondJSON(w, http.StatusBadGateway, `{}`)
				return true
			}
			return false
		}))
		content := make([]byte, multipartPartSize+1)
		content[multipartPartSize] = 7
		require.NoError(t, sb.FS().WriteBytes(t.Context(), FileSystemWriteBytesOptions{Path: "/tmp/a", Content: content}))
		var partTwo [][]byte
		for _, request := range requests() {
			if strings.HasSuffix(request.path, "/part") && request.query == "partNumber=2" {
				_, part, _ := formFile(t, request)
				partTwo = append(partTwo, part)
			}
		}
		require.Len(t, partTwo, 2)
		for _, part := range partTwo {
			require.True(t, bytes.Equal([]byte{7}, part), "part %v", part)
		}
	})

	t.Run("AbortsWhenPartFails", func(t *testing.T) {
		sb, requests := fsSandbox(t, multipartHandler(func(w http.ResponseWriter, partNumber string) bool {
			if partNumber == "1" {
				respondJSON(w, http.StatusInternalServerError, `{"error":"disk full"}`)
				return true
			}
			return false
		}))
		err := sb.FS().WriteBytes(t.Context(), FileSystemWriteBytesOptions{Path: "/tmp/a", Content: make([]byte, multipartPartSize+1)})
		apiErr := require.ErrorAs[*APIError](t, err)
		require.Contains(t, apiErr.Error(), "disk full")
		got := requests()
		require.Equal(t, "/filesystem-multipart/upload-1/abort", got[len(got)-1].path)
		for _, request := range got {
			require.False(t, strings.HasSuffix(request.path, "/complete"), "completed after a failed part")
		}
	})

	t.Run("AbortsWhenCompleteFailsKeepingItsError", func(t *testing.T) {
		sb, requests := fsSandbox(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/complete"):
				respondJSON(w, http.StatusInternalServerError, `{"error":"complete failed"}`)
			case strings.HasSuffix(r.URL.Path, "/abort"):
				respondJSON(w, http.StatusInternalServerError, `{"error":"abort failed"}`)
			default:
				multipartHandler(nil)(w, r)
			}
		})
		err := sb.FS().WriteBytes(t.Context(), FileSystemWriteBytesOptions{Path: "/tmp/a", Content: make([]byte, multipartPartSize+1)})
		apiErr := require.ErrorAs[*APIError](t, err)
		require.Contains(t, apiErr.Error(), "complete failed")
		got := requests()
		require.Equal(t, "/filesystem-multipart/upload-1/abort", got[len(got)-1].path)
	})

	t.Run("AbortsEvenWhenContextCanceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		sb, requests := fsSandbox(t, multipartHandler(func(w http.ResponseWriter, _ string) bool {
			cancel()
			respondJSON(w, http.StatusInternalServerError, `{"error":"canceled"}`)
			return true
		}))
		err := sb.FS().WriteBytes(ctx, FileSystemWriteBytesOptions{Path: "/tmp/a", Content: make([]byte, multipartPartSize+1)})
		require.Error(t, err)
		got := requests()
		require.Equal(t, "/filesystem-multipart/upload-1/abort", got[len(got)-1].path)
	})

	t.Run("AtMostTwoPartsInFlightPerSandbox", func(t *testing.T) {
		const uploads = 2
		const partsPerUpload = 3
		var inFlight, maxInFlight atomic.Int32
		arrived := make(chan struct{}, uploads*partsPerUpload)
		release := make(chan struct{})
		sb, _ := fsSandbox(t, multipartHandler(func(_ http.ResponseWriter, _ string) bool {
			n := inFlight.Add(1)
			for current := maxInFlight.Load(); n > current && !maxInFlight.CompareAndSwap(current, n); current = maxInFlight.Load() {
			}
			arrived <- struct{}{}
			<-release
			inFlight.Add(-1)
			return false
		}))
		errs := make(chan error, uploads)
		for i := range uploads {
			go func() {
				errs <- sb.FS().WriteBytes(t.Context(), FileSystemWriteBytesOptions{
					Path:    fmt.Sprintf("/tmp/%d", i),
					Content: make([]byte, 2*multipartPartSize+1),
				})
			}()
		}
		// Two parts are held from the start, and each release lets one
		// waiting part in, so parts beyond the limit would arrive while two
		// are held.
		<-arrived
		<-arrived
		for range uploads*partsPerUpload - 2 {
			release <- struct{}{}
			<-arrived
		}
		release <- struct{}{}
		release <- struct{}{}
		for range uploads {
			require.NoError(t, <-errs)
		}
		require.Equal(t, int32(2), maxInFlight.Load())
	})
}

// multipartHandler fakes the multipart upload endpoints, numbering uploads from
// 1 and answering each part with "etag-<n>". part, when set, runs first for
// each part and returns true if it wrote the response itself.
func multipartHandler(part func(w http.ResponseWriter, partNumber string) bool) http.HandlerFunc {
	var uploads atomic.Int32
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/filesystem-multipart/initiate/"):
			respondJSON(w, http.StatusOK, fmt.Sprintf(`{"uploadId":"upload-%d"}`, uploads.Add(1)))
		case strings.HasSuffix(r.URL.Path, "/part"):
			partNumber := r.URL.Query().Get("partNumber")
			if part != nil && part(w, partNumber) {
				return
			}
			respondJSON(w, http.StatusOK, fmt.Sprintf(`{"etag":"etag-%s","partNumber":%s}`, partNumber, partNumber))
		default:
			respondJSON(w, http.StatusOK, `{"message":"ok"}`)
		}
	}
}

// uploadedParts joins the content of every successful part request in part
// number order, each part once.
func uploadedParts(t *testing.T, requests []fsRequest) []byte {
	t.Helper()
	parts := map[int][]byte{}
	for _, request := range requests {
		if !strings.HasSuffix(request.path, "/part") {
			continue
		}
		var partNumber int
		_, err := fmt.Sscanf(request.query, "partNumber=%d", &partNumber)
		require.NoError(t, err)
		_, content, _ := formFile(t, request)
		parts[partNumber] = content
	}
	var joined []byte
	for i := 1; i <= len(parts); i++ {
		part, ok := parts[i]
		require.True(t, ok, "part %d missing", i)
		require.True(t, len(part) <= multipartPartSize, "part %d is %d bytes", i, len(part))
		joined = append(joined, part...)
	}
	return joined
}

func TestFileSystemMkdir(t *testing.T) {
	sb, requests := fsSandbox(t, func(w http.ResponseWriter, _ *http.Request) {
		respondJSON(w, http.StatusOK, `{"message":"ok"}`)
	})
	require.NoError(t, sb.FS().Mkdir(t.Context(), FileSystemMkdirOptions{Path: "/tmp/d"}))
	require.NoError(t, sb.FS().Mkdir(t.Context(), FileSystemMkdirOptions{Path: "/tmp/d", Permissions: "0700"}))
	got := requests()
	require.Equal(t, `{"isDirectory":true}`, string(got[0].body))
	require.Equal(t, `{"isDirectory":true,"permissions":"0700"}`, string(got[1].body))
}

func TestFileSystemList(t *testing.T) {
	t.Run("ListsDirectory", func(t *testing.T) {
		sb, _ := fsSandbox(t, func(w http.ResponseWriter, _ *http.Request) {
			respondJSON(w, http.StatusOK, `{"name":"tmp","path":"/tmp",`+
				`"files":[{"name":"a","path":"/tmp/a","size":3,"permissions":"-rw-r--r--","owner":"root","group":"root",`+
				`"lastModified":"2026-10-01T00:00:00Z"}],`+
				`"subdirectories":[{"name":"d","path":"/tmp/d"}]}`)
		})
		directory, err := sb.FS().List(t.Context(), FileSystemListOptions{Path: "/tmp"})
		require.NoError(t, err)
		require.Equal(t, "/tmp", directory.Path)
		require.Len(t, directory.Files, 1)
		require.Equal(t, FileSystemFileInfo{
			Name:         "a",
			Path:         "/tmp/a",
			Size:         3,
			Permissions:  "-rw-r--r--",
			Owner:        "root",
			Group:        "root",
			LastModified: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		}, directory.Files[0])
		require.Len(t, directory.Subdirectories, 1)
		require.Equal(t, FileSystemSubdirectoryInfo{Name: "d", Path: "/tmp/d"}, directory.Subdirectories[0])
	})

	t.Run("FileFails", func(t *testing.T) {
		sb, _ := fsSandbox(t, func(w http.ResponseWriter, _ *http.Request) {
			respondJSON(w, http.StatusOK, `{"name":"a","path":"/tmp/a","content":"x"}`)
		})
		_, err := sb.FS().List(t.Context(), FileSystemListOptions{Path: "/tmp/a"})
		require.Error(t, err)
		require.Contains(t, err.Error(), "is a file")
	})
}

func TestFileSystemRemove(t *testing.T) {
	shortBackoff(t)
	sb, requests := fsSandbox(t, func(w http.ResponseWriter, _ *http.Request) {
		respondJSON(w, http.StatusBadGateway, `{}`)
	})
	err := sb.FS().Remove(t.Context(), FileSystemRemoveOptions{Path: "/tmp/a"})
	require.ErrorAs[*GatewayError](t, err)
	err = sb.FS().Remove(t.Context(), FileSystemRemoveOptions{Path: "/tmp/d", Recursive: true})
	require.ErrorAs[*GatewayError](t, err)
	got := requests()
	// Not retried.
	require.Len(t, got, 2)
	require.Equal(t, http.MethodDelete, got[0].method)
	require.Equal(t, "", got[0].query)
	require.Equal(t, "recursive=true", got[1].query)
}

func TestFileSystemFind(t *testing.T) {
	t.Run("MapsEveryOption", func(t *testing.T) {
		sb, requests := fsSandbox(t, func(w http.ResponseWriter, _ *http.Request) {
			respondJSON(w, http.StatusOK, `{"matches":[{"path":"a.go","type":"file"}],"total":7}`)
		})
		result, err := sb.FS().Find(t.Context(), FileSystemFindOptions{
			Path:          "/src",
			Type:          FileSystemEntryTypeFile,
			Patterns:      []string{"*.go", "*.mod"},
			MaxResults:    5,
			ExcludeDirs:   []string{"vendor", ".git"},
			IncludeHidden: true,
		})
		require.NoError(t, err)
		require.Equal(t, "excludeDirs=vendor%2C.git&excludeHidden=false&maxResults=5&patterns=%2A.go%2C%2A.mod&type=file",
			requests()[0].query)
		require.Len(t, result.Matches, 1)
		require.Equal(t, FileSystemFindMatch{Path: "a.go", Type: FileSystemEntryTypeFile}, result.Matches[0])
		require.Equal(t, 7, result.Total)
	})

	t.Run("NothingSentWhenUnsetAndNegativeMaxResultsIsAll", func(t *testing.T) {
		sb, requests := fsSandbox(t, func(w http.ResponseWriter, _ *http.Request) {
			respondJSON(w, http.StatusOK, `{"matches":null,"total":0}`)
		})
		result, err := sb.FS().Find(t.Context(), FileSystemFindOptions{Path: "/src"})
		require.NoError(t, err)
		// The server sends null for no matches.
		require.True(t, result.Matches != nil, "nil matches")
		require.Len(t, result.Matches, 0)
		_, err = sb.FS().Find(t.Context(), FileSystemFindOptions{Path: "/src", MaxResults: -1})
		require.NoError(t, err)
		got := requests()
		require.Equal(t, "", got[0].query)
		require.Equal(t, "maxResults=0", got[1].query)
	})
}

func TestFileSystemGrep(t *testing.T) {
	t.Run("MapsEveryOption", func(t *testing.T) {
		sb, requests := fsSandbox(t, func(w http.ResponseWriter, _ *http.Request) {
			respondJSON(w, http.StatusOK, `{"query":"TODO","total":1,`+
				`"matches":[{"path":"a.go","line":3,"column":4,"text":"// TODO","context":"x\n// TODO"}]}`)
		})
		result, err := sb.FS().Grep(t.Context(), FileSystemGrepOptions{
			Path:          "/src",
			Query:         "TODO",
			CaseSensitive: true,
			MaxResults:    9,
			FilePattern:   "*.go",
			ExcludeDirs:   []string{"vendor"},
		})
		require.NoError(t, err)
		require.Equal(t, "caseSensitive=true&excludeDirs=vendor&filePattern=%2A.go&maxResults=9&query=TODO", requests()[0].query)
		require.Len(t, result.Matches, 1)
		require.Equal(t, FileSystemGrepMatch{Path: "a.go", Line: 3, Column: 4, Text: "// TODO", Context: "x\n// TODO"}, result.Matches[0])
	})

	t.Run("NullMatchesIsEmpty", func(t *testing.T) {
		sb, requests := fsSandbox(t, func(w http.ResponseWriter, _ *http.Request) {
			respondJSON(w, http.StatusOK, `{"query":"x","matches":null,"total":0}`)
		})
		result, err := sb.FS().Grep(t.Context(), FileSystemGrepOptions{Path: "/src", Query: "x"})
		require.NoError(t, err)
		require.True(t, result.Matches != nil, "nil matches")
		require.Len(t, result.Matches, 0)
		require.Equal(t, "query=x", requests()[0].query)
	})
}

func TestFileSystemCopy(t *testing.T) {
	copySandbox := func(t *testing.T, finished string) (*Sandbox, func() []fsRequest) {
		return fsSandbox(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				respondJSON(w, http.StatusOK, strings.Replace(processRecord, `"status":"completed"`, `"status":"running"`, 1))
				return
			}
			respondJSON(w, http.StatusOK, finished)
		})
	}

	t.Run("RunsQuotedCpThenWaits", func(t *testing.T) {
		sb, requests := copySandbox(t, processRecord)
		require.NoError(t, sb.FS().Copy(t.Context(), FileSystemCopyOptions{Source: "/tmp/it's", Destination: "/tmp/b c"}))
		got := requests()
		var exec map[string]any
		require.NoError(t, json.Unmarshal(got[0].body, &exec))
		require.Equal(t, `cp -r '/tmp/it'\''s' '/tmp/b c'`, exec["command"].(string))
		_, waited := exec["waitForCompletion"]
		require.False(t, waited, "copy waited server side")
		require.Equal(t, "/process/12", got[1].path)
	})

	t.Run("FailedCpFails", func(t *testing.T) {
		failed := strings.NewReplacer(`"status":"completed"`, `"status":"failed"`, `"exitCode":0`, `"exitCode":1`,
			`"stderr":""`, `"stderr":"cp: no such file\n"`).Replace(processRecord)
		sb, _ := copySandbox(t, failed)
		err := sb.FS().Copy(t.Context(), FileSystemCopyOptions{Source: "/a", Destination: "/b"})
		copyErr := require.ErrorAs[*FileSystemCopyError](t, err)
		require.Equal(t, ProcessStatusFailed, copyErr.Process.Status)
		require.Equal(t, "copying /a to /b ended failed: cp: no such file", copyErr.Error())
	})

	t.Run("KilledCpFails", func(t *testing.T) {
		sb, _ := copySandbox(t, strings.Replace(processRecord, `"status":"completed"`, `"status":"killed"`, 1))
		err := sb.FS().Copy(t.Context(), FileSystemCopyOptions{Source: "/a", Destination: "/b"})
		copyErr := require.ErrorAs[*FileSystemCopyError](t, err)
		require.Equal(t, "copying /a to /b ended killed: exit code 0", copyErr.Error())
	})
}

func TestFileSystemWriteTree(t *testing.T) {
	shortBackoff(t)
	var calls atomic.Int32
	sb, requests := fsSandbox(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			respondJSON(w, http.StatusGatewayTimeout, `{}`)
			return
		}
		respondJSON(w, http.StatusOK, `{"name":"app","path":"/app","files":[],"subdirectories":[]}`)
	})
	require.NoError(t, sb.FS().WriteTree(t.Context(), FileSystemWriteTreeOptions{
		Path:  "/app",
		Files: map[string]string{"src/a.txt": "a"},
	}))
	got := requests()
	require.Len(t, got, 2)
	require.Equal(t, "/filesystem/tree/%2Fapp", got[1].path)
	require.Equal(t, `{"files":{"src/a.txt":"a"}}`, string(got[1].body))
}

func TestFileSystemWatch(t *testing.T) {
	t.Run("JoinsDirectoryAndName", func(t *testing.T) {
		sb, requests := fsSandbox(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Write([]byte("[keepalive]\n" +
				`{"op":"CREATE","path":"/tmp","name":"a"}` + "\n\n" +
				`{"op":"WRITE","path":"/tmp/","name":"b"}` + "\n"))
		})
		var events []string
		for event, err := range sb.FS().Watch(t.Context(), FileSystemWatchOptions{Path: "/tmp", Ignore: []string{"*.log", "x"}}) {
			require.NoError(t, err)
			events = append(events, string(event.Op)+" "+event.Path)
		}
		require.Equal(t, "CREATE /tmp/a,WRITE /tmp/b", strings.Join(events, ","))
		require.Equal(t, "ignore=%2A.log%2Cx", requests()[0].query)
	})

	t.Run("StopsStreamWhenIterationStops", func(t *testing.T) {
		done := make(chan struct{})
		sb, _ := fsSandbox(t, func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"op":"CREATE","path":"/tmp","name":"a"}` + "\n"))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			close(done)
		})
		for _, err := range sb.FS().Watch(t.Context(), FileSystemWatchOptions{Path: "/tmp"}) {
			require.NoError(t, err)
			break
		}
		<-done
	})

	t.Run("UnparseableLineFails", func(t *testing.T) {
		sb, _ := fsSandbox(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Write([]byte("/tmp/a\n"))
		})
		var err error
		for _, err = range sb.FS().Watch(t.Context(), FileSystemWatchOptions{Path: "/tmp"}) {
		}
		require.Error(t, err)
	})
}
