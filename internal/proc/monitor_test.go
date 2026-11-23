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

func TestExtractDesktopIDFromCgroupAndIdleMesaFDFiltering(t *testing.T) {
	cgroupCases := map[string]string{
		"0::/user.slice/user-1000.slice/user@1000.service/app.slice/app-flatpak-dev.vencord.Vesktop-3849228871.scope\n": "dev.vencord.Vesktop",
		"0::/user.slice/user-1000.slice/user@1000.service/app.slice/app-dms-org.kde.dolphin-mwsv38f4dcs5.scope\n":       "org.kde.dolphin",
		`0::/user.slice/user-1000.slice/user@1000.service/app.slice/app-dms-Detroit\x20Become\x20Human-abc123.scope`:    "Detroit Become Human",
		"0::/user.slice/user-1000.slice/user@1000.service/app.slice/app-steam-app1422450-50625.scope\n":                 "steam_app_1422450",
		"0::/user.slice/user-1000.slice/user@1000.service/app.slice/app-com.github.wwmm.easyeffects@autostart.service":  "com.github.wwmm.easyeffects",
	}
	for raw, want := range cgroupCases {
		if got := extractDesktopIDFromCgroup(raw); got != want {
			t.Errorf("extractDesktopIDFromCgroup(%q) = %q, want %q", raw, got, want)
		}
	}

	procRoot := t.TempDir()

	// PID 2001: Electron browser process holding an idle i915 probe FD (0 memory, 0 ns engines) -> should be ignored
	idleDir := filepath.Join(procRoot, "2001")
	_ = os.MkdirAll(filepath.Join(idleDir, "fd"), 0o755)
	_ = os.MkdirAll(filepath.Join(idleDir, "fdinfo"), 0o755)
	_ = os.WriteFile(filepath.Join(idleDir, "comm"), []byte("vesktop.bin\n"), 0o644)
	_ = os.WriteFile(filepath.Join(idleDir, "cmdline"), []byte("/app/bin/vesktop/vesktop.bin\x00"), 0o644)
	_ = os.Symlink("/dev/dri/renderD128", filepath.Join(idleDir, "fd", "50"))
	_ = os.WriteFile(filepath.Join(idleDir, "fdinfo", "50"), []byte("drm-driver:\ti915\ndrm-pdev:\t0000:00:02.0\ndrm-total-system0:\t0\ndrm-engine-render:\t0 ns\n"), 0o644)

	// PID 2002: easyeffects actively using i915 (drm-total-system0: 2316 KiB) in a DMS/autostart cgroup -> must be kept
	activeDir := filepath.Join(procRoot, "2002")
	_ = os.MkdirAll(filepath.Join(activeDir, "fd"), 0o755)
	_ = os.MkdirAll(filepath.Join(activeDir, "fdinfo"), 0o755)
	_ = os.WriteFile(filepath.Join(activeDir, "comm"), []byte("easyeffects\n"), 0o644)
	_ = os.WriteFile(filepath.Join(activeDir, "cmdline"), []byte("easyeffects\x00"), 0o644)
	_ = os.WriteFile(filepath.Join(activeDir, "cgroup"), []byte("0::/user.slice/user-1000.slice/user@1000.service/app.slice/app-dms-com.github.wwmm.easyeffects-abc123.scope\n"), 0o644)
	_ = os.Symlink("/dev/dri/renderD128", filepath.Join(activeDir, "fd", "143"))
	_ = os.WriteFile(filepath.Join(activeDir, "fdinfo", "143"), []byte("drm-driver:\ti915\ndrm-pdev:\t0000:00:02.0\ndrm-total-system0:\t2316 KiB\ndrm-engine-render:\t0 ns\n"), 0o644)

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
	}

	procs := listGPUProcessesFrom(procRoot, gpus, config.Default(), false)
	if len(procs) != 1 {
		t.Fatalf("expected only active easyeffects (not idle i915 probe vesktop.bin), got %+v", procs)
	}
	if procs[0].PID != 2002 || procs[0].DesktopID != "com.github.wwmm.easyeffects" {
		t.Errorf("unexpected process result: %+v", procs[0])
	}
}

