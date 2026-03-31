// Package require loosely mimics testify/require but keeps the dependency out
// of the module.
package require

import (
	"errors"
	"fmt"
	"testing"
)

func Fail(t testing.TB, msg string, args ...any) {
	t.Helper()
	t.Fatalf(msg, args...)
}

func NoError(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func Error(t testing.TB, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func NotNil(t testing.TB, val any) {
	t.Helper()
	if val == nil {
		t.Fatal("expected non-nil value")
	}
}

func Equal[T comparable](t testing.TB, expected, actual T) {
	t.Helper()
	if expected != actual {
		t.Fatalf("expected %v, got %v", expected, actual)
	}
}

func ErrorAs[T error](t testing.TB, err error) T {
	t.Helper()
	var target T
	if !errors.As(err, &target) {
		t.Fatalf("expected error type %T, got %T: %v", target, err, err)
	}
	return target
}

func Len[T any](t testing.TB, s []T, expected int) {
	t.Helper()
	if len(s) != expected {
		t.Fatalf("expected length %d, got %d", expected, len(s))
	}
}

func Contains(t testing.TB, s, substr string) {
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

func MapEqual[K comparable, V comparable](t testing.TB, m map[K]V, key K, expected V) {
	t.Helper()
	actual, ok := m[key]
	if !ok {
		t.Fatalf("expected key %v in map, not found", key)
	}
	if actual != expected {
		t.Fatalf("expected map[%v] = %v, got %v", key, expected, actual)
	}
}

func True(t testing.TB, val bool, msg string, args ...any) {
	t.Helper()
	if !val {
		t.Fatalf(msg, args...)
	}
}

func False(t testing.TB, val bool, msg string, args ...any) {
	t.Helper()
	if val {
		t.Fatalf(msg, args...)
	}
}

func NotEqual[T comparable](t testing.TB, unexpected, actual T) {
	t.Helper()
	if unexpected == actual {
		t.Fatalf("expected value to differ from %v", unexpected)
	}
}

func Nil(t testing.TB, val any) {
	t.Helper()
	if val != nil {
		// Handle typed nil interface values.
		if fmt.Sprintf("%v", val) != "<nil>" {
			t.Fatalf("expected nil, got %v", val)
		}
	}
}
