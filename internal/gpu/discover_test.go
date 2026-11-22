package gpu

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizePCISlot(t *testing.T) {
	cases := map[string]string{
		"0000:01:00.0":     "0000:01:00.0",
		"00000000:01:00.0": "0000:01:00.0",
		"01:00.0":          "0000:01:00.0",
		"0000:00:02.0":     "0000:00:02.0",
		"00:02.0":          "0000:00:02.0",
		"0000:0A:00.0":     "0000:0a:00.0",
	}
	for in, want := range cases {
		if got := NormalizePCISlot(in); got != want {
			t.Errorf("NormalizePCISlot(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDiscoverFromSyntheticSysfsAndResolve(t *testing.T) {
	root := t.TempDir()
	sysDRM := filepath.Join(root, "sys_drm")
	devDRI := filepath.Join(root, "dev_dri")
	byPath := filepath.Join(devDRI, "by-path")
	if err := os.MkdirAll(byPath, 0o755); err != nil {
		t.Fatal(err)
	}

	// Create card0 = NVIDIA RTX 4050 discrete (suspended)
	c0Dev := filepath.Join(sysDRM, "card0", "device")
	_ = os.MkdirAll(filepath.Join(c0Dev, "power"), 0o755)
	_ = os.WriteFile(filepath.Join(c0Dev, "vendor"), []byte("0x10de\n"), 0o644)
	_ = os.WriteFile(filepath.Join(c0Dev, "device"), []byte("0x28e1\n"), 0o644)
	_ = os.WriteFile(filepath.Join(c0Dev, "boot_vga"), []byte("0\n"), 0o644)
	_ = os.WriteFile(filepath.Join(c0Dev, "power", "runtime_status"), []byte("suspended\n"), 0o644)
	_ = os.WriteFile(filepath.Join(c0Dev, "uevent"), []byte("DRIVER=nvidia\nPCI_SLOT_NAME=0000:01:00.0\n"), 0o644)
	_ = os.Symlink("../renderD129", filepath.Join(byPath, "pci-0000:01:00.0-render"))

	// Create card1 = Intel iGPU (active, boot_vga=1)
	c1Dev := filepath.Join(sysDRM, "card1", "device")
	_ = os.MkdirAll(filepath.Join(c1Dev, "power"), 0o755)
	_ = os.WriteFile(filepath.Join(c1Dev, "vendor"), []byte("0x8086\n"), 0o644)
	_ = os.WriteFile(filepath.Join(c1Dev, "device"), []byte("0xa78b\n"), 0o644)
	_ = os.WriteFile(filepath.Join(c1Dev, "boot_vga"), []byte("1\n"), 0o644)
	_ = os.WriteFile(filepath.Join(c1Dev, "power", "runtime_status"), []byte("active\n"), 0o644)
	_ = os.WriteFile(filepath.Join(c1Dev, "uevent"), []byte("DRIVER=i915\nPCI_SLOT_NAME=0000:00:02.0\n"), 0o644)
	_ = os.Symlink("../renderD128", filepath.Join(byPath, "pci-0000:00:02.0-render"))

	// Connector directory that must be ignored
	_ = os.MkdirAll(filepath.Join(sysDRM, "card1-eDP-1"), 0o755)

	gpus, err := DiscoverFrom(sysDRM, devDRI, false)
	if err != nil {
		t.Fatalf("DiscoverFrom failed: %v", err)
	}
	if len(gpus) != 2 {
		t.Fatalf("expected 2 GPUs, got %d", len(gpus))
	}

	// Deterministic sort: iGPU first (index 0), dGPU second (index 1)
	if gpus[0].Role != RoleIntegrated || gpus[0].Vendor != "intel" || gpus[0].RenderNode != "renderD128" {
		t.Errorf("unexpected GPU 0: %+v", gpus[0])
	}
	if gpus[1].Role != RoleDiscrete || gpus[1].Vendor != "nvidia" || gpus[1].RenderNode != "renderD129" {
		t.Errorf("unexpected GPU 1: %+v", gpus[1])
	}
	if gpus[1].PowerState != "suspended" {
		t.Errorf("expected suspended power state, got %q", gpus[1].PowerState)
	}

	// Verify selector resolution
	checks := map[string]string{
		"igpu":             "0000:00:02.0",
		"dgpu":             "0000:01:00.0",
		"0":                "0000:00:02.0",
		"1":                "0000:01:00.0",
		"nvidia":           "0000:01:00.0",
		"intel":            "0000:00:02.0",
		"card0":            "0000:01:00.0",
		"renderD128":       "0000:00:02.0",
		"01:00.0":          "0000:01:00.0",
		"00000000:01:00.0": "0000:01:00.0",
	}
	for sel, wantID := range checks {
		got, err := Resolve(gpus, sel)
		if err != nil {
			t.Errorf("Resolve(%q) returned error: %v", sel, err)
			continue
		}
		if got.ID != wantID {
			t.Errorf("Resolve(%q) = %q, want %q", sel, got.ID, wantID)
		}
	}
}
