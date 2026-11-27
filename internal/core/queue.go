package core

import (
	"context"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"sync"
)

// Queue bounds unique pending keys; hints coalesce and reactions take priority.
type Queue struct {
	mu         sync.Mutex
	limit      int
	order      []pluginapi.Key
	pending    map[pluginapi.Key]pluginapi.Event
	wake       chan struct{}
	dirty      map[pluginapi.Key]bool
	generation uint64
}

func NewQueue(limit int) *Queue {
	if limit < 1 {
		limit = 1
	}
	return &Queue{limit: limit, pending: map[pluginapi.Key]pluginapi.Event{}, dirty: map[pluginapi.Key]bool{}, wake: make(chan struct{}, 1)}
}
func (q *Queue) Push(e pluginapi.Event) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	k := pluginapi.Key{Channel: e.Channel, RootTS: e.RootTS}
	if e.Kind == "backfill" {
		k = pluginapi.Key{}
	}
	if old, ok := q.pending[k]; ok {
		if old.Kind == "reaction" && e.Kind != "reaction" {
			return true
		}
		if e.ItemTS == "" && old.Kind == "reaction" {
			// Keep the reply that a coalesced reaction event pointed at.
			e.ItemTS = old.ItemTS
		}
	} else {
		ordinary := len(q.pending)
		if _, ok := q.pending[pluginapi.Key{}]; ok {
			ordinary--
		}
		if e.Kind != "backfill" && ordinary >= q.limit {
			q.generation++
			if len(q.dirty) < q.limit {
				q.dirty[k] = true
			}
			// Reserve one extra slot for a repair sweep even under distinct floods.
			if _, ok := q.pending[pluginapi.Key{}]; !ok {
				q.pending[pluginapi.Key{}] = pluginapi.Event{Kind: "backfill"}
				q.order = append(q.order, pluginapi.Key{})
			}
			select {
			case q.wake <- struct{}{}:
			default:
			}
			return false
		}
		q.order = append(q.order, k)
	}
	q.pending[k] = e
	select {
	case q.wake <- struct{}{}:
	default:
	}
	return true
}
func (q *Queue) Len() int { q.mu.Lock(); defer q.mu.Unlock(); return len(q.pending) }
func (q *Queue) Pop(ctx context.Context) (pluginapi.Event, error) {
	for {
		if e := ctx.Err(); e != nil {
			return pluginapi.Event{}, e
		}
		q.mu.Lock()
		if len(q.order) > 0 {
			k := q.order[0]
			q.order = q.order[1:]
			v := q.pending[k]
			delete(q.pending, k)
			q.mu.Unlock()
			return v, nil
		}
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			return pluginapi.Event{}, ctx.Err()
		case <-q.wake:
		}
	}
}

// Repairs returns bounded dropped-key hints plus a generation. Any overflow is
// covered by a full history sweep; concurrent drops keep the dirty state alive.
func (q *Queue) Repairs() ([]pluginapi.Key, uint64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	keys := []pluginapi.Key{}
	for k := range q.dirty {
		keys = append(keys, k)
	}
	return keys, q.generation
}
func (q *Queue) Repaired(generation uint64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.generation == generation {
		q.dirty = map[pluginapi.Key]bool{}
		q.generation = 0
	}
}
