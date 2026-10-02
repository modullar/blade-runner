package supervisor

import "container/list"

// lru is a map that holds at most max entries and, when full, drops the one used longest ago.
// The supervisor's "already recorded" sets must be bounded, and emptying a full set would make
// every entry still in use fire again at once (a refusal per waiting job per poll, in the audit
// log): evicting the oldest costs at most one repeated record, for a job nobody has looked at since.
type lru[K comparable, V any] struct {
	max int
	ll  *list.List // front = most recently used
	idx map[K]*list.Element
}

type lruItem[K comparable, V any] struct {
	k K
	v V
}

func newLRU[K comparable, V any](max int) *lru[K, V] {
	return &lru[K, V]{max: max, ll: list.New(), idx: map[K]*list.Element{}}
}

// Get returns the value for k and marks it as just used.
func (c *lru[K, V]) Get(k K) (V, bool) {
	if e, ok := c.idx[k]; ok {
		c.ll.MoveToFront(e)
		return e.Value.(*lruItem[K, V]).v, true
	}
	var zero V
	return zero, false
}

// Put stores v under k, evicting the least recently used entries beyond the bound.
func (c *lru[K, V]) Put(k K, v V) {
	if e, ok := c.idx[k]; ok {
		e.Value.(*lruItem[K, V]).v = v
		c.ll.MoveToFront(e)
		return
	}
	c.idx[k] = c.ll.PushFront(&lruItem[K, V]{k, v})
	c.trim()
}

func (c *lru[K, V]) Delete(k K) {
	if e, ok := c.idx[k]; ok {
		c.ll.Remove(e)
		delete(c.idx, k)
	}
}

func (c *lru[K, V]) Len() int { return c.ll.Len() }

// SetMax changes the bound (never below 1) and evicts down to it.
func (c *lru[K, V]) SetMax(n int) {
	c.max = max(n, 1)
	c.trim()
}

// Keys returns the keys, most recently used first.
func (c *lru[K, V]) Keys() []K {
	out := make([]K, 0, c.ll.Len())
	for e := c.ll.Front(); e != nil; e = e.Next() {
		out = append(out, e.Value.(*lruItem[K, V]).k)
	}
	return out
}

func (c *lru[K, V]) trim() {
	for c.ll.Len() > c.max {
		old := c.ll.Back()
		c.ll.Remove(old)
		delete(c.idx, old.Value.(*lruItem[K, V]).k)
	}
}
