package bdn

import (
	"encoding/base64"
	"encoding/json"
	"slices"
	"strings"
)

// Scopes a grant can carry.
const (
	ScopePull = "pull"
	ScopePush = "push"
	ScopeTag  = "tag"
)

// Grants are the capabilities a token claims, read from the token itself
// without verifying its signature.
//
// This reads the minted token rather than the scope list the exchange endpoint
// reports, because only the token says what was actually granted. For a token
// from that endpoint the two currently coincide, since the mint issues a grant
// whose volume and tag selectors are wildcards and narrows only namespaces and
// scopes — but that invariant lives in a different service, and reading the
// token stays correct whether or not it holds.
//
// This is a hint about what is worth attempting, never an authorization
// decision: the server verifies the signature and enforces the grants on every
// request. It is used to skip a request that would certainly be denied — the
// prior-version lookup a push makes only as an optimization — and to ask for a
// head move only when the token could plausibly carry one, so that a push
// without head permission fails at its own precondition rather than at the
// commit that would otherwise have succeeded.
//
// An undecodable or absent token yields permissive grants: with nothing to go
// on, attempt the operation and let the server answer.
type Grants struct {
	// permissive is set when nothing could be read from the token.
	permissive bool
	org        string
	grants     []grant
}

type grant struct {
	Org        string   `json:"org"`
	Namespaces []string `json:"namespaces"`
	Volumes    []string `json:"volumes"`
	Tags       []string `json:"tags"`
	Scopes     []string `json:"scopes"`
}

// claims is the part of the token payload these checks read.
type claims struct {
	Org    string  `json:"org"`
	Grants []grant `json:"grants"`
}

// DecodeGrants reads the grants out of a token's payload. The signature is not
// checked, and cannot be: the client has no key. See [Grants].
func DecodeGrants(token string) Grants {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return Grants{permissive: true}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Grants{permissive: true}
	}
	var c claims
	if err := json.Unmarshal(payload, &c); err != nil || len(c.Grants) == 0 {
		return Grants{permissive: true}
	}
	// The server lowercases namespace and volume selectors when it verifies,
	// so a mixed-case grant that the server would accept has to be folded the
	// same way here or this check disagrees with the one that matters.
	for i := range c.Grants {
		for j := range c.Grants[i].Namespaces {
			c.Grants[i].Namespaces[j] = strings.ToLower(c.Grants[i].Namespaces[j])
		}
		for j := range c.Grants[i].Volumes {
			c.Grants[i].Volumes[j] = strings.ToLower(c.Grants[i].Volumes[j])
		}
	}
	// The org is the top-level claim when the token carries one. The service
	// treats that claim as the org and requires every grant to name the same
	// one, refusing the token outright when they disagree — so on any token
	// the service would accept, the claim and the grants already agree.
	//
	// A token minted before issuers wrote that claim carries only per-grant
	// orgs, and the service still derives the org from them in that case. The
	// same fallback is kept here for the same reason, and can go when it goes
	// there.
	org := c.Org
	if org == "" {
		org = c.Grants[0].Org
	}
	// Disagreement, or no org at all, is a token the service refuses. Neither
	// is treated as a denial here: the check this backs is a hint, so the
	// honest answer is to attempt the operation and let the service decide.
	if org == "" {
		return Grants{permissive: true}
	}
	for _, grant := range c.Grants {
		if grant.Org != org {
			return Grants{permissive: true}
		}
	}
	return Grants{org: org, grants: c.Grants}
}

// Permits reports whether some grant covers the namespace, volume, and tag,
// with at least one of the scopes.
//
// The rule it implements: one grant belonging to the token's org must cover
// all three levels, each either by an exact match or by a "*" selector, and
// must carry one of the scopes. Scope matching is a plain string comparison in
// which no scope implies another, so "admin" does not stand in for "push".
//
// The server applies a stricter variant on some routes, which additionally
// requires a "*" selector at any level the request leaves unnamed. Both
// callers here name all three levels, and with nothing unnamed the two agree —
// which is why one function serves both. A caller that left a level unnamed
// would need the stricter rule instead.
func (g Grants) Permits(namespace, volume, tag string, scopes ...string) bool {
	if g.permissive {
		return true
	}
	namespace, volume = strings.ToLower(namespace), strings.ToLower(volume)
	for _, grant := range g.grants {
		if grant.Org != g.org {
			continue
		}
		if !covers(grant.Namespaces, namespace) || !covers(grant.Volumes, volume) || !covers(grant.Tags, tag) {
			continue
		}
		for _, scope := range scopes {
			if slices.Contains(grant.Scopes, scope) {
				return true
			}
		}
	}
	return false
}

// PermitsHeadMove reports whether the token could move the volume's head,
// which is the reserved tag "head" under a push or tag scope. It mirrors the
// check the commit handler enforces.
func (g Grants) PermitsHeadMove(namespace, volume string) bool {
	return g.Permits(namespace, volume, HeadTag, ScopePush, ScopeTag)
}

// PermitsResolve reports whether the token could resolve the volume's head,
// the request a push makes to find the version it can reuse from. A ref naming
// no tag authorizes as the reserved head tag under the pull scope.
func (g Grants) PermitsResolve(namespace, volume string) bool {
	return g.Permits(namespace, volume, HeadTag, ScopePull)
}

func covers(allowed []string, requested string) bool {
	return slices.Contains(allowed, "*") || slices.Contains(allowed, requested)
}
