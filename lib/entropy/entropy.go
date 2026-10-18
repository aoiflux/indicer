// Package entropy computes the Shannon entropy of byte streams, in bits per
// byte, from 0 for constant data to 8 for uniformly random data.
package entropy

import "math"

// Counter counts byte values as data is added. The zero value is ready.
type Counter struct {
	counts [256]uint64
	total  uint64
}

// Add counts the bytes of p.
func (c *Counter) Add(p []byte) {
	for _, b := range p {
		c.counts[b]++
	}
	c.total += uint64(len(p))
}

// Total is how many bytes were counted.
func (c *Counter) Total() uint64 { return c.total }

// Value is the entropy of what was counted, 0 for no bytes.
func (c *Counter) Value() float64 {
	if c.total == 0 {
		return 0
	}
	inv := 1 / float64(c.total)
	var h float64
	for _, n := range c.counts {
		if n == 0 {
			continue
		}
		p := float64(n) * inv
		h -= p * math.Log2(p)
	}
	return h
}

// Of is the entropy of p.
func Of(p []byte) float64 {
	var c Counter
	c.Add(p)
	return c.Value()
}
