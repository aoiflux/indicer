package registry

import (
	"context"
	"encoding/json"
	"sync"

	coreerr "indicer/internal/core/errors"
)

type Service interface {
	Product() string
	Handle(ctx context.Context, operation string, params json.RawMessage) (any, *coreerr.Error)
}

type Registry struct {
	mu       sync.RWMutex
	services map[string]Service
}

func New() *Registry {
	return &Registry{services: make(map[string]Service)}
}

func (r *Registry) Register(s Service) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.services[s.Product()] = s
}

func (r *Registry) Get(product string) (Service, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.services[product]
	return s, ok
}
