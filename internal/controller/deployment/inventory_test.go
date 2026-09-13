package deployment

import "testing"

func TestNodeSelectorInventoryFailsClosedWhenConfigured(t *testing.T) {
	t.Parallel()
	reconciler := Reconciler{Config: Config{GPUNodeSelectors: map[string]map[string]string{
		"RTX_5090": {"nvidia.com/gpu.product": "NVIDIA-GeForce-RTX-5090"},
	}}}
	if _, err := reconciler.nodeSelectorFor("L40S"); err == nil {
		t.Fatal("unknown GPU SKU must not be scheduled without an inventory selector")
	}
	selector, err := reconciler.nodeSelectorFor("RTX_5090")
	if err != nil {
		t.Fatal(err)
	}
	if selector["nvidia.com/gpu.product"] != "NVIDIA-GeForce-RTX-5090" {
		t.Fatalf("selector=%#v", selector)
	}
}

func TestNodeSelectorInventoryAllowsExplicitLocalFakeMode(t *testing.T) {
	t.Parallel()
	selector, err := (&Reconciler{}).nodeSelectorFor("local-fake")
	if err != nil || selector != nil {
		t.Fatalf("selector=%#v err=%v", selector, err)
	}
}
