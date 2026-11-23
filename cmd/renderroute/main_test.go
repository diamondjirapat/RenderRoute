package main

import (
	"strings"
	"testing"

	"github.com/diamondjirapat/RenderRoute/internal/config"
	"github.com/diamondjirapat/RenderRoute/internal/gpu"
)

func TestResolveWithConfigDoesNotSilentlyFallbackOnExplicitDGPU(t *testing.T) {
	onlyIGPU := []gpu.GPU{
		{
			Index:     0,
			ID:        "0000:00:02.0",
			Card:      "card1",
			PCISlot:   "0000:00:02.0",
			Vendor:    "intel",
			Driver:    "i915",
			Name:      "Intel UHD",
			ShortName: "UHD",
			Role:      gpu.RoleIntegrated,
		},
	}
	warnings := []gpu.GPUWarning{
		{
			PCISlot: "0000:01:00.0",
			Vendor:  "nvidia",
			Driver:  "nvidia",
			Name:    "NVIDIA GeForce RTX 4050",
			Role:    gpu.RoleDiscrete,
			Reason:  `kernel driver "nvidia" is bound on PCI but created no DRM node in /sys/class/drm`,
		},
	}
	cfg := &config.Config{DefaultGPU: "dgpu"}

	// Explicit "dgpu" must return an error with the unbound PCI GPU diagnostic — NOT silently fall back to iGPU.
	if g, err := resolveWithConfigAndWarnings(onlyIGPU, cfg, "dgpu", warnings); err == nil {
		t.Fatalf("expected error for explicit dgpu when none is available, got GPU %+v", g)
	} else if !strings.Contains(err.Error(), "0000:01:00.0") {
		t.Errorf("expected diagnostic PCI slot in error, got: %v", err)
	}

	// Implicit "default" / "" should still gracefully fall back on single-GPU systems.
	for _, sel := range []string{"", "default"} {
		g, err := resolveWithConfigAndWarnings(onlyIGPU, cfg, sel, warnings)
		if err != nil {
			t.Fatalf("expected graceful fallback for %q, got error: %v", sel, err)
		}
		if g.ID != "0000:00:02.0" {
			t.Errorf("expected fallback to iGPU for %q, got %s", sel, g.ID)
		}
	}
}
