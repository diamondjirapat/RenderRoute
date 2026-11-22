package proc

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/diamondjirapat/RenderRoute/internal/config"
	"github.com/diamondjirapat/RenderRoute/internal/gpu"
)

func TestListGPUProcessesFromSyntheticProc(t *testing.T) {
	procRoot := t.TempDir()

	// Synthetic PID 4242 using renderD128 (Intel iGPU)
	pidDir := filepath.Join(procRoot, "4242")
	_ = os.MkdirAll(filepath.Join(pidDir, "fd"), 0o755)
	_ = os.MkdirAll(filepath.Join(pidDir, "fdinfo"), 0o755)
	_ = os.WriteFile(filepath.Join(pidDir, "comm"), []byte("firefox\n"), 0o644)
	_ = os.WriteFile(filepath.Join(pidDir, "cmdline"), []byte("/usr/bin/firefox\x00--new-window\x00"), 0o644)
	_ = os.Symlink("/dev/dri/renderD128", filepath.Join(pidDir, "fd", "12"))
	_ = os.WriteFile(filepath.Join(pidDir, "fdinfo", "12"), []byte("drm-pdev:\t0000:00:02.0\ndrm-resident-vram:\t65536 KiB\n"), 0o644)

	gpus := []gpu.GPU{
		{
			Index:      0,
			ID:         "0000:00:02.0",
			Card:       "card1",
			CardPath:   "/dev/dri/card1",
			RenderNode: "renderD128",
			RenderPath: "/dev/dri/renderD128",
			PCISlot:    "0000:00:02.0",
			Vendor:     "intel",
			Driver:     "i915",
			Name:       "Intel UHD",
			ShortName:  "Intel UHD",
			Role:       gpu.RoleIntegrated,
		},
		{
			Index:      1,
			ID:         "0000:01:00.0",
			Card:       "card0",
			CardPath:   "/dev/dri/card0",
			RenderNode: "renderD129",
			RenderPath: "/dev/dri/renderD129",
			PCISlot:    "0000:01:00.0",
			Vendor:     "nvidia",
			Driver:     "nvidia",
			Name:       "RTX 4050",
			ShortName:  "RTX 4050",
			Role:       gpu.RoleDiscrete,
			PowerState: "suspended",
		},
	}

	cfg := config.Default()
	cfg.SetRule(config.Rule{Target: "firefox", GPU: "dgpu"})

	procs := listGPUProcessesFrom(procRoot, gpus, cfg, false)
	if len(procs) != 1 {
		t.Fatalf("expected 1 GPU process, got %d", len(procs))
	}
	p := procs[0]
	if p.PID != 4242 || p.Name != "firefox" || p.GPUID != "0000:00:02.0" {
		t.Errorf("unexpected process: %+v", p)
	}
	if p.VRAMUsedMB == nil || *p.VRAMUsedMB != 64 {
		t.Errorf("expected 64MB VRAM from fdinfo, got %v", p.VRAMUsedMB)
	}
	if p.RuleGPU != "dgpu" || p.RuleMatches == nil || *p.RuleMatches {
		t.Errorf("expected rule mismatch (running on iGPU, rule says dgpu), got rule=%q matches=%v", p.RuleGPU, p.RuleMatches)
	}
}
