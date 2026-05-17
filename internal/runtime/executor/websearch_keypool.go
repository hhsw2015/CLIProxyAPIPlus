package executor

import (
	"sync"
	"sync/atomic"
	"time"
)

type wsKeyPool struct {
	keys     []string
	cursor   atomic.Uint64
	cooldown sync.Map // key -> time.Time
}

func newWSKeyPool(keys []string) *wsKeyPool {
	return &wsKeyPool{keys: keys}
}

func (p *wsKeyPool) Next() string {
	if len(p.keys) == 0 {
		return ""
	}
	now := time.Now()
	n := uint64(len(p.keys))
	start := p.cursor.Add(1) - 1
	for i := uint64(0); i < n; i++ {
		idx := (start + i) % n
		key := p.keys[idx]
		if v, ok := p.cooldown.Load(key); ok {
			if until, ok := v.(time.Time); ok && now.Before(until) {
				continue
			}
			p.cooldown.Delete(key)
		}
		return key
	}
	return p.keys[start%n]
}

func (p *wsKeyPool) MarkRateLimited(key string) {
	p.cooldown.Store(key, time.Now().Add(60*time.Second))
}
