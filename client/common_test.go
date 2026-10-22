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

func TestApplyUserAgentHeaderClientVersion(t *testing.T) {
	origName, origVersion := client.ClientName(), client.ClientVersion()
	t.Cleanup(func() {
		client.SetClientName(origName)
		client.SetClientVersion(origVersion)
	})
	require.Equal(t, "", client.ClientVersion())
	client.SetClientName("baseten-cli")
	client.SetClientVersion("1.5.0")
	require.Equal(t, "1.5.0", client.ClientVersion())
	h := http.Header{}
	client.ApplyUserAgentHeader(h)
	require.Regexp(t, `^baseten-cli/1\.5\.0 \(Go/\S+; [^)]+\)$`, h.Get("User-Agent"))
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
