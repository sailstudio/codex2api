package prismchannel

import (
	"container/list"
	"encoding/json"
	"sync"
	"time"
)

// continuity is private, owner-scoped state; it is never returned through the facade.
type continuity struct {
	SlotID            string
	ProjectID         string
	SandboxGeneration string
	ConversationID    string
	UpstreamID        string
	Snapshot          map[string]any
}
type storedTurn struct {
	Items      []Item
	Continuity *continuity `json:",omitempty"`
}

type cacheKey struct{ owner, id string }
type cacheEntry struct {
	key     cacheKey
	data    []byte
	tokens  int64
	expires time.Time
}
type TokenCache struct {
	mu         sync.Mutex
	entries    map[cacheKey]*list.Element
	lru        *list.List
	bytes      int64
	maxBytes   int64
	maxEntries int
	ttl        time.Duration
	metrics    *Metrics
}

func newTokenCache(c Config, m *Metrics) *TokenCache {
	return &TokenCache{entries: make(map[cacheKey]*list.Element), lru: list.New(), maxBytes: c.CacheBytes, maxEntries: c.CacheEntries, ttl: c.CacheTTL, metrics: m}
}
func (c *TokenCache) remove(e *list.Element) {
	v := e.Value.(cacheEntry)
	delete(c.entries, v.key)
	c.bytes -= int64(len(v.data))
	c.lru.Remove(e)
}
func (c *TokenCache) Get(owner, id string) ([]Item, int64, bool) {
	items, tokens, _, ok := c.getTurn(owner, id)
	return items, tokens, ok
}
func (c *TokenCache) getTurn(owner, id string) ([]Item, int64, *continuity, bool) {
	c.mu.Lock()
	e := c.entries[cacheKey{owner, id}]
	if e == nil {
		c.mu.Unlock()
		c.metrics.CacheMisses.Add(1)
		return nil, 0, nil, false
	}
	v := e.Value.(cacheEntry)
	if !time.Now().Before(v.expires) {
		c.remove(e)
		c.mu.Unlock()
		c.metrics.CacheMisses.Add(1)
		return nil, 0, nil, false
	}
	c.lru.MoveToFront(e)
	c.mu.Unlock()
	// Entry bytes are immutable. Decode outside the cache lock so large context
	// reads do not serialize unrelated owners or concurrent writers.
	var turn storedTurn
	if json.Unmarshal(v.data, &turn) != nil {
		c.mu.Lock()
		if c.entries[v.key] == e {
			c.remove(e)
		}
		c.mu.Unlock()
		c.metrics.CacheMisses.Add(1)
		return nil, 0, nil, false
	}
	c.metrics.CacheHits.Add(1)
	c.metrics.CacheReadTokens.Add(uint64(v.tokens))
	c.metrics.CacheReadBytes.Add(uint64(len(v.data)))
	return turn.Items, v.tokens, turn.Continuity, true
}
func (c *TokenCache) Put(owner, id string, items []Item) bool {
	_, ok := c.putTurn(owner, id, items, nil)
	return ok
}
func (c *TokenCache) putTurn(owner, id string, items []Item, cont *continuity) (int64, bool) {
	itemBytes, _ := json.Marshal(items)
	b, e := json.Marshal(storedTurn{items, cont})
	if e != nil || int64(len(b)) > c.maxBytes/4 {
		c.metrics.CacheSkipped.Add(1)
		return 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := cacheKey{owner, id}
	if old := c.entries[key]; old != nil {
		c.remove(old)
	}
	for c.lru.Len() > 0 && (c.bytes+int64(len(b)) > c.maxBytes || c.lru.Len() >= c.maxEntries) {
		c.remove(c.lru.Back())
		c.metrics.CacheEvictions.Add(1)
	}
	tokens := EstimateTokens(itemBytes)
	c.entries[key] = c.lru.PushFront(cacheEntry{key, b, tokens, time.Now().Add(c.ttl)})
	c.bytes += int64(len(b))
	c.metrics.CacheWriteTokens.Add(uint64(tokens))
	c.metrics.CacheWriteBytes.Add(uint64(len(b)))
	c.metrics.CacheWrites.Add(1)
	return tokens, true
}
func (c *TokenCache) Size() (int, int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for e := c.lru.Back(); e != nil; {
		prev := e.Prev()
		if !time.Now().Before(e.Value.(cacheEntry).expires) {
			c.remove(e)
		}
		e = prev
	}
	return c.lru.Len(), c.bytes
}
