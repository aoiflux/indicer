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

func (c *ConcMap) Set(key string, value int64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.data[key] = value
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
