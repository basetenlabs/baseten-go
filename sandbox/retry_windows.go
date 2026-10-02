package sandbox

import "syscall"

// Windows reports socket failures as WSA error codes, which the portable
// syscall constants, such as syscall.ECONNRESET, do not match. syscall exports
// only some of them, so the rest are their documented values.
var platformTransientErrors = []error{
	syscall.WSAECONNRESET,
	syscall.WSAECONNABORTED,
	syscall.Errno(10060), // WSAETIMEDOUT
	syscall.Errno(10061), // WSAECONNREFUSED
}
