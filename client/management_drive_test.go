package client

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-go/client/managementapi"
	"github.com/basetenlabs/baseten-go/internal/require"
)

// TODO: Change to /v1/drives/instances when drivePathRewriteDoer is removed.
const driveRoute = "/v1/sandboxes/drives"

const driveRecord = `{"name":"data","region":"us-was-1","status":"DEPLOYED","labels":{"k":"v"},` +
	`"created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-02T00:00:00Z","created_by":"me","updated_by":"you"}`

type driveRequest struct {
	method        string
	path          string
	query         string
	authorization string
	body          string
}

type driveResponse struct {
	status int
	header http.Header
	body   string
}

// driveServer is a fake management API: it mints "token-1", "token-2", and so
// on, records every other request, and answers each with the next queued
// response for its method and path, or a 404.
type driveServer struct {
	*httptest.Server
	mu        sync.Mutex
	mints     []driveRequest
	requests  []driveRequest
	responses map[string][]driveResponse
}

func newDriveServer(t *testing.T) *driveServer {
	t.Helper()
	s := &driveServer{responses: map[string][]driveResponse{}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		recorded := driveRequest{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"), string(body)}
		s.mu.Lock()
		defer s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/token" {
			s.mints = append(s.mints, recorded)
			json.NewEncoder(w).Encode(map[string]any{
				"token":      "token-" + string(rune('0'+len(s.mints))),
				"expires_at": time.Now().Add(time.Hour),
			})
			return
		}
		s.requests = append(s.requests, recorded)
		key := r.Method + " " + r.URL.Path
		queued := s.responses[key]
		if len(queued) == 0 {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"code":"NOT_FOUND","message":"not found"}`))
			return
		}
		s.responses[key] = queued[1:]
		for name, values := range queued[0].header {
			w.Header()[name] = values
		}
		w.WriteHeader(queued[0].status)
		w.Write([]byte(queued[0].body))
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *driveServer) respond(method, path string, status int, body string) {
	s.respondWithHeader(method, path, status, nil, body)
}

func (s *driveServer) respondWithHeader(method, path string, status int, header http.Header, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := method + " " + path
	s.responses[key] = append(s.responses[key], driveResponse{status, header, body})
}

func (s *driveServer) drives(t *testing.T) *DriveClient {
	t.Helper()
	management, err := NewManagementClient(ManagementClientOptions{APIKey: "key", BaseURL: s.URL})
	require.NoError(t, err)
	return management.Drives()
}

func (s *driveServer) lastRequest(t *testing.T) driveRequest {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	require.True(t, len(s.requests) > 0, "no request recorded")
	return s.requests[len(s.requests)-1]
}

func TestDriveCreate(t *testing.T) {
	s := newDriveServer(t)
	s.respond("POST", driveRoute, 201, driveRecord)
	info, err := s.drives(t).Create(t.Context(), DriveCreateOptions{
		TeamID: "team-1",
		Name:   "data",
		Region: "us-was-1",
		Labels: map[string]string{},
	})
	require.NoError(t, err)
	request := s.lastRequest(t)
	require.Equal(t, "team_id=team-1", request.query)
	require.Equal(t, `{"labels":{},"name":"data","region":"us-was-1"}`, request.body)
	require.Equal(t, "data", info.Name)
	require.Equal(t, "us-was-1", info.Region)
	require.Equal(t, DriveStatusDeployed, info.Status)
	require.MapEqual(t, info.Labels, "k", "v")
	require.Equal(t, "2026-10-01T00:00:00Z", info.CreatedAt.Format(time.RFC3339))
	require.Equal(t, "2026-10-02T00:00:00Z", info.UpdatedAt.Format(time.RFC3339))
	require.Equal(t, "me", info.CreatedBy)
	require.Equal(t, "you", info.UpdatedBy)
}

func TestDriveCreateSendsOnlySetFields(t *testing.T) {
	s := newDriveServer(t)
	s.respond("POST", driveRoute, 201, driveRecord)
	_, err := s.drives(t).Create(t.Context(), DriveCreateOptions{})
	require.NoError(t, err)
	request := s.lastRequest(t)
	require.Equal(t, "", request.query)
	require.Equal(t, `{}`, request.body)
}

func TestDriveAuthentication(t *testing.T) {
	s := newDriveServer(t)
	s.respond("GET", driveRoute+"/data", 200, driveRecord)
	s.respond("GET", driveRoute+"/data", 200, driveRecord)
	drives := s.drives(t)
	for range 2 {
		_, err := drives.GetInfo(t.Context(), DriveGetInfoOptions{Name: "data"})
		require.NoError(t, err)
	}
	// One token, minted with the API key for the sandboxes scope, serves
	// both requests in place of the API key.
	require.Len(t, s.mints, 1)
	require.Equal(t, "Bearer key", s.mints[0].authorization)
	require.Equal(t, `{"scopes":["sandboxes"]}`, s.mints[0].body)
	for _, request := range s.requests {
		require.Equal(t, "Bearer token-1", request.authorization)
	}
}

func TestDriveRevokedTokenResent(t *testing.T) {
	s := newDriveServer(t)
	s.respondWithHeader("PATCH", driveRoute+"/data", 401,
		http.Header{"X-Blaxel-Error-Code": {"TOKEN_REVOKED"}}, `{"code":"TOKEN_REVOKED","message":"revoked"}`)
	s.respond("PATCH", driveRoute+"/data", 200, driveRecord)
	_, err := s.drives(t).Update(t.Context(), DriveUpdateOptions{Name: "data", Labels: map[string]string{"k": "v"}})
	require.NoError(t, err)
	require.Len(t, s.mints, 2)
	require.Len(t, s.requests, 2)
	require.Equal(t, "Bearer token-1", s.requests[0].authorization)
	require.Equal(t, "Bearer token-2", s.requests[1].authorization)
	// The body is sent again with the new token.
	require.Equal(t, `{"labels":{"k":"v"}}`, s.requests[1].body)
}

func TestDriveGetInfo(t *testing.T) {
	s := newDriveServer(t)
	s.respond("GET", driveRoute+"/data", 200, driveRecord)
	info, err := s.drives(t).GetInfo(t.Context(), DriveGetInfoOptions{TeamID: "team-1", Name: "data"})
	require.NoError(t, err)
	require.Equal(t, "data", info.Name)
	require.Equal(t, "team_id=team-1", s.lastRequest(t).query)

	_, err = s.drives(t).GetInfo(t.Context(), DriveGetInfoOptions{})
	require.Error(t, err)
}

func TestDriveGetInfoNotFound(t *testing.T) {
	s := newDriveServer(t)
	_, err := s.drives(t).GetInfo(t.Context(), DriveGetInfoOptions{Name: "missing"})
	responseErr := require.ErrorAs[*managementapi.ResponseError](t, err)
	require.Equal(t, 404, responseErr.StatusCode)
	require.True(t, strings.Contains(responseErr.Body, "NOT_FOUND"), "body %s", responseErr.Body)
}

func TestDriveList(t *testing.T) {
	s := newDriveServer(t)
	s.respond("GET", driveRoute, 200,
		`{"items":[{"name":"a","region":"r","status":"DEPLOYED"}],"pagination":{"has_more":true,"cursor":"c1"}}`)
	s.respond("GET", driveRoute, 200,
		`{"items":[{"name":"b","region":"r","status":"FAILED"}],"pagination":{"has_more":false}}`)
	var names []string
	for info, err := range s.drives(t).List(t.Context(), DriveListOptions{
		TeamID:   "team-1",
		Query:    "x",
		Statuses: []DriveStatus{DriveStatusDeployed, DriveStatusFailed},
		PageSize: 1,
	}) {
		require.NoError(t, err)
		names = append(names, info.Name)
	}
	require.Equal(t, "a,b", strings.Join(names, ","))
	require.Len(t, s.requests, 2)
	require.Equal(t, "limit=1&q=x&status=DEPLOYED&status=FAILED&team_id=team-1", s.requests[0].query)
	require.Equal(t, "cursor=c1&limit=1&q=x&status=DEPLOYED&status=FAILED&team_id=team-1", s.requests[1].query)
}

func TestDriveListRepeatedCursor(t *testing.T) {
	s := newDriveServer(t)
	page := `{"items":[],"pagination":{"has_more":true,"cursor":"c1"}}`
	s.respond("GET", driveRoute, 200, page)
	s.respond("GET", driveRoute, 200, page)
	var lastErr error
	for _, err := range s.drives(t).List(t.Context(), DriveListOptions{}) {
		lastErr = err
	}
	require.Error(t, lastErr)
}

func TestDriveUpdate(t *testing.T) {
	s := newDriveServer(t)
	s.respond("PATCH", driveRoute+"/data", 200, driveRecord)
	info, err := s.drives(t).Update(t.Context(), DriveUpdateOptions{TeamID: "team-1", Name: "data", Labels: map[string]string{}})
	require.NoError(t, err)
	request := s.lastRequest(t)
	require.Equal(t, "team_id=team-1", request.query)
	require.Equal(t, `{"labels":{}}`, request.body)
	require.Equal(t, "data", info.Name)

	_, err = s.drives(t).Update(t.Context(), DriveUpdateOptions{})
	require.Error(t, err)
}

func TestDriveDelete(t *testing.T) {
	s := newDriveServer(t)
	s.respond("DELETE", driveRoute+"/data", 202, `{"name":"data","region":"us-was-1","status":"DELETING"}`)
	info, err := s.drives(t).Delete(t.Context(), DriveDeleteOptions{TeamID: "team-1", Name: "data"})
	require.NoError(t, err)
	require.Equal(t, "team_id=team-1", s.lastRequest(t).query)
	require.Equal(t, DriveStatusDeleting, info.Status)

	_, err = s.drives(t).Delete(t.Context(), DriveDeleteOptions{})
	require.Error(t, err)
}

// A field the server adds to the drive record but DriveInfo does not carry is
// a silent omission, so it fails here instead.
func TestDriveInfoParity(t *testing.T) {
	fieldNames := func(v any) []string {
		var names []string
		for field := range reflect.TypeOf(v).Fields() {
			names = append(names, strings.ToLower(field.Name))
		}
		slices.Sort(names)
		return names
	}
	require.Equal(t,
		strings.Join(fieldNames(managementapi.SandboxDrive{}), ","),
		strings.Join(fieldNames(DriveInfo{}), ","))
}
