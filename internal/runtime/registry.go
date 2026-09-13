package runtime

import "fmt"

type Registry struct {
	adapters map[Backend]Adapter
}

func NewRegistry(adapters ...Adapter) (*Registry, error) {
	registry := &Registry{adapters: make(map[Backend]Adapter, len(adapters))}
	for _, adapter := range adapters {
		if adapter == nil {
			return nil, fmt.Errorf("nil runtime adapter")
		}
		name := adapter.Name()
		if name == "" || name == BackendAuto {
			return nil, fmt.Errorf("invalid adapter backend %q", name)
		}
		if _, exists := registry.adapters[name]; exists {
			return nil, fmt.Errorf("duplicate runtime adapter %q", name)
		}
		registry.adapters[name] = adapter
	}
	return registry, nil
}

func (r *Registry) Get(backend Backend) (Adapter, error) {
	if r == nil {
		return nil, fmt.Errorf("runtime registry is nil")
	}
	adapter, ok := r.adapters[backend]
	if !ok {
		return nil, fmt.Errorf("runtime backend %q is not registered", backend)
	}
	return adapter, nil
}
