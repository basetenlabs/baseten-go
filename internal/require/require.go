// Package require loosely mimics testify/require but keeps the dependency out
// of the module.
package require

import (
	"errors"
	"fmt"
	"regexp"
	"time"
)

// T is the part of testing.TB that require uses.
type T interface {
	Helper()
	Fatal(args ...any)
	Fatalf(format string, args ...any)
}

func Fail(t T, msg string, args ...any) {
	t.Helper()
	t.Fatalf(msg, args...)
}

func NoError(t T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func Error(t T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func NotNil(t T, val any) {
	t.Helper()
	if val == nil {
		t.Fatal("expected non-nil value")
	}
}

func Equal[V comparable](t T, expected, actual V) {
	t.Helper()
	if expected != actual {
		t.Fatalf("expected %v, got %v", expected, actual)
	}
}

func ErrorAs[E error](t T, err error) E {
	t.Helper()
	var target E
	if !errors.As(err, &target) {
		t.Fatalf("expected error type %T, got %T: %v", target, err, err)
	}
	return target
}

func Len[V any](t T, s []V, expected int) {
	t.Helper()
	if len(s) != expected {
		t.Fatalf("expected length %d, got %d", expected, len(s))
	}
}

func Contains(t T, s, substr string) {
	t.Helper()
	if len(s) == 0 || len(substr) == 0 {
		t.Fatalf("expected %q to contain %q", s, substr)
	}
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return
		}
	}
	t.Fatalf("expected %q to contain %q", s, substr)
}

func MapEqual[K comparable, V comparable](t T, m map[K]V, key K, expected V) {
	t.Helper()
	actual, ok := m[key]
	if !ok {
		t.Fatalf("expected key %v in map, not found", key)
	}
	if actual != expected {
		t.Fatalf("expected map[%v] = %v, got %v", key, expected, actual)
	}
}

func True(t T, val bool, msg string, args ...any) {
	t.Helper()
	if !val {
		t.Fatalf(msg, args...)
	}
}

func False(t T, val bool, msg string, args ...any) {
	t.Helper()
	if val {
		t.Fatalf(msg, args...)
	}
}

func NotEqual[V comparable](t T, unexpected, actual V) {
	t.Helper()
	if unexpected == actual {
		t.Fatalf("expected value to differ from %v", unexpected)
	}
}

func Regexp(t T, pattern, s string) {
	t.Helper()
	matched, err := regexp.MatchString(pattern, s)
	if err != nil {
		t.Fatalf("invalid regexp %q: %v", pattern, err)
	}
	if !matched {
		t.Fatalf("expected %q to match %q", s, pattern)
	}
}

func Nil(t T, val any) {
	t.Helper()
	if val != nil {
		// Handle typed nil interface values.
		if fmt.Sprintf("%v", val) != "<nil>" {
			t.Fatalf("expected nil, got %v", val)
		}
	}
}

// Eventually calls check until it passes, waiting interval between the end of
// one call and the start of the next, and returns how long that took. A
// failure through check's t fails only that attempt, which is retried; once
// timeout passes, the test fails with the last attempt's failure. Failing the
// outer t inside check fails the test at once.
func Eventually(t T, timeout, interval time.Duration, check func(t T)) time.Duration {
	t.Helper()
	start := time.Now()
	for {
		attempt := &attemptT{}
		func() {
			defer func() {
				if r := recover(); r != nil && r != attempt {
					panic(r)
				}
			}()
			check(attempt)
		}()
		if attempt.failure == "" {
			return time.Since(start)
		}
		if time.Since(start)+interval > timeout {
			t.Fatalf("still failing after %v: %s", time.Since(start).Round(time.Millisecond), attempt.failure)
		}
		time.Sleep(interval)
	}
}

// attemptT fails only the current Eventually attempt.
type attemptT struct {
	failure string
}

func (a *attemptT) Helper() {}

func (a *attemptT) Fatal(args ...any) {
	a.failure = fmt.Sprint(args...)
	panic(a)
}

func (a *attemptT) Fatalf(format string, args ...any) {
	a.failure = fmt.Sprintf(format, args...)
	panic(a)
}
