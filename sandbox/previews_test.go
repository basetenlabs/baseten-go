package sandbox

import (
	"strings"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-go/internal/require"
)

const previewRecord = `{"name":"web","port":3000,"url":"https://p.example","status":"DEPLOYED","public":false,"region":"us-was-1",` +
	`"expires_at":"2026-10-20T12:00:00Z","response_headers":{"A":"1"},"request_headers":{"Authorization":"****"},` +
	`"created_at":"2026-10-13T12:00:00Z","created_by":"me"}`

func previewService(t *testing.T, cp *controlPlane) *PreviewService {
	t.Helper()
	return cp.client(t, ClientOptions{TeamID: "team-1"}).newSandbox("sb", "https://sb.example").Previews()
}

func TestPreviewCreate(t *testing.T) {
	t.Run("SendsOnlySetFields", func(t *testing.T) {
		cp := newControlPlane(t)
		cp.respond("POST", "/v1/sandboxes/instances/sb/previews", 201, previewRecord)
		info, err := previewService(t, cp).Create(t.Context(), PreviewCreateOptions{
			Port:            3000,
			Name:            "web",
			ResponseHeaders: map[string]string{},
			TTL:             90 * time.Minute,
		})
		require.NoError(t, err)
		request := cp.lastRequest(t)
		require.Equal(t, "team_id=team-1", request.query)
		require.Equal(t, `{"name":"web","port":3000,"response_headers":{},"ttl":"5400000ms"}`, request.body)
		require.Equal(t, "web", info.Name)
		require.Equal(t, 3000, info.Port)
		require.Equal(t, "https://p.example", info.URL)
		require.Equal(t, PreviewStatusDeployed, info.Status)
		require.False(t, info.Public, "private preview")
		require.Equal(t, "us-was-1", info.Region)
		require.True(t, info.ExpiresAt.Equal(time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)), "ExpiresAt %v", info.ExpiresAt)
		require.MapEqual(t, info.ResponseHeaders, "A", "1")
		require.MapEqual(t, info.RequestHeaders, "Authorization", "****")
		require.True(t, info.CreatedAt.Equal(time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)), "CreatedAt %v", info.CreatedAt)
		require.Equal(t, "me", info.CreatedBy)
		require.True(t, info.UpdatedAt.IsZero(), "UpdatedAt %v", info.UpdatedAt)
	})

	t.Run("AllFields", func(t *testing.T) {
		cp := newControlPlane(t)
		cp.respond("POST", "/v1/sandboxes/instances/sb/previews", 201, previewRecord)
		_, err := previewService(t, cp).Create(t.Context(), PreviewCreateOptions{
			Port:              3000,
			CreateIfNotExists: true,
			Public:            true,
			RequestHeaders:    map[string]string{"Authorization": "Bearer x"},
			ExpiresAt:         time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC),
		})
		require.NoError(t, err)
		require.Equal(t,
			`{"create_if_not_exists":true,"expires_at":"2026-10-20T12:00:00Z","port":3000,"public":true,"request_headers":{"Authorization":"Bearer x"}}`,
			cp.lastRequest(t).body)
	})

	t.Run("PortRequired", func(t *testing.T) {
		cp := newControlPlane(t)
		_, err := previewService(t, cp).Create(t.Context(), PreviewCreateOptions{})
		require.Error(t, err)
		require.Contains(t, err.Error(), "Port is required")
		require.Len(t, cp.requests, 0)
	})

	t.Run("APIError", func(t *testing.T) {
		cp := newControlPlane(t)
		cp.respond("POST", "/v1/sandboxes/instances/sb/previews", 409, `{"code":"CONFLICT","message":"exists"}`)
		_, err := previewService(t, cp).Create(t.Context(), PreviewCreateOptions{Port: 3000})
		apiErr := require.ErrorAs[*APIError](t, err)
		require.Equal(t, "CONFLICT", apiErr.Code)
	})
}

func TestPreviewGetInfo(t *testing.T) {
	cp := newControlPlane(t)
	cp.respond("GET", "/v1/sandboxes/instances/sb/previews/web", 200, previewRecord)
	info, err := previewService(t, cp).GetInfo(t.Context(), PreviewGetInfoOptions{Name: "web", ShowSecrets: true})
	require.NoError(t, err)
	require.Equal(t, "web", info.Name)
	require.Equal(t, "show_secrets=true&team_id=team-1", cp.lastRequest(t).query)
}

func TestPreviewList(t *testing.T) {
	cp := newControlPlane(t)
	cp.respond("GET", "/v1/sandboxes/instances/sb/previews", 200,
		`{"items":[{"name":"a","port":1,"public":true}],"pagination":{"has_more":true,"cursor":"c1"}}`)
	cp.respond("GET", "/v1/sandboxes/instances/sb/previews", 200,
		`{"items":[{"name":"b","port":2,"public":false}],"pagination":{"has_more":false}}`)
	var names []string
	for info, err := range previewService(t, cp).List(t.Context(), PreviewListOptions{PageSize: 1}) {
		require.NoError(t, err)
		names = append(names, info.Name)
	}
	require.Equal(t, "a,b", strings.Join(names, ","))
	require.Len(t, cp.requests, 2)
	require.Equal(t, "limit=1&team_id=team-1", cp.requests[0].query)
	require.Equal(t, "cursor=c1&limit=1&team_id=team-1", cp.requests[1].query)
}

