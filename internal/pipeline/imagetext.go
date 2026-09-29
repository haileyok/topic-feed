package pipeline

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Image text sources recorded in post_pipeline.image_text_sources.
const (
	SourceOCR    = "ocr"
	SourceLLM    = "luna"
	SourceNone   = "none"   // tesseract found too little and no description was available
	SourceBudget = "budget" // the daily LLM budget was used up
	SourceError  = "error"
)

// ThumbnailURL returns a small JPEG of an attachment: the CDN feed thumbnail for images,
// the poster frame for videos.
func ThumbnailURL(kind, did, cid string) string {
	if kind == "video" {
		return "https://video.bsky.app/watch/" + url.PathEscape(did) + "/" + cid + "/thumbnail.jpg"
	}
	return "https://cdn.bsky.app/img/feed_thumbnail/plain/" + did + "/" + cid + "@jpeg"
}

// Fetch downloads an image, up to maxBytes.
func Fetch(ctx context.Context, client *http.Client, u string, maxBytes int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "topic-feed-pipeline")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxBytes))
}

// OCR runs tesseract on an image and keeps confidently read words.
type OCR struct {
	Binary  string  // tesseract executable
	MinConf float64 // word confidence (0-100) to keep a word
	Workers int     // tesseract processes at once
	sem     chan struct{}
	once    sync.Once
}

var realWord = regexp.MustCompile(`\pL{2,}`)

// Words returns the confidently read words in reading order.
func (o *OCR) Words(ctx context.Context, img []byte) ([]string, error) {
	o.once.Do(func() { o.sem = make(chan struct{}, max(1, o.Workers)) })
	select {
	case o.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-o.sem }()
	cmd := exec.CommandContext(ctx, o.Binary, "stdin", "stdout", "tsv")
	cmd.Stdin = bytes.NewReader(img)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("tesseract: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return ParseTSV(out.Bytes(), o.MinConf), nil
}

// ParseTSV reads tesseract's TSV output and returns words at or above minConf that
// contain at least two letters.
func ParseTSV(tsv []byte, minConf float64) []string {
	var words []string
	sc := bufio.NewScanner(bytes.NewReader(tsv))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	first := true
	for sc.Scan() {
		if first { // header
			first = false
			continue
		}
		cols := strings.Split(sc.Text(), "\t")
		if len(cols) < 12 {
			continue
		}
		conf, err := strconv.ParseFloat(cols[10], 64)
		w := strings.TrimSpace(cols[11])
		if err != nil || conf < minConf || !realWord.MatchString(w) {
			continue
		}
		words = append(words, w)
	}
	return words
}

// Describer asks a vision LLM for a short description of an image.
type Describer struct {
	Endpoint string // OpenAI-compatible base URL (the AI gateway)
	APIKey   string
	Model    string
	Client   *http.Client
	Prices   Prices
	Budget   *Budget
	sem      chan struct{}
	once     sync.Once
	Workers  int
}

// Prices are list prices in USD per million tokens.
type Prices struct{ Input, CachedInput, Output float64 }

const describePrompt = "Describe this image in one or two sentences, like alt text, for deciding what topic a social media post is about. " +
	"Include any clearly readable text, and name recognizable things such as sports teams, games, shows, brands, or well-known people. " +
	"Plain description only, at most 50 words."

// ErrBudget means the daily budget is used up.
var ErrBudget = errors.New("daily LLM budget used up")

// Describe returns a description of an image (an https or data: URL) and its
// list-price cost.
func (d *Describer) Describe(ctx context.Context, imageURL string) (string, float64, error) {
	d.once.Do(func() { d.sem = make(chan struct{}, max(1, d.Workers)) })
	if !d.Budget.Allow() {
		return "", 0, ErrBudget
	}
	select {
	case d.sem <- struct{}{}:
	case <-ctx.Done():
		return "", 0, ctx.Err()
	}
	defer func() { <-d.sem }()

	body, _ := json.Marshal(map[string]any{
		"model": d.Model, "max_completion_tokens": 400,
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": describePrompt},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": imageURL, "detail": "low"}},
		}}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(d.Endpoint, "/")+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Authorization", "Bearer "+d.APIKey)
	req.Header.Set("x-agw-key", d.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Client", "topic-feed-pipeline")
	resp, err := d.Client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("describe: %s: %.200s", resp.Status, raw)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			PromptTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", 0, fmt.Errorf("describe: %w", err)
	}
	u := out.Usage
	cost := (float64(u.PromptTokens-u.PromptTokensDetails.CachedTokens)*d.Prices.Input +
		float64(u.PromptTokensDetails.CachedTokens)*d.Prices.CachedInput +
		float64(u.CompletionTokens)*d.Prices.Output) / 1e6
	d.Budget.Add(cost)
	if len(out.Choices) == 0 {
		return "", cost, errors.New("describe: no choices")
	}
	return strings.Join(strings.Fields(out.Choices[0].Message.Content), " "), cost, nil
}

// Budget caps LLM spending per UTC day.
type Budget struct {
	PerDay float64
	mu     sync.Mutex
	day    string
	spent  float64
}

// NewBudget starts a budget with what was already spent today.
func NewBudget(perDay, spentToday float64) *Budget {
	return &Budget{PerDay: perDay, day: time.Now().UTC().Format(time.DateOnly), spent: spentToday}
}

func (b *Budget) roll() {
	if d := time.Now().UTC().Format(time.DateOnly); d != b.day {
		b.day, b.spent = d, 0
	}
}

// Allow reports whether today's spending is still under the cap.
func (b *Budget) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.roll()
	return b.spent < b.PerDay
}

// Add records spending.
func (b *Budget) Add(usd float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.roll()
	b.spent += usd
	metricLLMSpentToday.Set(b.spent)
}
