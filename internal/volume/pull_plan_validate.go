package volume

func validatePullPlanStructure(
	plan pullPlan,
	maxTotalEntries int,
	limits portablePathLimits,
) error {
	index, err := newManifestPathIndex(maxTotalEntries, limits)
	if err != nil {
		return err
	}
	for _, directory := range plan.directories {
		if err := index.insert(directory.Path, manifestPathDirectory, ""); err != nil {
			return err
		}
	}
	for _, file := range plan.files {
		if err := index.insert(file.path, manifestPathFile, ""); err != nil {
			return err
		}
	}
	for _, symlink := range plan.symlinks {
		if err := index.insert(symlink.Path, manifestPathSymlink, symlink.Target); err != nil {
			return err
		}
	}
	return index.validateSymlinks()
}
