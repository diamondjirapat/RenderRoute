package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/diamondjirapat/RenderRoute/internal/config"
	"github.com/diamondjirapat/RenderRoute/internal/desktop"
	"github.com/diamondjirapat/RenderRoute/internal/gpu"
	"github.com/diamondjirapat/RenderRoute/internal/proc"
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

func TestResolveRuleAppProtectsSteamClientFromGameCollision(t *testing.T) {
	apps := []desktop.App{
		{
			ID:     "steam",
			Name:   "Steam",
			Binary: "steam",
			Exec:   "/usr/bin/steam %U",
		},
		{
			ID:         "Detroit Become Human",
			Name:       "Detroit: Become Human",
			Binary:     "steam",
			SteamAppID: "1222140",
			Exec:       "steam steam://rungameid/1222140",
		},
		{
			ID:         "Deadlock",
			Name:       "Deadlock",
			Binary:     "steam",
			SteamAppID: "1422450",
			Exec:       "steam steam://rungameid/1422450",
		},
	}

	// Pinning a Steam game shortcut while passing binary "steam" must rewrite target to the game ID
	target, desktopID, name, steamAppID := resolveRuleApp(apps, "steam", "Detroit Become Human", "Detroit: Become Human")
	if target != "Detroit Become Human" || desktopID != "Detroit Become Human" || steamAppID != "1222140" || name != "Detroit: Become Human" {
		t.Errorf("unexpected Steam game resolution: target=%q desktopID=%q name=%q steamAppID=%q", target, desktopID, name, steamAppID)
	}

	// Pinning Steam client itself must keep target="steam" and empty steamAppID
	target2, desktopID2, name2, steamAppID2 := resolveRuleApp(apps, "steam", "steam", "Steam")
	if target2 != "steam" || desktopID2 != "steam" || steamAppID2 != "" || name2 != "Steam" {
		t.Errorf("unexpected Steam client resolution: target=%q desktopID=%q name=%q steamAppID=%q", target2, desktopID2, name2, steamAppID2)
	}

	// Pinning a running Steam game process with cgroup desktop ID steam_app_1422450 must resolve to Deadlock
	target3, desktopID3, name3, steamAppID3 := resolveRuleApp(apps, "MainThrd", "steam_app_1422450", "MainThrd")
	if target3 != "MainThrd" || desktopID3 != "Deadlock" || name3 != "Deadlock" || steamAppID3 != "1422450" {
		t.Errorf("unexpected steam_app_ resolution: target=%q desktopID=%q name=%q steamAppID=%q", target3, desktopID3, name3, steamAppID3)
	}
}

func TestCollectRoutingWarningsDeduplicatesPerPIDAndCode(t *testing.T) {
	procs := []proc.GPUProcess{
		{PID: 100, Name: "zed-editor", GPUShort: "RTX 4050", RuleGPU: "igpu", RuleWarningCode: "flatpak_host_escape", RuleWarning: "Escaped sandbox"},
		{PID: 100, Name: "zed-editor", GPUShort: "RTX 4050", RuleGPU: "igpu", RuleWarningCode: "flatpak_host_escape", RuleWarning: "Escaped sandbox"},
		{PID: 200, Name: "easyeffects", GPUShort: "UHD", RuleGPU: "igpu"},
	}
	got := collectRoutingWarnings(procs)
	if len(got) != 1 {
		t.Fatalf("expected 1 deduplicated routing warning, got %+v", got)
	}
	if got[0].PID != 100 || got[0].Code != "flatpak_host_escape" {
		t.Errorf("unexpected routing warning: %+v", got[0])
	}
}

func TestIsSteamClientRule(t *testing.T) {
	if !isSteamClientRule("steam", "steam", "") {
		t.Errorf("expected native steam client to match")
	}
	if !isSteamClientRule("steam", "com.valvesoftware.Steam", "") {
		t.Errorf("expected flatpak steam client to match")
	}
	if isSteamClientRule("Detroit Become Human", "Detroit Become Human", "1222140") {
		t.Errorf("expected steam game shortcut not to be treated as steam client")
	}
	if isSteamClientRule("dolphin", "org.kde.dolphin", "") {
		t.Errorf("expected unrelated app not to match")
	}
}

func TestLoadConfigForReadFallsBackOnCorruptConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, "renderroute"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "renderroute", "config.json"), []byte(`{"rules": {`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := loadConfigForRead()
	if cfg == nil {
		t.Fatal("loadConfigForRead returned nil")
	}
	// Must not panic: this is what `renderroute run` does for every patched launcher.
	if _, ok := cfg.MatchRule("blender"); ok {
		t.Errorf("unexpected rule match on fallback config")
	}
	if g, err := resolveWithConfig([]gpu.GPU{{ID: "a", Role: gpu.RoleIntegrated}}, cfg, "default"); err != nil || g.ID != "a" {
		t.Errorf("default resolution on fallback config failed: %v %v", g, err)
	}
}
