package locks

import "context"

// Health describes a validated local tree at one point in time. Pending counts
// recovery sidecars. Overfull counts leaves that can still split. Terminal
// leaves can exceed 512 records without needing a split.
type Health struct {
	Leaves, Records, Largest, Overfull, Pending int
}

// Inspect validates and measures the tree under an exclusive root lock. It can
// create the persistent guard, but does not change data or recovery sidecars.
// An error means the returned counts are incomplete.
func (s *Local) Inspect(ctx context.Context) (Health, error) {
	if s == nil || s.path == "" {
		return Health{}, ErrFormat
	}
	return s.inspect(ctx, false, false)
}

// Rebalance validates the tree, recovers interrupted publications, and splits
// overfull nonterminal leaves under an exclusive root lock. It never merges
// leaves or chooses between conflicting values. Each split commits separately;
// an error can follow earlier completed recovery actions or splits.
func (s *Local) Rebalance(ctx context.Context) error {
	return s.RebalanceWithOptions(ctx, RebalanceOptions{})
}

// RebalanceOptions selects optional changes to a repaired tree.
type RebalanceOptions struct {
	// Merge combines subtrees with at most 512 records that fit one leaf.
	// Nodes must have matching metadata, including directory traversal rights.
	Merge bool
}

// RebalanceWithOptions also permits explicit merging of small subtrees. Merges
// discard ignored lines and retain record order within each source leaf.
// Earlier repairs can remain committed if a later operation fails.
func (s *Local) RebalanceWithOptions(ctx context.Context, options RebalanceOptions) error {
	if s == nil || s.path == "" {
		return ErrFormat
	}
	if s.options.ReadOnly {
		return ErrDenied
	}
	_, err := s.inspect(ctx, true, options.Merge)
	return err
}
