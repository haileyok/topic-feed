package main

import "testing"

func TestStoredModel(t *testing.T) {
	for in, want := range map[string]string{
		"v4":   "v4",
		"none": "", // rows the pipeline never classified are stored with an empty model
		"":     "", // (main refuses an empty flag before this is used)
		"None": "None",
	} {
		if got := storedModel(in); got != want {
			t.Errorf("storedModel(%q) = %q, want %q", in, got, want)
		}
	}
}
