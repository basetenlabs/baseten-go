package client_test

import (
	"net/http"
	"testing"

	"github.com/basetenlabs/baseten-go/client"
	"github.com/basetenlabs/baseten-go/internal/require"
)

func TestClientNameRoundTrip(t *testing.T) {
	orig := client.ClientName()
	t.Cleanup(func() { client.SetClientName(orig) })
	require.Equal(t, "baseten-go", client.ClientName())
	client.SetClientName("baseten-cli")
	require.Equal(t, "baseten-cli", client.ClientName())
}

func TestApplyUserAgentHeaderFormat(t *testing.T) {
	orig := client.ClientName()
	t.Cleanup(func() { client.SetClientName(orig) })
	h := http.Header{}
	client.ApplyUserAgentHeader(h)
	require.Regexp(t, `^baseten-go/\S+ \(Go/\S+; [^)]+\)$`, h.Get("User-Agent"))
}

func TestApplyUserAgentHeaderNonClobber(t *testing.T) {
	h := http.Header{"User-Agent": {"custom/1.0"}}
	client.ApplyUserAgentHeader(h)
	require.Equal(t, "custom/1.0", h.Get("User-Agent"))
}

func TestApplyUserAgentHeaderDisabled(t *testing.T) {
	orig := client.ClientName()
	t.Cleanup(func() { client.SetClientName(orig) })
	client.SetClientName("")
	h := http.Header{}
	client.ApplyUserAgentHeader(h)
	require.Equal(t, "", h.Get("User-Agent"))
}
