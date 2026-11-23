package sandbox

import (
	"net/http"
	"strings"
	"testing"

	"github.com/basetenlabs/baseten-go/internal/require"
)

func TestDriveMount(t *testing.T) {
	t.Run("SendsOnlySetFields", func(t *testing.T) {
		sb, last := execSandbox(t, func(w http.ResponseWriter, r *http.Request) {
			respondJSON(w, 200, `{"success":true,"driveName":"data","drivePath":"/sub","mountPath":"/mnt/data","readOnly":true}`)
		})
		mount, err := sb.Drives().Mount(t.Context(), DriveMountOptions{
			Name:      "data",
			MountPath: "/mnt/data",
			DrivePath: "/sub",
			ReadOnly:  true,
		})
		require.NoError(t, err)
		request := last()
		require.Equal(t, "/drives/mount", request.path)
		require.Equal(t, "Bearer token", request.authorization)
		require.Equal(t, `{"driveName":"data","drivePath":"/sub","mountPath":"/mnt/data","readOnly":true}`, request.body)
		require.Equal(t, "data", mount.DriveName)
		require.Equal(t, "/sub", mount.DrivePath)
		require.Equal(t, "/mnt/data", mount.MountPath)
		require.True(t, mount.ReadOnly, "read-only")
	})

	t.Run("DefaultsOmitted", func(t *testing.T) {
		sb, last := execSandbox(t, func(w http.ResponseWriter, r *http.Request) {
			respondJSON(w, 200, `{"success":true,"driveName":"data","drivePath":"/","mountPath":"/mnt/data"}`)
		})
		_, err := sb.Drives().Mount(t.Context(), DriveMountOptions{Name: "data", MountPath: "/mnt/data"})
		require.NoError(t, err)
		require.Equal(t, `{"driveName":"data","mountPath":"/mnt/data"}`, last().body)
	})

	t.Run("UnsuccessfulIsError", func(t *testing.T) {
		sb, _ := execSandbox(t, func(w http.ResponseWriter, r *http.Request) {
			respondJSON(w, 200, `{"success":false,"message":"blfs exited"}`)
		})
		_, err := sb.Drives().Mount(t.Context(), DriveMountOptions{Name: "data", MountPath: "/mnt/data"})
		require.True(t, err != nil && strings.Contains(err.Error(), "blfs exited"), "got %v", err)
	})

	t.Run("APIError", func(t *testing.T) {
		sb, _ := execSandbox(t, func(w http.ResponseWriter, r *http.Request) {
			respondJSON(w, 403, `{"error":"no permission matches the sandbox"}`)
		})
		_, err := sb.Drives().Mount(t.Context(), DriveMountOptions{Name: "data", MountPath: "/mnt/data"})
		require.Equal(t, 403, require.ErrorAs[*APIError](t, err).Status)
	})

	t.Run("RequiresNameAndMountPath", func(t *testing.T) {
		sb, _ := execSandbox(t, func(w http.ResponseWriter, r *http.Request) {})
		_, err := sb.Drives().Mount(t.Context(), DriveMountOptions{MountPath: "/mnt/data"})
		require.Error(t, err)
		_, err = sb.Drives().Mount(t.Context(), DriveMountOptions{Name: "data"})
		require.Error(t, err)
	})
}

func TestDriveUnmount(t *testing.T) {
	t.Run("EscapesMountPath", func(t *testing.T) {
		sb, last := execSandbox(t, func(w http.ResponseWriter, r *http.Request) {
			respondJSON(w, 200, `{"success":true,"mountPath":"/mnt/data"}`)
		})
		require.NoError(t, sb.Drives().Unmount(t.Context(), DriveUnmountOptions{MountPath: "/mnt/data"}))
		// The mount path is one escaped segment, decoded here by the server.
		require.Equal(t, "/drives/mount//mnt/data", last().path)
	})

	t.Run("UnsuccessfulIsError", func(t *testing.T) {
		sb, _ := execSandbox(t, func(w http.ResponseWriter, r *http.Request) {
			respondJSON(w, 200, `{"success":false,"message":"not mounted"}`)
		})
		err := sb.Drives().Unmount(t.Context(), DriveUnmountOptions{MountPath: "/mnt/data"})
		require.True(t, err != nil && strings.Contains(err.Error(), "not mounted"), "got %v", err)
	})

	t.Run("RequiresMountPath", func(t *testing.T) {
		sb, _ := execSandbox(t, func(w http.ResponseWriter, r *http.Request) {})
		require.Error(t, sb.Drives().Unmount(t.Context(), DriveUnmountOptions{}))
	})
}

func TestDriveListMounts(t *testing.T) {
	t.Run("Mounts", func(t *testing.T) {
		sb, last := execSandbox(t, func(w http.ResponseWriter, r *http.Request) {
			respondJSON(w, 200, `{"mounts":[{"driveName":"a","drivePath":"/","mountPath":"/mnt/a"},`+
				`{"driveName":"b","drivePath":"/x","mountPath":"/mnt/b","readOnly":true}]}`)
		})
		mounts, err := sb.Drives().ListMounts(t.Context())
		require.NoError(t, err)
		require.Equal(t, "/drives/mount", last().path)
		require.Len(t, mounts, 2)
		require.Equal(t, DriveMount{DriveName: "a", DrivePath: "/", MountPath: "/mnt/a"}, mounts[0])
		require.Equal(t, DriveMount{DriveName: "b", DrivePath: "/x", MountPath: "/mnt/b", ReadOnly: true}, mounts[1])
	})

	t.Run("None", func(t *testing.T) {
		sb, _ := execSandbox(t, func(w http.ResponseWriter, r *http.Request) {
			respondJSON(w, 200, `{}`)
		})
		mounts, err := sb.Drives().ListMounts(t.Context())
		require.NoError(t, err)
		require.Len(t, mounts, 0)
	})
}
