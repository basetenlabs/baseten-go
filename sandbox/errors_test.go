package sandbox

import (
	"errors"
	"fmt"
	"testing"

	"github.com/basetenlabs/baseten-go/client/managementapi"
	"github.com/basetenlabs/baseten-go/client/sandboxapi"
	"github.com/basetenlabs/baseten-go/internal/require"
)

func TestControlPlaneError(t *testing.T) {
	t.Run("CodeInCode", func(t *testing.T) {
		err := controlPlaneError(&managementapi.ResponseError{
			StatusCode: 404,
			Body:       `{"code":"NOT_FOUND","message":"image not found"}`,
		})
		apiErr := require.ErrorAs[*APIError](t, err)
		require.Equal(t, 404, apiErr.Status)
		require.Equal(t, "NOT_FOUND", apiErr.Code)
		require.Equal(t, "sandbox API error (HTTP 404) NOT_FOUND: image not found", err.Error())
	})

	t.Run("CodeInErrorWithMessage", func(t *testing.T) {
		err := controlPlaneError(&managementapi.ResponseError{
			StatusCode: 409,
			Body:       `{"error":"RESOURCE_IN_USE","message":"image in use","details":{"by":"x"}}`,
		})
		apiErr := require.ErrorAs[*APIError](t, err)
		require.Equal(t, "RESOURCE_IN_USE", apiErr.Code)
		require.Equal(t, "x", apiErr.Details.(map[string]any)["by"])
		require.Equal(t, "sandbox API error (HTTP 409) RESOURCE_IN_USE: image in use", err.Error())
	})

	t.Run("NonJSONBody", func(t *testing.T) {
		err := controlPlaneError(&managementapi.ResponseError{StatusCode: 500, Body: "boom"})
		apiErr := require.ErrorAs[*APIError](t, err)
		require.Equal(t, "boom", apiErr.Body.(string))
		require.Equal(t, "", apiErr.Code)
		require.Equal(t, "sandbox API error (HTTP 500)", err.Error())
	})

	t.Run("GatewayStatusStaysPlain", func(t *testing.T) {
		err := controlPlaneError(&managementapi.ResponseError{StatusCode: 503, Body: "{}"})
		var gatewayErr *GatewayError
		require.False(t, errors.As(err, &gatewayErr), "control plane 503 must not be a GatewayError")
	})

	t.Run("OtherErrorsUnchanged", func(t *testing.T) {
		original := errors.New("dial failed")
		require.Equal(t, original, controlPlaneError(original))
	})
}

func TestSandboxError(t *testing.T) {
	t.Run("DescriptionInError", func(t *testing.T) {
		err := sandboxError(&sandboxapi.ResponseError{StatusCode: 404, Body: `{"error":"process not found"}`})
		apiErr := require.ErrorAs[*APIError](t, err)
		require.Equal(t, "", apiErr.Code)
		require.Equal(t, "sandbox API error (HTTP 404): process not found", err.Error())
	})

	t.Run("TypedErrorResponse", func(t *testing.T) {
		err := sandboxError(fmt.Errorf("wrapped: %w", &sandboxapi.ResponseErrorResponse{
			StatusCode:    400,
			ErrorResponse: sandboxapi.ErrorResponse{Error: "bad path"},
		}))
		apiErr := require.ErrorAs[*APIError](t, err)
		require.Equal(t, 400, apiErr.Status)
		require.Equal(t, "sandbox API error (HTTP 400): bad path", err.Error())
	})

	t.Run("GatewayStatuses", func(t *testing.T) {
		for _, status := range []int{502, 503, 504} {
			err := sandboxError(&sandboxapi.ResponseError{StatusCode: status, Body: "<html>"})
			gatewayErr := require.ErrorAs[*GatewayError](t, err)
			require.Equal(t, status, gatewayErr.Status)
			// A gateway error is an APIError too.
			apiErr := require.ErrorAs[*APIError](t, err)
			require.Equal(t, status, apiErr.Status)
		}
	})

	t.Run("NonGatewayIsNotGatewayError", func(t *testing.T) {
		err := sandboxError(&sandboxapi.ResponseError{StatusCode: 500, Body: "{}"})
		var gatewayErr *GatewayError
		require.False(t, errors.As(err, &gatewayErr), "500 must not be a GatewayError")
	})
}
