package windows

import (
	"path/filepath"
	"testing"
	"time"
)

// The committed labeling windows must load, cover every UTC hour once, and be 15
// minutes each.
func TestRepoWindows(t *testing.T) {
	ws, err := Load(filepath.Join("..", "..", "config", "labeling_windows.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	hours := map[int]bool{}
	for _, w := range ws {
		if w.End.Sub(w.Start) != 15*time.Minute {
			t.Errorf("%s is %v long", w.ID, w.End.Sub(w.Start))
		}
		hours[w.Start.Hour()] = true
	}
	if len(ws) != 24 || len(hours) != 24 {
		t.Errorf("%d windows covering %d distinct hours, want 24 and 24", len(ws), len(hours))
	}
	if got := Of(ws, ws[3].Start.Add(time.Minute)); got != ws[3].ID {
		t.Errorf("Of = %q, want %q", got, ws[3].ID)
	}
}
