package core

import (
	"context"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"sync"
)

type pendingKey struct {
	pluginapi.Key
	ItemTS string
}

// Queue bounds unique pending candidates; hints coalesce and reactions take priority.
type Queue struct {
	mu         sync.Mutex
	limit      int
	order      []pendingKey
	pending    map[pendingKey]pluginapi.Event
	wake       chan struct{}
	dirty      map[pendingKey]pluginapi.Event
	generation uint64
}

func NewQueue(limit int) *Queue {
	if limit < 1 {
		limit = 1
	}
	return &Queue{limit: limit, pending: map[pendingKey]pluginapi.Event{}, dirty: map[pendingKey]pluginapi.Event{}, wake: make(chan struct{}, 1)}
}
func (q *Queue) Push(e pluginapi.Event) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	k := pendingKey{Key: pluginapi.Key{Channel: e.Channel, RootTS: e.RootTS}, ItemTS: e.ItemTS}
	if e.Kind == "backfill" {
		k = pendingKey{}
	}
	if old, ok := q.pending[k]; ok {
		if old.Kind == "reaction" && e.Kind != "reaction" {
			return true
		}
	} else {
		ordinary := len(q.pending)
		if _, ok := q.pending[pendingKey{}]; ok {
			ordinary--
		}
		if e.Kind != "backfill" && ordinary >= q.limit {
			q.generation++
			if len(q.dirty) < q.limit {
				q.dirty[k] = e
			}
			// Reserve one extra slot for a repair sweep even under distinct floods.
			if _, ok := q.pending[pendingKey{}]; !ok {
				q.pending[pendingKey{}] = pluginapi.Event{Kind: "backfill"}
				q.order = append(q.order, pendingKey{})
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
// covered by a full history sweep and bounded reply hints. Beyond the hint bound,
// reply-only triggers require another live event; root history cannot recover them.
// Concurrent drops keep the dirty state alive.
func (q *Queue) Repairs() ([]pluginapi.Event, uint64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	keys := []pluginapi.Event{}
	for _, event := range q.dirty {
		keys = append(keys, event)
	}
	return keys, q.generation
}
func (q *Queue) Repaired(generation uint64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.generation == generation {
		q.dirty = map[pendingKey]pluginapi.Event{}
		q.generation = 0
	}
}
