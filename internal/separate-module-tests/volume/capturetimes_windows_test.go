//go:build !unix

package separatemoduletests_test

import "time"

// setSymlinkTime is a no-op here: there is no lutimes, and the capture's
// byte-identity test is already gated off this platform by
// requireExpressibleModes, so the link's time is never compared.
func setSymlinkTime(string, time.Time) error { return nil }
