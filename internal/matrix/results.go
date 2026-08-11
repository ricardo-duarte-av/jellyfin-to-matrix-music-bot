package matrix

import (
	"fmt"
	"sync"
	"time"

	"github.com/daedric/jellyfin-to-matrix-music-bot/internal/jellyfin"
)

// results holds the last search result set so that "!play 3" has something to
// resolve against. Results expire so a stale index cannot silently play the
// wrong thing much later.
type results struct {
	ttl time.Duration

	mu      sync.Mutex
	items   []jellyfin.Item
	written time.Time
}

func newResults(ttl time.Duration) *results {
	return &results{ttl: ttl}
}

// set replaces the current result set.
func (r *results) set(items []jellyfin.Item) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items = items
	r.written = time.Now()
}

// get returns the current result set, or nil if there is none or it expired.
func (r *results) get() []jellyfin.Item {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.items) == 0 || time.Since(r.written) > r.ttl {
		return nil
	}
	out := make([]jellyfin.Item, len(r.items))
	copy(out, r.items)
	return out
}

// resolve maps a 1-based index from the last search to its item.
func (r *results) resolve(index int) (jellyfin.Item, error) {
	items := r.get()
	if len(items) == 0 {
		return jellyfin.Item{}, fmt.Errorf("no recent search results — run a search first")
	}
	if index < 1 || index > len(items) {
		return jellyfin.Item{}, fmt.Errorf("index %d is out of range (1-%d)", index, len(items))
	}
	return items[index-1], nil
}
