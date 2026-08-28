package volume

func decodeLimits(maxDecodedBytes uint64) DecodeLimits {
	resourceBytes := max(maxDecodedBytes, uint64(minZstdResourceBytes))
	return DecodeLimits{
		MaxEncodedBytes: maxDecodedBytes + metadataEncodingOverhead,
		MaxDecodedBytes: maxDecodedBytes,
		MaxWindowBytes:  resourceBytes,
		MaxMemoryBytes:  resourceBytes,
	}
}
