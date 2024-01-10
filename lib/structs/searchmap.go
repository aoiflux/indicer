package structs

import "sync"

type SeenChonkMap struct {
	mu   sync.Mutex
	data map[string]SearchResult
}

func NewSeenChonkMap() *SeenChonkMap { return &SeenChonkMap{data: make(map[string]SearchResult)} }
func (s *SeenChonkMap) Set(key []byte, val SearchResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[string(key)] = val
}
func (s *SeenChonkMap) Get(key []byte) (SearchResult, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	val, ok := s.data[string(key)]
	return val, ok
}

type SearchIDMap struct {
	mu   sync.Mutex
	data map[string]SearchResult
}

type SearchResult struct {
	Count   int
	Matches map[int64]string
}

func NewSearchIDMap() *SearchIDMap {
	return &SearchIDMap{data: make(map[string]SearchResult)}
}
func (s *SearchIDMap) Set(key string, _val SearchResult) {
	s.mu.Lock()
	defer s.mu.Unlock()

	val, ok := s.data[key]

	if ok {
		for mkey, mval := range _val.Matches {
			if _, ok := val.Matches[mkey]; ok {
				continue
			}

			val.Matches[mkey] = mval
			val.Count++
		}

		s.data[key] = val
		return
	}

	s.data[key] = _val
}
func (s *SearchIDMap) GetData() map[string]SearchResult { return s.data }
