package polecat

// MinPolecatDirsPerRig is the floor for the per-rig polecat directory cap. A rig's
// effective cap is max(its configured max_polecats, MinPolecatDirsPerRig). Fresh
// polecat allocation is refused once a rig's polecat-directory count reaches the
// cap, so stale directories must be reclaimed to keep spawns from deadlocking.
const MinPolecatDirsPerRig = 30

// EffectivePolecatDirCap returns the per-rig polecat directory cap: the configured
// max_polecats floored at MinPolecatDirsPerRig.
func EffectivePolecatDirCap(configured int) int {
	if configured < MinPolecatDirsPerRig {
		return MinPolecatDirsPerRig
	}
	return configured
}
