//go:build !linux && !darwin

package volume

import "context"

type stagedTreeSnapshot struct {
	root rootedFileStat
}

func (c *Client) verifyStagingAfterExtraction(
	context.Context,
	*rootedDirectory,
	map[string]rootedFileStat,
	pullPlan,
	*progressReporter,
) (stagedTreeSnapshot, error) {
	return stagedTreeSnapshot{}, errAtomicPullUnsupported
}

func (c *Client) verifyStagingForPublication(
	context.Context,
	*rootedDirectory,
	map[string]rootedFileStat,
	pullPlan,
	stagedTreeSnapshot,
) (stagedTreeSnapshot, error) {
	return stagedTreeSnapshot{}, errAtomicPullUnsupported
}
