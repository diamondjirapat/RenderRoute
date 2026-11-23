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

func TestListGPUProcessesIgnoresLeakedNvidiaRenderFDAndDetectsDesktopID(t *testing.T) {
	procRoot := t.TempDir()

	// PID 1001: dolphin holding ONLY a leaked /dev/dri/renderD129 (no /dev/nvidia* FD, no DRM activity)
	leakedDir := filepath.Join(procRoot, "1001")
	_ = os.MkdirAll(filepath.Join(leakedDir, "fd"), 0o755)
	_ = os.MkdirAll(filepath.Join(leakedDir, "fdinfo"), 0o755)
	_ = os.WriteFile(filepath.Join(leakedDir, "comm"), []byte("dolphin\n"), 0o644)
	_ = os.WriteFile(filepath.Join(leakedDir, "cmdline"), []byte("dolphin\x00"), 0o644)
	_ = os.Symlink("/dev/dri/renderD129", filepath.Join(leakedDir, "fd", "33"))
	_ = os.WriteFile(filepath.Join(leakedDir, "fdinfo", "33"), []byte("pos:\t0\nflags:\t0100002\n"), 0o644)

	// PID 1002: zed-editor actually using NVIDIA (/dev/nvidia0 + /dev/dri/renderD129) with Flatpak env
	zedDir := filepath.Join(procRoot, "1002")
	_ = os.MkdirAll(filepath.Join(zedDir, "fd"), 0o755)
	_ = os.MkdirAll(filepath.Join(zedDir, "fdinfo"), 0o755)
	_ = os.WriteFile(filepath.Join(zedDir, "comm"), []byte("zed-editor\n"), 0o644)
	_ = os.WriteFile(filepath.Join(zedDir, "cmdline"), []byte("/home/user/.local/share/flatpak/app/dev.zed.Zed/x86_64/stable/abc/files/libexec/zed-editor\x00"), 0o644)
	_ = os.WriteFile(filepath.Join(zedDir, "environ"), []byte("FLATPAK_XDG_CONFIG_HOME=/home/user/.var/app/dev.zed.Zed/config\x00"), 0o644)
	_ = os.Symlink("/dev/dri/renderD129", filepath.Join(zedDir, "fd", "10"))
	_ = os.Symlink("/dev/nvidia0", filepath.Join(zedDir, "fd", "11"))

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
			ShortName:  "UHD",
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
			PowerState: "active",
		},
	}

	// Rule is keyed by Flatpak DesktopID ("dev.zed.Zed") rather than "zed-editor"
	cfg := config.Default()
	cfg.SetRule(config.Rule{Target: "dev.zed.Zed", DesktopID: "dev.zed.Zed", GPU: "igpu"})

	procs := listGPUProcessesFrom(procRoot, gpus, cfg, false)
	if len(procs) != 1 {
		t.Fatalf("expected only zed-editor (not leaked-FD dolphin), got %+v", procs)
	}
	p := procs[0]
	if p.PID != 1002 || p.Name != "zed-editor" {
		t.Fatalf("unexpected process: %+v", p)
	}
	if p.DesktopID != "dev.zed.Zed" {
		t.Errorf("expected DesktopID=dev.zed.Zed, got %q", p.DesktopID)
	}
	if p.RuleGPU != "igpu" || p.RuleMatches == nil || *p.RuleMatches {
		t.Errorf("expected rule matched via DesktopID with mismatch, got rule=%q matches=%v", p.RuleGPU, p.RuleMatches)
	}
}
