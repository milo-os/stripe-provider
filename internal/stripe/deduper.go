// SPDX-License-Identifier: AGPL-3.0-only

package stripe

import (
	"container/list"
	"sync"
)

// MemoryDeduper is an in-memory bounded LRU of Stripe event ids used to
// drop retried webhook deliveries. Stripe retries for ~3 days with
// exponential backoff; a few thousand entries in memory absorb realistic
// duplicate bursts. Persistence across restarts is unnecessary because
// the underlying CR patches are idempotent — deduping just saves wasted
// reconciles.
type MemoryDeduper struct {
	mu       sync.Mutex
	capacity int
	order    *list.List
	index    map[string]*list.Element
}

// NewMemoryDeduper returns a deduper retaining up to capacity event ids.
func NewMemoryDeduper(capacity int) *MemoryDeduper {
	if capacity <= 0 {
		capacity = 4096
	}
	return &MemoryDeduper{
		capacity: capacity,
		order:    list.New(),
		index:    make(map[string]*list.Element, capacity),
	}
}

// SeenOrRecord returns true if id was already recorded; otherwise records it.
func (d *MemoryDeduper) SeenOrRecord(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if elem, ok := d.index[id]; ok {
		d.order.MoveToBack(elem)
		return true
	}
	if d.order.Len() >= d.capacity {
		front := d.order.Front()
		if front != nil {
			delete(d.index, front.Value.(string))
			d.order.Remove(front)
		}
	}
	d.index[id] = d.order.PushBack(id)
	return false
}
