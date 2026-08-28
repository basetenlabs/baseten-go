//go:build !linux && !darwin

package volume

import "io/fs"

func sourceIdentityFor(_ fs.FileInfo) sourceIdentity {
	return sourceIdentity{}
}
