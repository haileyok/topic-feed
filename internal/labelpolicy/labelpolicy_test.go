package labelpolicy

import "testing"

func TestRepoPolicy(t *testing.T) {
	p, err := Load("../../config/label_policy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		labels []string
		want   string
	}{
		{nil, OK},
		{[]string{"!warn"}, OK}, // not in either list
		{[]string{"porn"}, AdultOnly},
		{[]string{"sexual", "nudity"}, AdultOnly},
		{[]string{"porn", "!takedown"}, Drop}, // drop beats adult_only
		{[]string{"spam"}, Drop},
		{[]string{"graphic-media"}, Drop},
	}
	for _, c := range cases {
		if got := p.Decide(c.labels); got != c.want {
			t.Errorf("Decide(%v) = %s, want %s", c.labels, got, c.want)
		}
	}
}
