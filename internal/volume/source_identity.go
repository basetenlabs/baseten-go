package volume

type sourceIdentity struct {
	available      bool
	device         uint64
	inode          uint64
	changedSeconds int64
	changedNanos   int64
}
