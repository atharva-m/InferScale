package deployment

import (
	"fmt"
	"strings"
)

func (r *Reconciler) nodeSelectorFor(acceleratorType string) (map[string]string, error) {
	// An empty inventory is reserved for the explicit local fake-runtime path.
	// Once any real GPU inventory is configured, every requested logical SKU
	// must resolve exactly instead of falling through to the cluster scheduler.
	if len(r.Config.GPUNodeSelectors) == 0 {
		return nil, nil
	}
	acceleratorType = strings.TrimSpace(acceleratorType)
	selector, found := r.Config.GPUNodeSelectors[acceleratorType]
	if !found || len(selector) == 0 {
		return nil, fmt.Errorf("accelerator SKU %q has no verified node-selector inventory", acceleratorType)
	}
	return selector, nil
}
