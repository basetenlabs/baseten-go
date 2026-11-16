// Package tokenauth holds what every short-lived Baseten token needs: a cache
// that mints a new token shortly before the current one expires, and sending a
// request again with a new token when the server refuses the current one.
package tokenauth

import (
	"context"
	"sync"
	"time"
)

// Token is a minted bearer token.
type Token struct {
	Value     string
	ExpiresAt time.Time
}

// CacheOptions configures a [Cache].
type CacheOptions struct {
	// Mint gets a new token. Its context carries no caller's cancellation,
	// only MintTimeout.
	Mint func(ctx context.Context) (Token, error)

	// RefreshBefore is how long before expiry a token is replaced, capped at
	// half the token's lifetime so a short-lived token is still reused.
	RefreshBefore time.Duration

	// MintTimeout bounds one mint, which every waiting caller shares.
	MintTimeout time.Duration

	// Now, if set, replaces time.Now.
	Now func() time.Time
}

// Cache holds one token and shares one mint among the callers that ask while
// it is in flight. Safe for concurrent use.
type Cache struct {
	opts CacheOptions

	mu      sync.Mutex
	current *mint
	// previous is the token last dropped for being due for refresh. It is
	// still valid until it expires, so it stands in when a refresh fails.
	previous Token
}

// mint is one call to CacheOptions.Mint. Its fields are set before done is
// closed.
type mint struct {
	done      chan struct{}
	token     Token
	refreshAt time.Time
	err       error
}

// NewCache returns an empty cache; the first mint happens on first use.
func NewCache(opts CacheOptions) *Cache {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Cache{opts: opts}
}

// Token returns the cached token, minting a new one when none is cached or the
// cached one is due for refresh.
func (c *Cache) Token(ctx context.Context) (Token, error) {
	joinedFailure := false
	for {
		c.mu.Lock()
		m := c.current
		started := m == nil
		if started {
			m = c.startMint(ctx)
			c.current = m
		}
		c.mu.Unlock()
		select {
		case <-m.done:
		case <-ctx.Done():
			return Token{}, ctx.Err()
		}
		if m.err != nil {
			// Another caller's failed mint gets one more try, which may be
			// this caller's own; the failed one already left the cache.
			if !started && !joinedFailure {
				joinedFailure = true
				continue
			}
			// A refresh that fails while the replaced token is still valid
			// falls back to it, so a brief outage of the minting endpoint
			// does not fail requests the old token can still serve.
			c.mu.Lock()
			previous := c.previous
			c.mu.Unlock()
			if previous.Value != "" && c.opts.Now().Before(previous.ExpiresAt) {
				return previous, nil
			}
			return Token{}, m.err
		}
		// A token this caller just minted is used even if already due for
		// refresh, so a skewed clock cannot loop here forever.
		if started || c.opts.Now().Before(m.refreshAt) {
			return m.token, nil
		}
		c.mu.Lock()
		if c.current == m {
			c.current = nil
			c.previous = m.token
		}
		c.mu.Unlock()
	}
}

// startMint starts a mint. It runs without the caller's cancellation, since
// one caller giving up must not fail the others waiting on it; each only stops
// waiting on its own. MintTimeout bounds it instead, so a stalled mint cannot
// hold every caller. Must be called with mu held.
func (c *Cache) startMint(ctx context.Context) *mint {
	m := &mint{done: make(chan struct{})}
	go func() {
		mintCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.opts.MintTimeout)
		defer cancel()
		startedAt := c.opts.Now()
		m.token, m.err = c.opts.Mint(mintCtx)
		if m.err == nil {
			c.mu.Lock()
			c.previous = Token{}
			c.mu.Unlock()
			lifetime := max(m.token.ExpiresAt.Sub(startedAt), 0)
			m.refreshAt = m.token.ExpiresAt.Add(-min(c.opts.RefreshBefore, lifetime/2))
		} else {
			// A failed mint is not cached, so the next caller tries again.
			c.mu.Lock()
			if c.current == m {
				c.current = nil
			}
			c.mu.Unlock()
		}
		close(m.done)
	}()
	return m
}

// Invalidate drops token if it is still the cached one, so the next caller
// gets a new one.
func (c *Cache) Invalidate(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.previous.Value == token {
		c.previous = Token{}
	}
	if c.current == nil {
		return
	}
	select {
	case <-c.current.done:
		// Compared by value, so a refusal of an older token never discards a
		// newer one another caller already minted.
		if c.current.token.Value == token {
			c.current = nil
		}
	default:
		// Still minting, so the cached token is already a newer one.
	}
}
