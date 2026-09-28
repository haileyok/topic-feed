package ingest

import "sync"

// TextCache holds the text of recently seen posts, keyed by URI, so quoted posts can
// usually be resolved from memory. When full it evicts the oldest entries first.
type TextCache struct {
	mu    sync.Mutex
	max   int
	texts map[string]string
	order []string // ring buffer of URIs in insertion order
	next  int
}

func NewTextCache(max int) *TextCache {
	return &TextCache{max: max, texts: make(map[string]string, max), order: make([]string, 0, max)}
}

func (c *TextCache) Put(uri, text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.texts[uri]; ok {
		c.texts[uri] = text
		return
	}
	if len(c.order) < c.max {
		c.order = append(c.order, uri)
	} else {
		delete(c.texts, c.order[c.next])
		c.order[c.next] = uri
		c.next = (c.next + 1) % c.max
	}
	c.texts[uri] = text
}

func (c *TextCache) Get(uri string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.texts[uri]
	return t, ok
}

func (c *TextCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.texts)
}
