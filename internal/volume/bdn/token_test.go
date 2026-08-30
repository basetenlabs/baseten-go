package bdn

import (
	"encoding/base64"
	"testing"

	"github.com/basetenlabs/baseten-go/internal/require"
)

// makeToken builds a token whose payload is the given JSON. The signature is
// filler: nothing here verifies one, and nothing here could.
func makeToken(payload string) string {
	return "header." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".signature"
}

func TestDecodeGrants(t *testing.T) {
	token := makeToken(`{"org":"org1","grants":[
		{"org":"org1","namespaces":["Default"],"volumes":["GPT2"],"tags":["*"],"scopes":["pull","push"]},
		{"org":"org1","namespaces":["other"],"volumes":["*"],"tags":["prod"],"scopes":["tag"]}
	]}`)
	grants := DecodeGrants(token)

	tests := []struct {
		name                   string
		namespace, volume, tag string
		scopes                 []string
		want                   bool
	}{
		// Selectors are lowercased on the server before they are matched, so
		// a mixed-case grant that the server accepts has to match here too.
		{"case folded grant", "default", "gpt2", "head", []string{ScopePull}, true},
		{"case folded request", "DEFAULT", "GPT2", "head", []string{ScopePull}, true},
		{"wildcard tag", "default", "gpt2", "anything", []string{ScopePush}, true},
		{"wrong scope", "default", "gpt2", "head", []string{ScopeTag}, false},
		{"wrong volume", "default", "llama", "head", []string{ScopePull}, false},
		{"wrong namespace", "missing", "gpt2", "head", []string{ScopePull}, false},
		{"wildcard volume", "other", "anything", "prod", []string{ScopeTag}, true},
		{"tag outside grant", "other", "anything", "staging", []string{ScopeTag}, false},
		{"any of several scopes", "other", "anything", "prod", []string{ScopePush, ScopeTag}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, grants.Permits(tc.namespace, tc.volume, tc.tag, tc.scopes...))
		})
	}
}

func TestGrantsHeadAndResolveChecks(t *testing.T) {
	pullOnly := DecodeGrants(makeToken(
		`{"org":"o","grants":[{"org":"o","namespaces":["ns"],"volumes":["vol"],"tags":["*"],"scopes":["pull"]}]}`))
	require.True(t, pullOnly.PermitsResolve("ns", "vol"), "pull scope should permit resolving")
	require.False(t, pullOnly.PermitsHeadMove("ns", "vol"), "pull scope should not permit moving head")

	// A push grant restricted to a named tag cannot move head, which is the
	// reserved tag. Committing with update_head would be a certain denial, so
	// the push asks for it to be left alone instead.
	taggedPush := DecodeGrants(makeToken(
		`{"org":"o","grants":[{"org":"o","namespaces":["ns"],"volumes":["vol"],"tags":["prod"],"scopes":["push"]}]}`))
	require.False(t, taggedPush.PermitsHeadMove("ns", "vol"), "a prod-only grant should not move head")

	headPush := DecodeGrants(makeToken(
		`{"org":"o","grants":[{"org":"o","namespaces":["ns"],"volumes":["vol"],"tags":["head"],"scopes":["push"]}]}`))
	require.True(t, headPush.PermitsHeadMove("ns", "vol"), "a head grant should move head")
}

// TestUnreadableTokensArePermissive pins the fallback: a hint that cannot be
// read must not become a refusal. These checks exist to skip requests that
// would certainly be denied, and with nothing to read, nothing is certain —
// the server decides.
func TestUnreadableTokensArePermissive(t *testing.T) {
	tests := map[string]string{
		"empty":              "",
		"not a jwt":          "opaque-token",
		"two segments":       "header.payload",
		"payload not base64": "header.!!!.signature",
		"payload not json":   makeToken("not json"),
		"no grants":          makeToken(`{"org":"o"}`),
	}
	for name, token := range tests {
		t.Run(name, func(t *testing.T) {
			grants := DecodeGrants(token)
			require.True(t, grants.PermitsHeadMove("ns", "vol"), "an unreadable token should permit everything")
			require.True(t, grants.PermitsResolve("ns", "vol"), "an unreadable token should permit everything")
		})
	}
}

// TestOrgComesFromTheGrants pins where the org is read from. The server
// derives it from the grants and ignores the top-level claim, so a token whose
// claim disagrees with its grants must still be read the server's way —
// otherwise this client would skip a head move the server would have allowed,
// and the push would publish without moving head for no reason.
func TestOrgComesFromTheGrants(t *testing.T) {
	grants := DecodeGrants(makeToken(
		`{"org":"stale-claim","grants":[{"org":"real","namespaces":["ns"],"volumes":["vol"],` +
			`"tags":["*"],"scopes":["push"]}]}`))
	require.True(t, grants.PermitsHeadMove("ns", "vol"), "the grant's own org should be the token's org")
}

// TestGrantsDisagreeingOnOrgArePermissive covers a token the server would
// refuse to derive an org from at all. Both places that derivation is used
// treat the refusal as "attempt it and let the server decide", so a denial
// here would be this client inventing a stricter rule than the one enforced.
func TestGrantsDisagreeingOnOrgArePermissive(t *testing.T) {
	grants := DecodeGrants(makeToken(
		`{"grants":[` +
			`{"org":"one","namespaces":["ns"],"volumes":["vol"],"tags":["*"],"scopes":["push"]},` +
			`{"org":"two","namespaces":["*"],"volumes":["*"],"tags":["*"],"scopes":["pull"]}]}`))
	require.True(t, grants.PermitsHeadMove("other", "volume"), "a token with no derivable org should permit everything")
	require.True(t, grants.PermitsResolve("other", "volume"), "a token with no derivable org should permit everything")
}

// TestScopesDoNotImplyOneAnother pins that scope matching is a plain string
// comparison: a broader-sounding scope does not stand in for a narrower one.
func TestScopesDoNotImplyOneAnother(t *testing.T) {
	admin := DecodeGrants(makeToken(
		`{"grants":[{"org":"o","namespaces":["ns"],"volumes":["vol"],"tags":["*"],"scopes":["admin"]}]}`))
	require.False(t, admin.PermitsHeadMove("ns", "vol"), "admin should not stand in for push or tag")
	require.False(t, admin.PermitsResolve("ns", "vol"), "admin should not stand in for pull")
}
