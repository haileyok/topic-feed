package feedgen

import (
	"context"
	"fmt"
)

var _ PostTextSource = (*Store)(nil)

// PostTexts returns the text of the stored posts among uris, by URI. Posts that aren't stored
// are left out of the answer.
func (s *Store) PostTexts(ctx context.Context, uris []string) (map[string]string, error) {
	out := make(map[string]string, len(uris))
	if len(uris) == 0 {
		return out, nil
	}
	var rows []struct {
		URI  string `ch:"uri"`
		Text string `ch:"text"`
	}
	if err := s.Conn.Select(ctx, &rows, `SELECT uri, text FROM posts WHERE uri IN ?`, uris); err != nil {
		return nil, fmt.Errorf("select post texts: %w", err)
	}
	for _, r := range rows {
		out[r.URI] = r.Text
	}
	return out, nil
}
