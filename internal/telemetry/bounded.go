package telemetry

import "sync"

// Other is the value an out-of-bound label collapses to.
const Other = "other"

// DefaultBoundedMax is how many distinct first-seen values a Bounded without
// an allow-list admits before collapsing the rest to Other.
const DefaultBoundedMax = 64

// Bounded caps a label's cardinality (design §6, Q6): with an allow-list,
// values outside it become Other; without one, the first max distinct values
// pass through and later ones become Other.
type Bounded struct {
	allow map[string]bool // nil: first-seen mode

	mu   sync.Mutex
	seen map[string]bool
	max  int
}

// NewBounded returns a Bounded over allow (empty means first-seen up to max).
func NewBounded(allow []string, max int) *Bounded {
	b := &Bounded{seen: map[string]bool{}, max: max}
	if len(allow) > 0 {
		b.allow = map[string]bool{}
		for _, v := range allow {
			b.allow[v] = true
		}
	}
	return b
}

// Value returns v, or Other when v is out of bounds. A nil Bounded passes
// every value through.
func (b *Bounded) Value(v string) string {
	if b == nil {
		return v
	}
	if b.allow != nil {
		if b.allow[v] {
			return v
		}
		return Other
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.seen[v] {
		return v
	}
	if len(b.seen) >= b.max {
		return Other
	}
	b.seen[v] = true
	return v
}
