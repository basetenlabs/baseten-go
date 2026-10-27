package sandbox

import (
	"strings"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-go/internal/require"
)

const volumeRecord = `{"name":"data","size":1024,"region":"us-pdx-1","status":"DEPLOYED","attached_to":"sb",
	"labels":{"k":"v"},"created_at":"2026-10-01T00:00:00Z","created_by":"me"}`

func TestVolumeCreate(t *testing.T) {
	cp := newControlPlane(t)
	cp.respond("POST", "/v1/sandboxes/volumes", 201, volumeRecord)
	info, err := cp.client(t, ClientOptions{TeamID: "team-1"}).Volumes().Create(t.Context(), VolumeCreateOptions{
		Name:          "data",
		SizeMegabytes: 1024,
		Labels:        map[string]string{},
	})
	require.NoError(t, err)
	request := cp.lastRequest(t)
	require.Equal(t, "team_id=team-1", request.query)
	require.Equal(t, `{"labels":{},"name":"data","size":1024}`, request.body)
	require.Equal(t, "data", info.Name)
	require.Equal(t, 1024, info.SizeMegabytes)
	require.Equal(t, "us-pdx-1", info.Region)
	require.Equal(t, VolumeStatusDeployed, info.Status)
	require.Equal(t, "sb", info.AttachedTo)
	require.MapEqual(t, info.Labels, "k", "v")
	require.Equal(t, "2026-10-01T00:00:00Z", info.CreatedAt.Format(time.RFC3339))
	require.Equal(t, "me", info.CreatedBy)
}

func TestVolumeSizeForms(t *testing.T) {
	t.Run("CreateString", func(t *testing.T) {
		cp := newControlPlane(t)
		cp.respond("POST", "/v1/sandboxes/volumes", 201, `{"name":"data","size":"10Gi","region":"r","status":"DEPLOYING"}`)
		info, err := cp.client(t, ClientOptions{}).Volumes().Create(t.Context(), VolumeCreateOptions{Size: "10Gi"})
		require.NoError(t, err)
		require.Equal(t, `{"size":"10Gi"}`, cp.lastRequest(t).body)
		require.Equal(t, "10Gi", info.Size)
		require.Equal(t, 0, info.SizeMegabytes)
	})

	t.Run("UpdateString", func(t *testing.T) {
		cp := newControlPlane(t)
		cp.respond("PATCH", "/v1/sandboxes/volumes/data", 200, volumeRecord)
		_, err := cp.client(t, ClientOptions{}).Volumes().Update(t.Context(), VolumeUpdateOptions{Name: "data", Size: "20Gi"})
		require.NoError(t, err)
		require.Equal(t, `{"size":"20Gi"}`, cp.lastRequest(t).body)
	})

	t.Run("CreateRequiresOne", func(t *testing.T) {
		volumes := newControlPlane(t).client(t, ClientOptions{}).Volumes()
		_, err := volumes.Create(t.Context(), VolumeCreateOptions{})
		require.Error(t, err)
		_, err = volumes.Create(t.Context(), VolumeCreateOptions{Size: "1Gi", SizeMegabytes: 1024})
		require.Error(t, err)
	})

	t.Run("UpdateCannotCombine", func(t *testing.T) {
		volumes := newControlPlane(t).client(t, ClientOptions{}).Volumes()
		_, err := volumes.Update(t.Context(), VolumeUpdateOptions{Name: "data", Size: "1Gi", SizeMegabytes: 1024})
		require.Error(t, err)
	})
}

func TestVolumeGetInfo(t *testing.T) {
	cp := newControlPlane(t)
	cp.respond("GET", "/v1/sandboxes/volumes/data", 200, volumeRecord)
	info, err := cp.client(t, ClientOptions{}).Volumes().GetInfo(t.Context(), VolumeGetInfoOptions{Name: "data"})
	require.NoError(t, err)
	require.Equal(t, "data", info.Name)
	require.Equal(t, "", cp.lastRequest(t).query)
}

