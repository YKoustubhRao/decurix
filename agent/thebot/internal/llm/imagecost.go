package llm

// imagecost.go: what a picture really costs on a route, learned from the provider's own usage.
//
// The context budget prices a screenshot at a flat figure (skills/context.go pictureChars), and a flat figure is wrong
// both ways: a screenshot costs about a thousand tokens on one hosted model and four thousand or more on a local
// model's vision encoder. The provider prices every picture exactly on the call that carries it, so the cost can be
// read off: with the previous call of the same conversation as the anchor (its real prompt count, and the messages it
// priced), the next call's real prompt count, less the anchor's, less the text added since (at the rate the anchor's
// own text cost), is what the pictures added since cost. Each reading is kept per route (RouteKey) and smoothed
// (an exponential average, weight 0.5); a reading outside a plausible band, or one whose anchor no longer matches (the
// transcript was rewritten, a compaction), teaches nothing. A route that reports only the part of the prompt it did not
// have cached (a local server reusing its prefix) gives readings below the anchor, which are dropped the same way.
// After Hermes agent/image_token_cost.py and agent/usage_anchor.py.

import (
	"hash/fnv"
	"sync"
)

const (
	// defaultImageTokens is a picture's price before any reading: the rate the anchor's text is measured at needs one.
	defaultImageTokens = 1500
	// A reading outside this band is noise in the text estimate, not a picture's price.
	minImageTokens = 64
	maxImageTokens = 32_768
	// imageCostWeight is the weight of a new reading in the average.
	imageCostWeight = 0.5
	// minAnchorTextBytes is the least text an anchor must hold for its rate to be worth using.
	minAnchorTextBytes = 1000
)

// usageAnchor is a call's real prompt count and the messages it priced.
type usageAnchor struct {
	hashes    []uint64 // one per message priced, in order
	prompt    int      // the real prompt tokens
	textBytes int      // the text of the messages priced
	images    int      // the pictures among them
}

// messageHash identifies a message as the provider saw it: role, text, labels and each picture's bytes.
func messageHash(m Message) uint64 {
	h := fnv.New64a()
	h.Write([]byte(m.Role))
	h.Write([]byte{0})
	h.Write([]byte(m.Content))
	for i, img := range m.Images {
		h.Write([]byte{0})
		h.Write([]byte(m.ImageLabel(i)))
		h.Write([]byte{0})
		h.Write(img)
	}
	return h.Sum64()
}

// anchorFor is the anchor of a call that sent messages and was priced at prompt tokens.
func anchorFor(messages []Message, prompt int) *usageAnchor {
	a := &usageAnchor{hashes: make([]uint64, len(messages)), prompt: prompt}
	for i, m := range messages {
		a.hashes[i] = messageHash(m)
		a.textBytes += len(m.Content)
		a.images += len(m.Images)
	}
	return a
}

// delta is the messages sent after the anchor's, when messages (whose hashes are given) still begin with exactly the
// ones it priced.
func (a *usageAnchor) delta(messages []Message, hashes []uint64) ([]Message, bool) {
	if a == nil || len(hashes) != len(messages) || len(messages) < len(a.hashes) {
		return nil, false
	}
	for i, h := range a.hashes {
		if hashes[i] != h {
			return nil, false
		}
	}
	return messages[len(a.hashes):], true
}

// imageCostTable is the learned price of a picture on each route.
type imageCostTable struct {
	mu      sync.Mutex
	byRoute map[RouteKey]int
	// active is the route a price was last learned on: soul runs one brain, so it is the brain's route.
	active    RouteKey
	hasActive bool
}

// imageCosts is this process's table.
var imageCosts = &imageCostTable{}

// ImageTokens is the learned price of one picture on route, and whether there is one.
func ImageTokens(route RouteKey) (int, bool) {
	imageCosts.mu.Lock()
	defer imageCosts.mu.Unlock()
	n, ok := imageCosts.byRoute[route]
	return n, ok
}

// ActiveImageTokens is the learned price of one picture on the route a price was last learned on - the brain's, in a
// soul that runs one - and whether there is one.
func ActiveImageTokens() (int, bool) {
	imageCosts.mu.Lock()
	defer imageCosts.mu.Unlock()
	if !imageCosts.hasActive {
		return 0, false
	}
	n, ok := imageCosts.byRoute[imageCosts.active]
	return n, ok
}

// learn takes one call's reading - messages, priced at prompt tokens, now is their anchor - against the conversation's
// previous anchor (see the file comment) and returns the route's new price, or 0 when the call taught nothing.
func (t *imageCostTable) learn(route RouteKey, anchor *usageAnchor, messages []Message, now *usageAnchor) int {
	if anchor == nil || now == nil {
		return 0
	}
	prompt := now.prompt
	delta, ok := anchor.delta(messages, now.hashes)
	if !ok || prompt <= anchor.prompt || anchor.textBytes < minAnchorTextBytes {
		return 0
	}
	pictures, textBytes := 0, 0
	for _, m := range delta {
		pictures += len(m.Images)
		textBytes += len(m.Content)
	}
	if pictures == 0 {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	prior, known := t.byRoute[route]
	price := prior
	if !known {
		price = defaultImageTokens
	}
	rate := float64(anchor.prompt-anchor.images*price) / float64(anchor.textBytes)
	if rate <= 0.05 || rate > 2 {
		return 0 // the anchor's own figures do not hold together: no reading
	}
	reading := (float64(prompt-anchor.prompt) - rate*float64(textBytes)) / float64(pictures)
	if reading < minImageTokens || reading > maxImageTokens {
		return 0
	}
	learned := int(reading + 0.5)
	if known {
		learned = int(float64(prior) + imageCostWeight*(reading-float64(prior)) + 0.5)
	}
	if t.byRoute == nil || len(t.byRoute) >= maxRouteFacts {
		t.byRoute = map[RouteKey]int{}
	}
	t.byRoute[route] = learned
	t.active, t.hasActive = route, true
	return learned
}

// forget drops every price (for tests).
func (t *imageCostTable) forget() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.byRoute, t.hasActive = nil, false
}
