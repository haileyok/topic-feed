package feedgen

import "context"

// buildNow builds a feed once and waits for it: what the timer does, for tests that want the
// builds to happen at known moments.
func (fs *Feeds) buildNow(ctx context.Context, f Feed) {
	fs.refresh(ctx, fs.stateOf(f.Key()))
}
