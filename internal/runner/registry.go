package runner

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/basant-kumar/rolemux/internal/provider"
	"github.com/basant-kumar/rolemux/internal/task"
)

// Factory constructs one provider adapter and returns its resolved executable
// path. repoRoot is available for adapters that keep repository-private state.
type Factory func(cliPath, repoRoot string) (Adapter, string, error)

// Registry is the only provider-construction boundary used by the CLI. A new
// provider adds an Adapter implementation and registers one factory; workflow,
// catalog, picker, and command code remain provider-agnostic.
type Registry struct {
	factories map[string]Factory
}

func NewRegistry() *Registry { return &Registry{factories: map[string]Factory{}} }

func (r *Registry) Register(name string, factory Factory) error {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" || factory == nil {
		return errors.New("provider registration requires an ID and factory")
	}
	if _, exists := r.factories[name]; exists {
		return fmt.Errorf("provider %q is already registered", name)
	}
	r.factories[name] = factory
	return nil
}

func (r *Registry) Has(name string) bool {
	if r == nil {
		return false
	}
	_, ok := r.factories[strings.ToLower(strings.TrimSpace(name))]
	return ok
}

func (r *Registry) Names() []string {
	if r == nil {
		return nil
	}
	names := make([]string, 0, len(r.factories))
	for name := range r.factories {
		names = append(names, name)
	}
	provider.SortNames(names)
	return names
}

func (r *Registry) Build(name, cliPath, repoRoot string) (Adapter, string, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if r == nil || r.factories[name] == nil {
		return nil, "", fmt.Errorf("no adapter registered for provider %s", name)
	}
	return r.factories[name](cliPath, repoRoot)
}

// BuiltinRegistry contains the adapters shipped in this binary.
func BuiltinRegistry() *Registry {
	r := NewRegistry()
	mustRegister := func(name string, factory Factory) {
		if err := r.Register(name, factory); err != nil {
			panic(err)
		}
	}
	mustRegister(provider.Codex, func(path, _ string) (Adapter, string, error) {
		adapter, err := NewCodex(path)
		if err != nil {
			return nil, "", err
		}
		return adapter, adapter.Path, nil
	})
	mustRegister(provider.Claude, func(path, _ string) (Adapter, string, error) {
		adapter, err := NewClaude(path)
		if err != nil {
			return nil, "", err
		}
		return adapter, adapter.Path, nil
	})
	mustRegister(provider.Copilot, func(path, repoRoot string) (Adapter, string, error) {
		adapter, err := NewCopilot(path)
		if err != nil {
			return nil, "", err
		}
		if repoRoot != "" {
			store := task.NewStore(repoRoot)
			if store.Dir != "" {
				adapter.BaseDirectory = filepath.Join(filepath.Dir(store.Dir), "copilot")
			}
		}
		return adapter, adapter.Path, nil
	})
	mustRegister(provider.Antigravity, func(path, _ string) (Adapter, string, error) {
		adapter, err := NewAntigravity(path)
		if err != nil {
			return nil, "", err
		}
		return adapter, adapter.Path, nil
	})
	return r
}
