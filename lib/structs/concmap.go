package structs

import (
	"sync"
)

type ConcMap struct {
	mu   sync.Mutex
	data map[string]float64
}

func NewConcMap() *ConcMap {
	return &ConcMap{
		data: make(map[string]float64),
	}
}

func (c *ConcMap) Set(key string, _v float64, replace ...bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	v, ok := c.data[key]

	if ok {
		if len(replace) > 0 {
			if replace[0] {
				c.data[key] = _v
				return
			}
		}

		c.data[key] = v + _v
		return
	}
	c.data[key] = _v
}

func (c *ConcMap) Get(key string) (float64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	value, ok := c.data[key]
	return value, ok
}

func (c *ConcMap) GetData() map[string]float64 {
	return c.data
}
