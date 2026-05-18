package provider

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// detectTimeout bounds a single provider's Detect() call so one misbehaving
// agent binary (e.g., a hanging `--version`) can't stall DetectAll.
const detectTimeout = 5 * time.Second

// Registry holds all known providers — built-ins registered at startup plus any
// declarative YAML providers loaded from ~/.uta/providers/.
type Registry struct {
	mu        sync.RWMutex
	providers map[string]AgentProvider
}

func NewRegistry() *Registry {
	return &Registry{providers: map[string]AgentProvider{}}
}

// Register adds a provider. Later registrations with the same name overwrite
// only if force is true — built-ins win by default so a stray YAML can't
// shadow them silently.
func (r *Registry) Register(p AgentProvider, force bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.providers[p.Name()]; exists && !force {
		return fmt.Errorf("provider %q already registered", p.Name())
	}
	r.providers[p.Name()] = p
	return nil
}

func (r *Registry) Get(name string) (AgentProvider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[name]
	return p, ok
}

// Names returns all registered provider names sorted alphabetically.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.providers))
	for n := range r.providers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// DetectAll runs Detect() on every registered provider, in parallel. Results
// come back keyed by provider name.
func (r *Registry) DetectAll(ctx context.Context) map[string]Detection {
	r.mu.RLock()
	providers := make([]AgentProvider, 0, len(r.providers))
	for _, p := range r.providers {
		providers = append(providers, p)
	}
	r.mu.RUnlock()

	out := make(map[string]Detection, len(providers))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, p := range providers {
		wg.Add(1)
		go func(p AgentProvider) {
			defer wg.Done()
			dctx, cancel := context.WithTimeout(ctx, detectTimeout)
			defer cancel()
			d := p.Detect(dctx)
			mu.Lock()
			out[p.Name()] = d
			mu.Unlock()
		}(p)
	}
	wg.Wait()
	return out
}
