package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// SeqAtTime finds a Jetstream sequence number to replay from so that delivery starts
// at or slightly before t. Replay is addressed by sequence number, not time, so this
// walks the archive's segment list (listSegments, paginated) and returns the value to
// pass to WithAfterSeq: one less than the first sequence number of the segment that
// covers t. Events before t still arrive (up to one segment, ~2.5h) and the caller
// drops them. If t is newer than the sealed archive, it returns the archive tip.
func SeqAtTime(ctx context.Context, host, apiKey string, t time.Time) (uint64, error) {
	target := t.UnixMicro()
	client := &http.Client{Timeout: 60 * time.Second}
	cursor := ""
	var tipSeq uint64 // last sealed sequence number seen
	for page := 0; page < 1000; page++ {
		q := url.Values{"limit": {"1000"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			"https://"+host+"/xrpc/network.bsky.jetstream.listSegments?"+q.Encode(), nil)
		if err != nil {
			return 0, err
		}
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err := client.Do(req)
		if err != nil {
			return 0, err
		}
		var body struct {
			Cursor   json.RawMessage `json:"cursor"`
			Segments []struct {
				MinSeq         uint64 `json:"minSeq"`
				MaxSeq         uint64 `json:"maxSeq"`
				MaxWitnessedAt int64  `json:"maxWitnessedAt"`
			} `json:"segments"`
		}
		err = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return 0, fmt.Errorf("listSegments: HTTP %d", resp.StatusCode)
		}
		if err != nil {
			return 0, fmt.Errorf("listSegments: %w", err)
		}
		for _, s := range body.Segments {
			tipSeq = max(tipSeq, s.MaxSeq)
			if s.MaxWitnessedAt >= target {
				if s.MinSeq == 0 {
					return 0, nil
				}
				return s.MinSeq - 1, nil
			}
		}
		cursor = rawCursor(body.Cursor)
		if cursor == "" || len(body.Segments) == 0 {
			break
		}
	}
	if tipSeq == 0 {
		return 0, fmt.Errorf("listSegments returned no segments")
	}
	// t is newer than the sealed archive. Replaying after the tip plans nothing from
	// the archive and goes straight to the live stream, which covers the recent past.
	return tipSeq, nil
}

// rawCursor accepts a cursor encoded as either a JSON string or a number.
func rawCursor(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}
