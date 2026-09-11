package model

import "container/list"

// rowCache is a small LRU over parsed rows, keyed by data-record index. It keeps
// scrolling around a multi-GB file cheap: only the visible window plus recent
// history stays parsed in memory.
type rowCache struct {
	cap   int
	ll    *list.List
	items map[int]*list.Element
}

type cacheEntry struct {
	key int
	rec []string
}

func newRowCache(capacity int) *rowCache {
	if capacity < 1 {
		capacity = 1
	}
	return &rowCache{
		cap:   capacity,
		ll:    list.New(),
		items: make(map[int]*list.Element, capacity),
	}
}

func (c *rowCache) get(key int) ([]string, bool) {
	if el, ok := c.items[key]; ok {
		c.ll.MoveToFront(el)
		return el.Value.(*cacheEntry).rec, true
	}
	return nil, false
}

func (c *rowCache) put(key int, rec []string) {
	if el, ok := c.items[key]; ok {
		el.Value.(*cacheEntry).rec = rec
		c.ll.MoveToFront(el)
		return
	}
	el := c.ll.PushFront(&cacheEntry{key: key, rec: rec})
	c.items[key] = el
	if c.ll.Len() > c.cap {
		oldest := c.ll.Back()
		if oldest != nil {
			c.ll.Remove(oldest)
			delete(c.items, oldest.Value.(*cacheEntry).key)
		}
	}
}
