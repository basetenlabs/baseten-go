package volume

// TransferObservation reports storage-neutral signals observed inside one
// ObjectReader or ObjectUploader invocation. RetryCount excludes the initial
// attempt. StallObserved is true when any attempt encountered throttling,
// congestion, queue saturation, or a no-progress transport stall.
//
// Adapters must not use StallObserved for caller cancellation, authentication,
// authorization, missing content, validation, or integrity failures. They must
// not attach endpoints, status payloads, raw causes, credentials, or
// provider-specific fields to this value.
//
// A successful nonempty transfer with no retries or stall is a clean AIMD
// success and contributes a latency sample. A retry without a stall is neutral.
// A reported stall cuts concurrency. A deduplicated or otherwise empty transfer
// is neutral regardless of its observation, and cancellation is always neutral.
type TransferObservation struct {
	RetryCount    uint32
	StallObserved bool
}

func snapshotTransferObservation(observation *TransferObservation) TransferObservation {
	if observation == nil {
		return TransferObservation{}
	}
	return *observation
}
