package proc

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/diamondjirapat/RenderRoute/internal/config"
	"github.com/diamondjirapat/RenderRoute/internal/gpu"
)

func TestParseDRMMemoryUnitsAndRegions(t *testing.T) {
	cases := []struct {
		info   map[string]string
		wantMB float64
		wantOK bool
	}{
		{map[string]string{"drm-resident-vram": "65536 KiB"}, 64, true},
		{map[string]string{"drm-resident-vram": "2 MiB"}, 2, true},
		// Unit-less values are bytes per the DRM fdinfo spec.
		{map[string]string{"drm-resident-vram": "1048576"}, 1, true},
		{map[string]string{"drm-resident-vram0": "4096 KiB"}, 4, true},
		// GTT is host system memory, not VRAM.
		{map[string]string{"drm-memory-gtt": "65536 KiB"}, 0, false},
	}
	for _, tc := range cases {
		got, ok := parseDRMMemoryMB(tc.info)
		if ok != tc.wantOK || got != tc.wantMB {
			t.Errorf("parseDRMMemoryMB(%v) = %v, %v; want %v, %v", tc.info, got, ok, tc.wantMB, tc.wantOK)
		}
	}
}

func TestMultiGPUProcessOnPinnedGPUIsNotFlaggedAsMismatch(t *testing.T) {
	procRoot := t.TempDir()
	pidDir := filepath.Join(procRoot, "5150")
	_ = os.MkdirAll(filepath.Join(pidDir, "fd"), 0o755)
	_ = os.MkdirAll(filepath.Join(pidDir, "fdinfo"), 0o755)
	_ = os.WriteFile(filepath.Join(pidDir, "comm"), []byte("blender\n"), 0o644)
	_ = os.WriteFile(filepath.Join(pidDir, "cmdline"), []byte("/usr/bin/blender\x00"), 0o644)
	// Active on the iGPU (buffer import) ...
	_ = os.Symlink("/dev/dri/renderD128", filepath.Join(pidDir, "fd", "10"))
	_ = os.WriteFile(filepath.Join(pidDir, "fdinfo", "10"), []byte("drm-driver:\ti915\ndrm-pdev:\t0000:00:02.0\ndrm-total-system0:\t4096 KiB\n"), 0o644)
	// ... and rendering on the AMD dGPU it is pinned to.
	_ = os.Symlink("/dev/dri/renderD129", filepath.Join(pidDir, "fd", "11"))
	_ = os.WriteFile(filepath.Join(pidDir, "fdinfo", "11"), []byte("drm-driver:\tamdgpu\ndrm-pdev:\t0000:03:00.0\ndrm-memory-vram:\t524288 KiB\n"), 0o644)

	gpus := []gpu.GPU{
		{Index: 0, ID: "0000:00:02.0", CardPath: "/dev/dri/card1", RenderPath: "/dev/dri/renderD128", RenderNode: "renderD128", PCISlot: "0000:00:02.0", Vendor: "intel", Driver: "i915", Role: gpu.RoleIntegrated},
		{Index: 1, ID: "0000:03:00.0", CardPath: "/dev/dri/card0", RenderPath: "/dev/dri/renderD129", RenderNode: "renderD129", PCISlot: "0000:03:00.0", Vendor: "amd", Driver: "amdgpu", Role: gpu.RoleDiscrete},
	}
	cfg := config.Default()
	cfg.SetRule(config.Rule{Target: "blender", GPU: "dgpu"})

	procs := listGPUProcessesFrom(procRoot, gpus, cfg, false)
	if len(procs) != 2 {
		t.Fatalf("expected one entry per GPU (2), got %d: %+v", len(procs), procs)
	}
	for _, p := range procs {
		if p.RuleMatches == nil || !*p.RuleMatches || p.RuleWarning != "" {
			t.Errorf("process active on its pinned dGPU must not be flagged on %s: matches=%v warning=%q", p.GPUID, p.RuleMatches, p.RuleWarning)
		}
	}
}

func TestRelaunchRefusesBeforeKillingWhenExecutableMissing(t *testing.T) {
	sleepBin, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep not available")
	}
	data, err := os.ReadFile(sleepBin)
	if err != nil {
		t.Skip(err)
	}
	tmpBin := filepath.Join(t.TempDir(), "rr-test-sleeper")
	if err := os.WriteFile(tmpBin, data, 0o755); err != nil {
		t.Fatal(err)
	}
	child := exec.Command(tmpBin, "30")
	// Clean env: the test runner may itself be a host process spawned from a Flatpak app.
	child.Env = []string{"PATH=/usr/bin:/bin"}
	if err := child.Start(); err != nil {
		t.Skip(err)
	}
	defer func() {
		_ = child.Process.Kill()
		_, _ = child.Process.Wait()
	}()
	// The binary disappears (e.g. package upgrade/uninstall) so the process cannot be restarted.
	_ = os.Remove(tmpBin)
	time.Sleep(50 * time.Millisecond)

	_, err = RelaunchProcess(child.Process.Pid, gpu.GPU{}, nil, nil, true)
	if err == nil {
		t.Fatalf("expected relaunch to fail when the executable is gone")
	}
	if killErr := syscall.Kill(child.Process.Pid, 0); killErr != nil {
		t.Fatalf("process was terminated even though it could not be relaunched: %v", killErr)
	}
}
