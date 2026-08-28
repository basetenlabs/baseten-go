package volume

import (
	"context"
)

type pullVolumeTotals struct {
	logicalBytes   *uint64
	fileCount      *uint64
	directoryCount *uint64
}

func (c *Client) loadPullPlan(
	ctx context.Context,
	reader ObjectReader,
	manifestDigest Digest,
	selectors []string,
	progress *progressReporter,
	budget *byteGate,
) (pullPlan, pullVolumeTotals, error) {
	progress.emit(ProgressEvent{Phase: ProgressValidate})
	manifestBody, err := c.readVerifiedObject(
		ctx,
		reader,
		manifestDigest,
		expectedObject{
			kind:            ObjectKindManifest,
			maxDecodedBytes: c.maxManifestBytes,
		},
		budget,
	)
	if err != nil {
		return pullPlan{}, pullVolumeTotals{}, err
	}
	manifest, err := decodeManifest(
		manifestBody.data,
		c.maxManifestBytes,
		c.maxFiles,
		c.effectivePortablePathLimits(),
	)
	manifestBody.release()
	if err != nil {
		return pullPlan{}, pullVolumeTotals{}, err
	}
	objectKinds, err := manifestObjectKinds(manifestDigest, manifest)
	if err != nil {
		return pullPlan{}, pullVolumeTotals{}, err
	}
	var totals pullVolumeTotals
	if len(selectors) != 0 {
		totals.logicalBytes = totalPointer(manifest.TotalSize)
		totals.fileCount = totalPointer(uint64(len(manifest.Files)))
		totals.directoryCount = totalPointer(uint64(len(manifest.Directories)))
	}
	manifest, err = selectManifest(
		manifest,
		selectors,
		c.maxFiles,
		c.effectivePortablePathLimits(),
	)
	if err != nil {
		return pullPlan{}, pullVolumeTotals{}, err
	}
	progress.emit(ProgressEvent{
		Phase:      ProgressValidate,
		TotalItems: totalPointer(uint64(len(manifest.Files))),
		TotalBytes: totalPointer(manifest.TotalSize),
	})
	plan, err := c.buildPullPlan(
		ctx,
		reader,
		manifest,
		progress,
		budget,
		objectKinds,
	)
	if err != nil {
		return pullPlan{}, pullVolumeTotals{}, err
	}
	return plan, totals, nil
}
