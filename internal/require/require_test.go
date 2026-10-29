package require

import (
	"fmt"
	"testing"
	"time"
)

// fakeT records a fatal failure and panics to stop like FailNow.
type fakeT struct {
	failure string
}

func (f *fakeT) Helper() {}

func (f *fakeT) Fatal(args ...any) { f.Fatalf("%s", fmt.Sprint(args...)) }

func (f *fakeT) Fatalf(format string, args ...any) {
	f.failure = fmt.Sprintf(format, args...)
	panic(f)
}

func TestEventuallyRetriesUntilPass(t *testing.T) {
	attempts := 0
	Eventually(t, time.Second, time.Millisecond, func(attempt T) {
		attempts++
		Equal(attempt, 3, attempts)
	})
	Equal(t, 3, attempts)
}

func TestEventuallyFailsWithLastFailure(t *testing.T) {
	outer := &fakeT{}
	attempts := 0
	func() {
		defer func() {
			if r := recover(); r != outer {
				panic(r)
			}
		}()
		Eventually(outer, 20*time.Millisecond, 5*time.Millisecond, func(attempt T) {
			attempts++
			Fail(attempt, "attempt %d", attempts)
		})
	}()
	True(t, attempts > 1, "expected retries, got %d attempts", attempts)
	Contains(t, outer.failure, fmt.Sprintf("attempt %d", attempts))
}
