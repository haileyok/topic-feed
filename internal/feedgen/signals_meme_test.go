package feedgen

import "testing"

func TestSignalsIncludeMeme(t *testing.T) {
	found := false
	for _, s := range Signals {
		found = found || s == "meme"
	}
	if !found {
		t.Fatalf("meme missing from Signals %v", Signals)
	}
	// A cutoff on meme must validate like any other signal.
	if err := (Rules{Max: map[string]float32{"meme": 0.5}}).validate("signal", Signals); err != nil {
		t.Errorf("meme cutoff: %v", err)
	}
	if err := (Rules{Max: map[string]float32{"not_a_signal": 0.5}}).validate("signal", Signals); err == nil {
		t.Error("an unknown signal name should be rejected")
	}
}
