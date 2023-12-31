package structs

import "sync"

type SeenChonkMap struct {
	mu   sync.Mutex
	data map[string]struct{}
}

func NewSeenChonkMap() *SeenChonkMap { return &SeenChonkMap{data: make(map[string]struct{})} }
func (s *SeenChonkMap) Set(key []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[string(key)] = struct{}{}
}
func (s *SeenChonkMap) GetOk(key []byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.data[string(key)]
	return ok
}

type SearchIDMap struct {
	mu   sync.Mutex
	data map[string]int
}

func NewSearchIDMap() *SearchIDMap {
	return &SearchIDMap{data: make(map[string]int)}
}
func (s *SearchIDMap) Set(key string, _val int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	val, ok := s.data[key]
	if ok {
		s.data[key] = val + _val
		return
	}
	s.data[key] = _val
}
func (s *SearchIDMap) GetData() map[string]int { return s.data }
