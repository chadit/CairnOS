package runner

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
)

// Factory constructs a configured runner from its config block (the runner's
// section of the CairnOS config, decoded as a map).
type Factory func(cfg map[string]any) (Runner, error)

// Registry maps names to runner factories and their built instances. A runner
// is constructed once per process from its config block and reused across
// runs, which is why Execute must be safe for concurrent use.
type Registry struct {
	factories map[string]Factory
	instances map[string]Runner
	mu        sync.RWMutex
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		factories: map[string]Factory{},
		instances: map[string]Runner{},
	}
}

// Register adds a factory under a name. Registration happens once at startup
// from explicit wiring, never from init side effects, so the build graph
// stays readable.
func (r *Registry) Register(name string, factory Factory) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.factories[name]; exists {
		return fmt.Errorf("%w: %q", ErrDuplicateRunner, name)
	}

	r.factories[name] = factory

	return nil
}

// Build constructs the named runner from its config block and caches the
// instance for Get.
func (r *Registry) Build(name string, cfg map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	factory, exists := r.factories[name]
	if !exists {
		return fmt.Errorf("%w: %q", ErrUnknownRunner, name)
	}

	instance, err := factory(cfg)
	if err != nil {
		return fmt.Errorf("build runner %q: %w", name, err)
	}

	r.instances[name] = instance

	return nil
}

// BuildAll constructs every registered runner, passing each its config block
// (or nil when the config has none).
func (r *Registry) BuildAll(cfgs map[string]map[string]any) error {
	names := r.registeredNames()

	for _, name := range names {
		if err := r.Build(name, cfgs[name]); err != nil {
			return err
		}
	}

	return nil
}

// Get returns the built instance for a name. The false return is the
// runner-not-found condition checked at run start
// (.agents/spec/02-workflow-schema.md, validation rule 7).
func (r *Registry) Get(name string) (Runner, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	instance, exists := r.instances[name]

	return instance, exists
}

// List returns the built runner names, sorted for stable output.
func (r *Registry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return slices.Sorted(maps.Keys(r.instances))
}

// Close shuts down every built instance that holds resources. Called once at
// engine stop.
func (r *Registry) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	var errs []error

	for name, instance := range r.instances {
		closer, ok := instance.(Closer)
		if !ok {
			continue
		}

		if err := closer.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close runner %q: %w", name, err))
		}
	}

	if len(errs) == 0 {
		return nil
	}

	return fmt.Errorf("close registry: %w", errors.Join(errs...))
}

func (r *Registry) registeredNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return slices.Sorted(maps.Keys(r.factories))
}
