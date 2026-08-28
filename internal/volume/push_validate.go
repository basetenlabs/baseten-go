package volume

func validatePushInputStructure(
	inputs pushInputs,
	maxTotalEntries int,
	limits portablePathLimits,
) error {
	index, err := newManifestPathIndex(maxTotalEntries, limits)
	if err != nil {
		return err
	}
	for _, directory := range inputs.directories {
		if err := index.insert(directory.Path, manifestPathDirectory, ""); err != nil {
			return err
		}
	}
	for _, file := range inputs.files {
		if err := index.insert(file.relativePath, manifestPathFile, ""); err != nil {
			return err
		}
	}
	for _, symlink := range inputs.symlinks {
		if err := index.insert(symlink.Path, manifestPathSymlink, symlink.Target); err != nil {
			return err
		}
	}
	return index.validateSymlinks()
}
