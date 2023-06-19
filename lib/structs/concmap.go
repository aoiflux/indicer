package structs

import (
	"sync"
)

type ConcMap struct {
	mu   sync.Mutex
	data map[string]int64
}

func NewConcMap() *ConcMap {
	return &ConcMap{
		data: make(map[string]int64),
	}
}

func (c *ConcMap) Set(key string, _v int64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if v, ok := c.data[key]; ok {
		c.data[key] = v + _v
	} else {
		c.data[key] = _v
	}
}

func (c *ConcMap) Get(key string) (int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	value, ok := c.data[key]
	return value, ok
}

func (c *ConcMap) GetData() map[string]int64 {
	return c.data
}
