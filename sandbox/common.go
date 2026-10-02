package sandbox

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const day = 24 * time.Hour

// formatDuration formats a duration for the control plane, in whole
// milliseconds.
func formatDuration(d time.Duration) string {
	return strconv.FormatInt(d.Round(time.Millisecond).Milliseconds(), 10) + "ms"
}

// parseDuration parses a control plane duration: Go duration syntax, or a
// whole number of days or weeks on its own, at 24 hours per day. what names
// the value in errors.
func parseDuration(value, what string) (time.Duration, error) {
	body := strings.TrimLeft(value, "+-")
	sign := time.Duration(1)
	if strings.HasPrefix(value, "-") {
		sign = -1
	}
	if unit := strings.TrimLeft(body, "0123456789"); len(body) > len(unit) && (unit == "d" || unit == "w") {
		n, err := strconv.ParseInt(strings.TrimSuffix(body, unit), 10, 64)
		if err == nil {
			if unit == "w" {
				n *= 7
			}
			return sign * time.Duration(n) * day, nil
		}
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s is not a valid duration: %q", what, value)
	}
	return d, nil
}

// Formats an execution API timestamp may come in. The API declares none, and
// its examples differ. time.Parse must consume the whole string, so none can
// partially match another. time.RFC1123 is deliberately absent: Go parses a
// zone abbreviation it does not know as offset zero, silently shifting the
// time, while http.TimeFormat accepts only GMT.
var timestampLayouts = []string{
	time.RFC3339Nano,
	http.TimeFormat,
	"2006-01-02 15:04:05.999999999Z07:00",
}

// parseTimestamp parses an execution API timestamp. Empty is the zero time.
// what names the value in errors.
func parseTimestamp(value, what string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	for _, layout := range timestampLayouts {
		if t, err := time.Parse(layout, value); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("%s is not a recognized timestamp: %q", what, value)
}

// deref returns the value a pointer points to, or the zero value for nil.
func deref[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}

// optional returns a pointer to value, or nil for the zero value, so unset
// fields are omitted from requests.
func optional[T comparable](value T) *T {
	var zero T
	if value == zero {
		return nil
	}
	return &value
}

// optionalSlice returns a pointer to a set slice, nil for a nil one, so an
// empty but set slice is still sent.
func optionalSlice[T any](values []T) *[]T {
	if values == nil {
		return nil
	}
	return &values
}

// optionalMap returns a pointer to a set map, nil for a nil one, so an empty
// but set map is still sent.
func optionalMap[K comparable, V any](values map[K]V) *map[K]V {
	if values == nil {
		return nil
	}
	return &values
}