func TestVolumeList(t *testing.T) {
	cp := newControlPlane(t)
	cp.respond("GET", "/v1/sandboxes/volumes", 200,
		`{"items":[{"name":"a","size":1,"region":"r","status":"DEPLOYED"}],"pagination":{"has_more":true,"cursor":"c1"}}`)
	cp.respond("GET", "/v1/sandboxes/volumes", 200,
		`{"items":[{"name":"b","size":1,"region":"r","status":"FAILED"}],"pagination":{"has_more":false}}`)
	var names []string
	for info, err := range cp.client(t, ClientOptions{}).Volumes().List(t.Context(), VolumeListOptions{
		Query:    "x",
		Statuses: []VolumeStatus{VolumeStatusDeployed, VolumeStatusFailed},
		PageSize: 1,
	}) {
		require.NoError(t, err)
		// List omits labels, distinct from a volume with none.
		require.True(t, info.Labels == nil, "labels nil when omitted")
		names = append(names, info.Name)
	}
	require.Equal(t, "a,b", strings.Join(names, ","))
	require.Len(t, cp.requests, 2)
	require.Equal(t, "limit=1&q=x&status=DEPLOYED&status=FAILED", cp.requests[0].query)
	require.Equal(t, "cursor=c1&limit=1&q=x&status=DEPLOYED&status=FAILED", cp.requests[1].query)
}

func TestVolumeUpdate(t *testing.T) {
	t.Run("SendsOnlySetFields", func(t *testing.T) {
		cp := newControlPlane(t)
		cp.respond("PATCH", "/v1/sandboxes/volumes/data", 200, volumeRecord)
		_, err := cp.client(t, ClientOptions{}).Volumes().Update(t.Context(), VolumeUpdateOptions{
			Name:          "data",
			SizeMegabytes: 2048,
			Labels:        map[string]string{},
		})
		require.NoError(t, err)
		require.Equal(t, `{"labels":{},"size":2048}`, cp.lastRequest(t).body)
	})

	t.Run("ZeroSizeUnchanged", func(t *testing.T) {
		cp := newControlPlane(t)
		cp.respond("PATCH", "/v1/sandboxes/volumes/data", 200, volumeRecord)
		_, err := cp.client(t, ClientOptions{}).Volumes().Update(t.Context(), VolumeUpdateOptions{
			Name:   "data",
			Labels: map[string]string{"k": "v"},
		})
		require.NoError(t, err)
		require.Equal(t, `{"labels":{"k":"v"}}`, cp.lastRequest(t).body)
	})
}

func TestVolumeDelete(t *testing.T) {
	t.Run("ReturnsDeleting", func(t *testing.T) {
		cp := newControlPlane(t)
		cp.respond("DELETE", "/v1/sandboxes/volumes/data", 202, `{"name":"data","size":1024,"region":"r","status":"DELETING"}`)
		info, err := cp.client(t, ClientOptions{}).Volumes().Delete(t.Context(), VolumeDeleteOptions{Name: "data"})
		require.NoError(t, err)
		require.Equal(t, VolumeStatusDeleting, info.Status)
	})

	t.Run("MountedConflict", func(t *testing.T) {
		cp := newControlPlane(t)
		cp.respond("DELETE", "/v1/sandboxes/volumes/data", 409, `{"code":"RESOURCE_IN_USE","message":"mounted"}`)
		_, err := cp.client(t, ClientOptions{}).Volumes().Delete(t.Context(), VolumeDeleteOptions{Name: "data"})
		apiErr := require.ErrorAs[*APIError](t, err)
		require.Equal(t, 409, apiErr.Status)
	})
}

func TestVolumeNameRequired(t *testing.T) {
	volumes := newControlPlane(t).client(t, ClientOptions{}).Volumes()
	_, err := volumes.GetInfo(t.Context(), VolumeGetInfoOptions{})
	require.Error(t, err)
	_, err = volumes.Update(t.Context(), VolumeUpdateOptions{})
	require.Error(t, err)
	_, err = volumes.Delete(t.Context(), VolumeDeleteOptions{})
	require.Error(t, err)
}

func TestCreateWithVolume(t *testing.T) {
	cp := newControlPlane(t)
	cp.respond("POST", "/v1/sandboxes/instances", 201,
		`{"name":"sb","url":"https://sb.example","status":"DEPLOYING","volumes":[{"name":"data","mount_path":"/data","read_only":true}]}`)
	result, err := cp.client(t, ClientOptions{}).Create(t.Context(), CreateOptions{
		Volumes: []VolumeMount{{Name: "data", MountPath: "/data", ReadOnly: true}},
	})
	require.NoError(t, err)
	require.Equal(t, `{"volumes":[{"mount_path":"/data","name":"data","read_only":true}]}`, cp.lastRequest(t).body)
	require.Len(t, result.Info.Volumes, 1)
	require.Equal(t, VolumeMount{Name: "data", MountPath: "/data", ReadOnly: true}, result.Info.Volumes[0])
}