func TestDiagnoseRuleMismatchCoversBypassBehaviors(t *testing.T) {
	igpu := gpu.GPU{ID: "0000:00:02.0", Role: gpu.RoleIntegrated, ShortName: "UHD"}
	dgpu := gpu.GPU{ID: "0000:01:00.0", Role: gpu.RoleDiscrete, ShortName: "RTX 4050"}

	tests := []struct {
		name     string
		cgroup   string
		environ  string
		proc     GPUProcess
		rule     config.Rule
		target   gpu.GPU
		wantCode string
	}{
		{
			name:     "flatpak host escape",
			cgroup:   "0::/user.slice/user-1000.slice/user@1000.service/app.slice/flatpak-session-helper.service\n",
			environ:  "FLATPAK_XDG_DATA_HOME=/home/user/.var/app/dev.zed.Zed/data\x00",
			proc:     GPUProcess{PID: 10, Name: "zed-editor", GPUShort: "RTX 4050"},
			rule:     config.Rule{Target: "zed-editor", GPU: "igpu", DesktopPatched: true},
			target:   igpu,
			wantCode: "flatpak_host_escape",
		},
		{
			name:     "steam game inherited steam gpu",
			cgroup:   "0::/user.slice/user-1000.slice/user@1000.service/app.slice/app-steam-app1422450-50625.scope\n",
			environ:  "RENDERROUTE_GPU=0000:00:02.0\x00RENDERROUTE_GPU_ROLE=igpu\x00",
			proc:     GPUProcess{PID: 20, Name: "MainThrd", DesktopID: "steam_app_1422450", GPUShort: "UHD"},
			rule:     config.Rule{Target: "mainthrd", GPU: "dgpu", DesktopPatched: false},
			target:   dgpu,
			wantCode: "steam_child_inherited",
		},
		{
			name:     "parent renderroute env inherited",
			cgroup:   "0::/user.slice/user-1000.slice/user@1000.service/app.slice/app-dms-parent-123.scope\n",
			environ:  "RENDERROUTE_GPU=0000:00:02.0\x00RENDERROUTE_GPU_ROLE=igpu\x00",
			proc:     GPUProcess{PID: 30, Name: "childapp", GPUShort: "UHD"},
			rule:     config.Rule{Target: "childapp", GPU: "dgpu", DesktopPatched: true},
			target:   dgpu,
			wantCode: "parent_env_inherited",
		},
		{
			name:     "app ignored renderroute env",
			cgroup:   "0::/user.slice/user-1000.slice/user@1000.service/app.slice/app-dms-foo-123.scope\n",
			environ:  "RENDERROUTE_GPU=0000:00:02.0\x00RENDERROUTE_GPU_ROLE=igpu\x00",
			proc:     GPUProcess{PID: 40, Name: "foo", GPUShort: "RTX 4050"},
			rule:     config.Rule{Target: "foo", GPU: "igpu", DesktopPatched: true},
			target:   igpu,
			wantCode: "env_ignored",
		},
		{
			name:     "no desktop override patched",
			cgroup:   "0::/user.slice/user-1000.slice/user@1000.service/app.slice/app-niri-foo-123.scope\n",
			environ:  "PATH=/usr/bin\x00",
			proc:     GPUProcess{PID: 50, Name: "foo", GPUShort: "RTX 4050"},
			rule:     config.Rule{Target: "foo", GPU: "igpu", DesktopPatched: false},
			target:   igpu,
			wantCode: "no_desktop_override",
		},
		{
			name:     "launched outside renderroute",
			cgroup:   "0::/user.slice/user-1000.slice/user@1000.service/app.slice/app-niri-ghostty-123.scope\n",
			environ:  "PATH=/usr/bin\x00",
			proc:     GPUProcess{PID: 60, Name: "ghostty", GPUShort: "RTX 4050"},
			rule:     config.Rule{Target: "ghostty", GPU: "igpu", DesktopPatched: true},
			target:   igpu,
			wantCode: "launched_without_renderroute",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pidDir := t.TempDir()
			if tc.cgroup != "" {
				_ = os.WriteFile(filepath.Join(pidDir, "cgroup"), []byte(tc.cgroup), 0o644)
			}
			if tc.environ != "" {
				_ = os.WriteFile(filepath.Join(pidDir, "environ"), []byte(tc.environ), 0o644)
			}
			code, msg := diagnoseRuleMismatch(pidDir, tc.proc, tc.rule, tc.target)
			if code != tc.wantCode {
				t.Errorf("code = %q, want %q (msg=%q)", code, tc.wantCode, msg)
			}
			if msg == "" {
				t.Errorf("expected non-empty warning message for %q", tc.name)
			}
		})
	}
}
