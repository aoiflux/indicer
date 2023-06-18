package structs

import "sync"

type RimMap struct {
	mu   sync.Mutex
	data map[int64][]string
}

func NewRimMap() *RimMap {
	return &RimMap{
		data: make(map[int64][]string),
	}
}

func (c *RimMap) Set(key int64, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if v, ok := c.data[key]; ok {
		v = append(v, value)
		c.data[key] = v
	} else {
		c.data[key] = []string{value}
	}
}

func (c *RimMap) Get(key int64) ([]string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	value, ok := c.data[key]
	return value, ok
}
