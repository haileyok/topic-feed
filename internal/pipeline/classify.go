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
	URL    string // e.g. http://host.docker.internal:8700
	Client *http.Client
}

// Health is the classifier service's /healthz answer.
type Health struct {
	Model           string `json:"model"`
	TaxonomyVersion string `json:"taxonomy_version"`
	PostdocVersion  string `json:"postdoc_version"`
	Device          string `json:"device"`
}

// Prediction is one post's classifier output (top entries only).
type Prediction struct {
	Broad   map[string]float32 `json:"broad"`
	Paths   map[string]float32 `json:"paths"`
	Signals map[string]float32 `json:"signals"`
	Tone    map[string]float32 `json:"tone"`
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
		return h, fmt.Errorf("classifier health: %s", resp.Status)
	}
	return h, json.NewDecoder(resp.Body).Decode(&h)
}

// Classify returns the served model's name and one prediction per text.
func (c *Classifier) Classify(ctx context.Context, texts []string) (string, []Prediction, error) {
	body, _ := json.Marshal(map[string]any{"texts": texts})
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
	if len(out.Results) != len(texts) {
		return "", nil, fmt.Errorf("classify: %d results for %d texts", len(out.Results), len(texts))
	}
	return out.Model, out.Results, nil
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
