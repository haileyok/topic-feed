package feedgen

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/haileyok/topic-feed/internal/chdb"
)

// TuningStore reads and saves how viewers have tuned their personal feeds. *Store implements it.
type TuningStore interface {
	// ViewerTuning reads a viewer's saved tuning; the zero tuning when they have none.
	ViewerTuning(ctx context.Context, did string) (Tuning, error)
	// SaveTuning stores a viewer's tuning, replacing the previous one.
	SaveTuning(ctx context.Context, did string, t Tuning) error
}

var _ TuningStore = (*Store)(nil)

func (s *Store) ViewerTuning(ctx context.Context, did string) (Tuning, error) {
	var rows []struct {
		Settings string `ch:"settings"`
	}
	// Every save is a row and old ones are only dropped when parts merge, so take the newest
	// rather than counting on a merge having happened.
	if err := s.Conn.Select(ctx, &rows, `
		SELECT settings FROM viewer_settings WHERE viewer_did = ? ORDER BY updated_at DESC LIMIT 1`, did); err != nil {
		return Tuning{}, fmt.Errorf("select viewer settings (is schema/010_viewer_settings.sql applied?): %w", err)
	}
	if len(rows) == 0 {
		return Tuning{}, nil
	}
	var t Tuning
	if err := json.Unmarshal([]byte(rows[0].Settings), &t); err != nil {
		return Tuning{}, fmt.Errorf("viewer settings for %s are not valid: %w", did, err)
	}
	return t, nil
}

func (s *Store) SaveTuning(ctx context.Context, did string, t Tuning) error {
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return chdb.Insert(ctx, s.Conn, "viewer_settings", []struct {
		ViewerDID string    `ch:"viewer_did"`
		Settings  string    `ch:"settings"`
		UpdatedAt time.Time `ch:"updated_at"`
	}{{ViewerDID: did, Settings: string(b), UpdatedAt: time.Now().UTC()}})
}