func TestPreviewUpdate(t *testing.T) {
	t.Run("SendsOnlySetFields", func(t *testing.T) {
		cp := newControlPlane(t)
		cp.respond("PATCH", "/v1/sandboxes/instances/sb/previews/web", 200, previewRecord)
		public := false
		_, err := previewService(t, cp).Update(t.Context(), PreviewUpdateOptions{
			Name:           "web",
			Public:         &public,
			RequestHeaders: map[string]string{},
		})
		require.NoError(t, err)
		// False is sent, since it differs from unchanged, and an empty map
		// clears the set.
		require.Equal(t, `{"public":false,"request_headers":{}}`, cp.lastRequest(t).body)
	})

	t.Run("EmptyOptionsSendEmptyBody", func(t *testing.T) {
		cp := newControlPlane(t)
		cp.respond("PATCH", "/v1/sandboxes/instances/sb/previews/web", 200, previewRecord)
		_, err := previewService(t, cp).Update(t.Context(), PreviewUpdateOptions{Name: "web"})
		require.NoError(t, err)
		require.Equal(t, `{}`, cp.lastRequest(t).body)
	})
}

func TestPreviewDelete(t *testing.T) {
	cp := newControlPlane(t)
	cp.respond("DELETE", "/v1/sandboxes/instances/sb/previews/web", 200, `{"name":"web","port":3000,"public":false,"status":"DELETING"}`)
	info, err := previewService(t, cp).Delete(t.Context(), PreviewDeleteOptions{Name: "web"})
	require.NoError(t, err)
	require.Equal(t, PreviewStatusDeleting, info.Status)
}

func TestPreviewTokens(t *testing.T) {
	const token = `{"name":"t1","token":"secret","expires_at":"2026-10-14T12:00:00Z","expired":false}`
	expiresAt := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

	t.Run("Create", func(t *testing.T) {
		cp := newControlPlane(t)
		cp.respond("POST", "/v1/sandboxes/instances/sb/previews/web/tokens", 201, token)
		info, err := previewService(t, cp).CreateToken(t.Context(), PreviewCreateTokenOptions{
			PreviewName:       "web",
			ExpiresAt:         expiresAt,
			CreateIfNotExists: true,
		})
		require.NoError(t, err)
		require.Equal(t, `{"create_if_not_exists":true,"expires_at":"2026-10-14T12:00:00Z"}`, cp.lastRequest(t).body)
		require.Equal(t, "t1", info.Name)
		require.Equal(t, "secret", info.Token)
		require.True(t, info.ExpiresAt.Equal(expiresAt), "ExpiresAt %v", info.ExpiresAt)
		require.False(t, info.Expired, "token not expired")
	})

	t.Run("CreateRequiresExpiresAt", func(t *testing.T) {
		cp := newControlPlane(t)
		_, err := previewService(t, cp).CreateToken(t.Context(), PreviewCreateTokenOptions{PreviewName: "web"})
		require.Error(t, err)
		require.Contains(t, err.Error(), "ExpiresAt is required")
		require.Len(t, cp.requests, 0)
	})

	t.Run("List", func(t *testing.T) {
		cp := newControlPlane(t)
		cp.respond("GET", "/v1/sandboxes/instances/sb/previews/web/tokens", 200, `{"items":[`+token+`],"pagination":{"has_more":false}}`)
		var names []string
		for info, err := range previewService(t, cp).ListTokens(t.Context(), PreviewListTokensOptions{PreviewName: "web"}) {
			require.NoError(t, err)
			names = append(names, info.Name)
		}
		require.Equal(t, "t1", strings.Join(names, ","))
	})

	t.Run("Delete", func(t *testing.T) {
		cp := newControlPlane(t)
		cp.respond("DELETE", "/v1/sandboxes/instances/sb/previews/web/tokens/t1", 204, "")
		err := previewService(t, cp).DeleteToken(t.Context(), PreviewDeleteTokenOptions{PreviewName: "web", TokenName: "t1"})
		require.NoError(t, err)
		require.Equal(t, "DELETE", cp.lastRequest(t).method)
	})
}

func TestPreviewsWithoutSandboxName(t *testing.T) {
	cp := newControlPlane(t)
	sb, err := cp.client(t, ClientOptions{}).SandboxFromURL(SandboxFromURLOptions{URL: "https://sb.example"})
	require.NoError(t, err)
	previews := sb.Previews()
	ctx := t.Context()
	errs := map[string]error{}
	_, errs["Create"] = previews.Create(ctx, PreviewCreateOptions{Port: 3000})
	_, errs["GetInfo"] = previews.GetInfo(ctx, PreviewGetInfoOptions{Name: "web"})
	_, errs["Update"] = previews.Update(ctx, PreviewUpdateOptions{Name: "web"})
	_, errs["Delete"] = previews.Delete(ctx, PreviewDeleteOptions{Name: "web"})
	_, errs["CreateToken"] = previews.CreateToken(ctx, PreviewCreateTokenOptions{PreviewName: "web", ExpiresAt: time.Now()})
	errs["DeleteToken"] = previews.DeleteToken(ctx, PreviewDeleteTokenOptions{PreviewName: "web", TokenName: "t1"})
	for _, err := range previews.List(ctx, PreviewListOptions{}) {
		errs["List"] = err
	}
	for _, err := range previews.ListTokens(ctx, PreviewListTokensOptions{PreviewName: "web"}) {
		errs["ListTokens"] = err
	}
	require.Equal(t, 8, len(errs))
	for name, err := range errs {
		require.True(t, err != nil && strings.Contains(err.Error(), "SandboxFromURL"), "%s: got %v", name, err)
	}
	require.Len(t, cp.requests, 0)
}
