package mirror

import (
	"container/list"
	"sync"

	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/paprikaclient"
)

// DefaultBodyCacheSize is the number of recipe bodies kept.
const DefaultBodyCacheSize = 50

// bodyCache is a most-recently-used cache of full recipes. A cached body is
// only returned when its hash equals the hash the caller expects.
type bodyCache struct {
	mu    sync.Mutex
	max   int
	order *list.List
	items map[string]*list.Element
}

type bodyItem struct {
	uid   string
	entry paprikaclient.Entry[paprikaclient.Recipe]
}

func newBodyCache(max int) *bodyCache {
	if max <= 0 {
		max = DefaultBodyCacheSize
	}
	return &bodyCache{max: max, order: list.New(), items: map[string]*list.Element{}}
}

func (c *bodyCache) get(uid, hash string) (paprikaclient.Entry[paprikaclient.Recipe], bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[uid]
	if !ok {
		return paprikaclient.Entry[paprikaclient.Recipe]{}, false
	}
	item := el.Value.(*bodyItem)
	if item.entry.Value.Hash != hash {
		c.order.Remove(el)
		delete(c.items, uid)
		return paprikaclient.Entry[paprikaclient.Recipe]{}, false
	}
	c.order.MoveToFront(el)
	return item.entry, true
}

func (c *bodyCache) put(entry paprikaclient.Entry[paprikaclient.Recipe]) {
	uid := entry.Value.UID
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[uid]; ok {
		el.Value.(*bodyItem).entry = entry
		c.order.MoveToFront(el)
		return
	}
	c.items[uid] = c.order.PushFront(&bodyItem{uid: uid, entry: entry})
	for c.order.Len() > c.max {
		last := c.order.Back()
		c.order.Remove(last)
		delete(c.items, last.Value.(*bodyItem).uid)
	}
}

func (c *bodyCache) remove(uid string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[uid]; ok {
		c.order.Remove(el)
		delete(c.items, uid)
	}
}

func (c *bodyCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}
