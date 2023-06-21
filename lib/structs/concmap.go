package structs

import (
	"sync"
)

type ConcMap struct {
	mu   sync.Mutex
	data map[string]float32
}

func NewConcMap() *ConcMap {
	return &ConcMap{
		data: make(map[string]float32),
	}
}

func (c *ConcMap) Set(key string, _v float32) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if v, ok := c.data[key]; ok {
		c.data[key] = v + _v
	} else {
		c.data[key] = _v
	}
}

func (c *ConcMap) Get(key string) (float32, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	value, ok := c.data[key]
	return value, ok
}

func (c *ConcMap) GetData() map[string]float32 {
	return c.data
}
