package sandbox

import (
	"testing"
	"time"

	"github.com/basetenlabs/baseten-go/internal/require"
)

func TestParseDuration(t *testing.T) {
	for value, expected := range map[string]time.Duration{
		"0":         0,
		"90s":       90 * time.Second,
		"1h30m":     90 * time.Minute,
		"3600000ms": time.Hour,
		"7d":        7 * day,
		"2w":        14 * day,
		"-1d":       -day,
	} {
		d, err := parseDuration(value, "test")
		require.NoError(t, err)
		require.Equal(t, expected, d)
	}
	for _, value := range []string{"", "1d12h", "d", "1x", "abc"} {
		_, err := parseDuration(value, "test")
		require.Error(t, err)
	}
}

func TestFormatDuration(t *testing.T) {
	require.Equal(t, "90000ms", formatDuration(90*time.Second))
	require.Equal(t, "2ms", formatDuration(1500*time.Microsecond))
	d, err := parseDuration(formatDuration(36*time.Hour), "test")
	require.NoError(t, err)
	require.Equal(t, 36*time.Hour, d)
}

func TestParseTimestamp(t *testing.T) {
	expected := time.Date(2023, 1, 1, 12, 0, 0, 0, time.UTC)
	for _, value := range []string{
		"2023-01-01T12:00:00Z",
		"2023-01-01T12:00:00.000000000Z",
		"Sun, 01 Jan 2023 12:00:00 GMT",
		"2023-01-01 12:00:00+00:00",
		"2023-01-01 13:00:00+01:00",
	} {
		parsed, err := parseTimestamp(value, "test")
		require.NoError(t, err)
		require.True(t, expected.Equal(parsed), "%q parsed as %v", value, parsed)
	}

	t.Run("EmptyIsZero", func(t *testing.T) {
		parsed, err := parseTimestamp("", "test")
		require.NoError(t, err)
		require.True(t, parsed.IsZero(), "expected zero time")
	})

	t.Run("RejectsUnknown", func(t *testing.T) {
		for _, value := range []string{
			"yesterday",
			"2023-01-01T12:00:00Z extra",
			// An ambiguous zone abbreviation would silently parse as UTC.
			"Sun, 01 Jan 2023 12:00:00 PST",
		} {
			_, err := parseTimestamp(value, "process start time")
			require.Error(t, err)
			require.Contains(t, err.Error(), "process start time")
		}
	})
}
