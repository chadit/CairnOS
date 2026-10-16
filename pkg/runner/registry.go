package runner

import (
	"errors"
	"fmt"
	"sync"

	"github.com/chadit/CairnOS/internal/registry"
)

// Factory constructs a configured runner from its config block (the runner's
// section of the CairnOS config, decoded as a map).
type Factory func(cfg map[string]any) (Runner, error)

// Registry maps names to runner factories and their built instances. A runner
// is constructed once per process from its config block and reused across
// runs, which is why Execute must be safe for concurrent use.
type Registry struct {
	// Table holds the built instances. Its Get is the runner-not-found check at
	// run start (.agents/spec/02-workflow-schema.md, validation rule 7), and its
	// List gives the built names sorted for stable output.
	registry.Table[Runner]

	factories registry.Table[Factory]

	// mu makes Register's duplicate check and insert one step, and serializes
	// Build and Close, so no instance is built twice at once or built while
	// Close runs, either of which would leave one unclosed.
	mu sync.Mutex
}

// NewRegistry returns an empty registry. The zero value is empty and ready to
// use too.
func NewRegistry() *Registry {
	return &Registry{}
}

// Register adds a factory under a name. Registration happens once at startup
// from explicit wiring, never from init side effects, so the build graph
// stays readable.
func (r *Registry) Register(name string, factory Factory) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.factories.Get(name); exists {
		return fmt.Errorf("%w: %q", ErrDuplicateRunner, name)
	}

	r.factories.Add(name, factory)

	return nil
}

// Registered reports whether a factory is registered under name. It reads the
// factories, not the built instances Get reads, so unlike Get it answers true
// before BuildAll, and wiring can check a config block's name at startup.
func (r *Registry) Registered(name string) bool {
	_, exists := r.factories.Get(name)

	return exists
}

// Build constructs the named runner from its config block and caches the
// instance for Get, replacing any instance an earlier Build made.
func (r *Registry) Build(name string, cfg map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	factory, exists := r.factories.Get(name)
	if !exists {
		return fmt.Errorf("%w: %q", ErrUnknownRunner, name)
	}

	instance, err := factory(cfg)
	if err != nil {
		return fmt.Errorf("build runner %q: %w", name, err)
	}

	r.Add(name, instance)

	return nil
}

// BuildAll constructs every registered runner, passing each its config block
// (or nil when the config has none).
func (r *Registry) BuildAll(cfgs map[string]map[string]any) error {
	for _, name := range r.factories.List() {
		if err := r.Build(name, cfgs[name]); err != nil {
			return err
		}
	}

	return nil
}

// Close shuts down every built instance that holds resources. Called once at
// engine stop.
func (r *Registry) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	var errs []error

	for name, instance := range r.Snapshot() {
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
