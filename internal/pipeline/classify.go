package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Classifier calls the classifier service (trainer/serve.py).
type Classifier struct {
	URL    string // e.g. http://127.0.0.1:8700
	Client *http.Client
}

// Health is the classifier service's /healthz answer.
type Health struct {
	Model           string   `json:"model"`
	TaxonomyVersion string   `json:"taxonomy_version"`
	PostdocVersion  string   `json:"postdoc_version"`
	Device          string   `json:"device"`
	MaxImages       int      `json:"max_images"` // pictures per post the model looks at
	Signals         []string `json:"signals"`
}

// Item is one post for the classifier: its rendered text and the pictures to look at (encoded
// image files, at most Health.MaxImages).
type Item struct {
	Text     string   `json:"text"`
	Pictures [][]byte `json:"pictures,omitempty"` // JSON base64
}

// Prediction is one post's classifier output (top entries only).
type Prediction struct {
	Broad        map[string]float32 `json:"broad"`
	Paths        map[string]float32 `json:"paths"`
	Signals      map[string]float32 `json:"signals"`
	Tone         map[string]float32 `json:"tone"`
	PicturesUsed int                `json:"pictures_used"` // how many of the pictures could be read and were used
}

// Health asks which model is being served.
func (c *Classifier) Health(ctx context.Context) (Health, error) {
	var h Health
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.URL, "/")+"/healthz", nil)
	if err != nil {
		return h, err
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return h, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return h, fmt.Errorf("healthz: %s", resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		return h, fmt.Errorf("healthz: %w", err)
	}
	return h, nil
}

// Classify returns the served model's name and one prediction per item.
func (c *Classifier) Classify(ctx context.Context, items []Item) (string, []Prediction, error) {
	body, err := json.Marshal(map[string]any{"posts": items})
	if err != nil {
		return "", nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.URL, "/")+"/classify", bytes.NewReader(body))
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Client.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		return "", nil, fmt.Errorf("classify: %s: %s", resp.Status, raw)
	}
	var out struct {
		Model   string       `json:"model"`
		Results []Prediction `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", nil, fmt.Errorf("classify: %w", err)
	}
	if len(out.Results) != len(items) {
		return "", nil, fmt.Errorf("classify: %d results for %d posts", len(out.Results), len(items))
	}
	return out.Model, out.Results, nil
}

// Limits for one /classify request: the service reads the whole body, and pictures are base64 in
// JSON, so keep requests to a few MB of pictures and a few hundred posts.
const (
	maxRequestPosts        = 512
	maxRequestPictureBytes = 12 << 20
)

// chunkItems splits items into consecutive ranges [lo, hi) that each fit one request.
func chunkItems(items []Item) [][2]int {
	var out [][2]int
	lo, size := 0, 0
	for i, it := range items {
		n := 0
		for _, pic := range it.Pictures {
			n += len(pic)
		}
		if i > lo && (i-lo >= maxRequestPosts || size+n > maxRequestPictureBytes) {
			out = append(out, [2]int{lo, i})
			lo, size = i, 0
		}
		size += n
	}
	if lo < len(items) {
		out = append(out, [2]int{lo, len(items)})
	}
	return out
}

// top returns the highest-probability key and its probability.
func top(m map[string]float32) (string, float32) {
	var k string
	var p float32 = -1
	for kk, pp := range m {
		if pp > p || (pp == p && kk < k) {
			k, p = kk, pp
		}
	}
	if p < 0 {
		return "", 0
	}
	return k, p
}
