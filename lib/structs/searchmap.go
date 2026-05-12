package structs

import "sync"

type SeenChonkMap struct {
	mu   sync.RWMutex
	data map[string]int
}

func NewSeenChonkMap() *SeenChonkMap { return &SeenChonkMap{data: make(map[string]int)} }
func (s *SeenChonkMap) Set(key []byte, val int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[string(key)] = val
}
func (s *SeenChonkMap) Get(key []byte) (int, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	val, ok := s.data[string(key)]
	return val, ok
}

func (s *SeenChonkMap) GetOrCompute(key []byte, compute func() int) int {
	keyStr := string(key)

	s.mu.RLock()
	val, ok := s.data[keyStr]
	s.mu.RUnlock()
	if ok {
		return val
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	val, ok = s.data[keyStr]
	if ok {
		return val
	}

	val = compute()
	s.data[keyStr] = val
	return val
}

type SearchIDMap struct {
	mu   sync.Mutex
	data map[string]int
}

func NewSearchIDMap() *SearchIDMap {
	return &SearchIDMap{data: make(map[string]int)}
}
func (s *SearchIDMap) Set(key string, count int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, ok := s.data[key]
	if ok {
		s.data[key] = existing + count
		return
	}
	s.data[key] = count
}
func (s *SearchIDMap) GetData() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int, len(s.data))
	for k, v := range s.data {
		out[k] = v
	}
	return out
}
