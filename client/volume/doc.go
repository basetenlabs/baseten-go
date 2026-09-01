// Package volume holds every type a caller touches when pushing or
// downloading a volume: options, results, progress, warnings, the
// concurrency knobs, the seam a caller fills in, and the error type volume
// operations return. The operations themselves are methods on the management
// client — [PushOptions] and [DownloadOptions] go into
// ManagementClient.PushVolume and ManagementClient.DownloadVolume — so this
// package is data, not connections.
//
// A volume is a versioned directory tree stored by content, so pushing a
// tree that mostly matches one already stored transfers only what differs,
// and downloading one twice transfers nothing the second time.
//
// Two pieces are not built in, because building them in would put a hashing
// library and a cloud SDK into every program that imports this package. Both
// are supplied on the options:
//
//   - Hasher must return an unkeyed BLAKE3 hash with a 32-byte digest. That
//     is the whole content-addressing scheme, so getting it wrong produces a
//     volume no other client can read. Both common libraries need to be told
//     the size explicitly or by default:
//     github.com/zeebo/blake3 with func() hash.Hash { return blake3.New() },
//     or lukechampine.com/blake3 with
//     func() hash.Hash { return blake3.New(32, nil) }.
//     A 64-byte extended output is the mistake to watch for; it is checked
//     against the published test vectors before a transfer starts.
//
//   - Store reads stored objects and unwraps the zstd streams the service
//     may store them as. The two abilities are one interface because they
//     are only ever usable together. aws-sdk-go-v2's GetObject and
//     github.com/klauspost/compress/zstd fill both methods.
package volume
