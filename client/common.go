package client

import (
	"net/http"
	"runtime"
	"runtime/debug"
	"strings"
	"sync/atomic"
)

const sdkModulePath = "github.com/basetenlabs/baseten-go"

var clientName atomic.Pointer[string]

func init() {
	name := "baseten-go"
	clientName.Store(&name)
}

// SetClientName overrides the product token used in the Baseten User-Agent
// header (e.g. "baseten-cli"). Defaults to "baseten-go". Set to "" to disable
// User-Agent injection.
//
// Must be called before constructing any client. The User-Agent value is
// captured into the client's headers at construction time; later changes do
// not propagate to existing clients.
func SetClientName(name string) {
	clientName.Store(&name)
}

// ClientName returns the current client name used in the Baseten User-Agent
// header.
func ClientName() string {
	return *clientName.Load()
}

// ApplyUserAgentHeader builds and sets a Baseten User-Agent on h if one is not
// already present. The value looks like "baseten-go/0.1.0 (Go/1.25.0; linux)".
// This is a no-op if the client name has been disabled via SetClientName("").
func ApplyUserAgentHeader(h http.Header) {
	name := ClientName()
	if name == "" {
		return
	}
	if h.Get("User-Agent") != "" {
		return
	}
	h.Set("User-Agent", name+"/"+sdkVersion()+" (Go/"+goVersion()+"; "+runtime.GOOS+")")
}

// Not cached: ApplyUserAgentHeader is usually invoked once per client
// construction, so debug.ReadBuildInfo runs a handful of times per process at
// most.
func sdkVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		if info.Main.Path == sdkModulePath {
			return strings.TrimPrefix(info.Main.Version, "v")
		}
		for _, dep := range info.Deps {
			if dep.Path == sdkModulePath {
				return strings.TrimPrefix(dep.Version, "v")
			}
		}
	}
	return "dev"
}

func goVersion() string {
	return strings.TrimPrefix(runtime.Version(), "go")
}
