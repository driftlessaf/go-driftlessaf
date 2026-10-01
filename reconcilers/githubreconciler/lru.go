/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package githubreconciler

import (
	"container/list"
	"sync"
)

// lruCache is a size-bounded, least-recently-used map. It is safe for
// concurrent use; every method holds mu only for the map and list update.
type lruCache[K comparable, V any] struct {
	mu    sync.Mutex
	size  int
	order *list.List // front is most recently used; elements hold *lruItem
	items map[K]*list.Element
}

type lruItem[K comparable, V any] struct {
	key   K
	value V
}

// newLRU returns an lruCache holding at most size entries. size must be
// positive.
func newLRU[K comparable, V any](size int) *lruCache[K, V] {
	return &lruCache[K, V]{
		size:  size,
		order: list.New(),
		items: make(map[K]*list.Element, size),
	}
}

// get returns key's value and marks it most recently used.
func (c *lruCache[K, V]) get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		var zero V
		return zero, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*lruItem[K, V]).value, true
}

// add sets key's value, marks it most recently used, and evicts the least
// recently used entry when the cache is over its size.
func (c *lruCache[K, V]) add(key K, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		el.Value.(*lruItem[K, V]).value = value
		c.order.MoveToFront(el)
		return
	}
	c.items[key] = c.order.PushFront(&lruItem[K, V]{key: key, value: value})
	if c.order.Len() > c.size {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.items, oldest.Value.(*lruItem[K, V]).key)
	}
}

// removeIf drops every entry that matches.
func (c *lruCache[K, V]) removeIf(match func(K, V) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, el := range c.items {
		if match(key, el.Value.(*lruItem[K, V]).value) {
			c.order.Remove(el)
			delete(c.items, key)
		}
	}
}

// remove drops key.
func (c *lruCache[K, V]) remove(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.order.Remove(el)
		delete(c.items, key)
	}
}

// purge drops every entry.
func (c *lruCache[K, V]) purge() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.order.Init()
	clear(c.items)
}

// len returns the number of entries held.
func (c *lruCache[K, V]) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}
