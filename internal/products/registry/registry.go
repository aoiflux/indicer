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

func (r *Registry) RegisterAs(product string, s Service) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.services[product] = s
}

func (r *Registry) Get(product string) (Service, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.services[product]
	return s, ok
}

// Stopper is a service with background work to stop.
type Stopper interface {
	// Shutdown cancels the service's background tasks, refuses new ones,
	// and waits for them until ctx ends. It returns how many it cancelled
	// and how many were still running when it returned.
	Shutdown(ctx context.Context) (canceled, running int)
}

// Services returns every registered service once, however many names it is
// registered under.
func (r *Registry) Services() []Service {
	r.mu.RLock()
	defer r.mu.RUnlock()
	seen := make(map[Service]bool, len(r.services))
	out := make([]Service, 0, len(r.services))
	for _, s := range r.services {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
