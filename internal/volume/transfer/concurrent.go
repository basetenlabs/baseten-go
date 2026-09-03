package transfer

import (
	"context"
	"sync"
)

// forEach runs fn over items, at most limit at a time, and stops at the first
// failure.
//
// Both engines process files this way, and the loop is the trickiest code in
// either: a bounded fan-out that has to cancel its siblings on the first
// error, wait for them, and report the original cause rather than the
// cancellation it caused. Keeping one copy is worth the generic, because two
// copies means fixing that reasoning twice.
//
// fn receives each item's index so a caller can write results into a slice
// without synchronizing: every call has an index of its own.
func forEach[T any](ctx context.Context, limit int, items []T, fn func(context.Context, int, T) error) error {
	if limit < 1 {
		limit = 1
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	fail := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		if firstErr == nil {
			// Everything still running is now working toward a result that
			// will be thrown away.
			firstErr = err
			cancel()
		}
	}

	slots := make(chan struct{}, limit)
	for i, item := range items {
		// Checked before the select below, because when a slot is free and the
		// context is already done both of its cases are ready and the choice
		// between them is random — so a caller who gave up before this loop
		// started would still get work done, up to one item per slot. On a
		// download that work reaches the destination filesystem: a file is
		// created and sized before anything consults the context.
		//
		// Being cancelled between this check and the select is inherent and
		// harmless; the select's own ctx.Done case catches it.
		if err := ctx.Err(); err != nil {
			wg.Wait()
			mu.Lock()
			defer mu.Unlock()
			if firstErr != nil {
				return firstErr
			}
			return err
		}

		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			mu.Lock()
			defer mu.Unlock()
			// The cancellation is almost always this loop reacting to a
			// failure it already recorded; that cause is the useful one.
			if firstErr != nil {
				return firstErr
			}
			return ctx.Err()
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			if err := fn(ctx, i, item); err != nil {
				fail(err)
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	return firstErr
}
