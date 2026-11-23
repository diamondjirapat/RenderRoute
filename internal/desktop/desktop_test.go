package desktop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/diamondjirapat/RenderRoute/internal/config"
	"github.com/diamondjirapat/RenderRoute/internal/gpu"
)

func TestWrapUnwrapAndPatchReset(t *testing.T) {
	orig := `env FOO=1 /usr/bin/blender --window-geometry %f`
	wrapped := WrapExec("/usr/local/bin/renderroute", "dgpu", orig)
	if !strings.HasPrefix(wrapped, "/usr/local/bin/renderroute run dgpu -- ") {
		t.Fatalf("unexpected wrapped exec: %q", wrapped)
	}
	if unwrapped := UnwrapExec(wrapped); unwrapped != orig {
		t.Fatalf("UnwrapExec(%q) = %q, want %q", wrapped, unwrapped, orig)
	}

	// Re-wrapping must be idempotent (never double-wrap!)
	rewrapped := WrapExec("/usr/local/bin/renderroute", "igpu", wrapped)
	if strings.Count(rewrapped, "renderroute run") != 1 {
		t.Fatalf("double wrapped! %q", rewrapped)
	}

	// Test system + user override lifecycle
	root := t.TempDir()
	sysDir := filepath.Join(root, "sys")
	userDir := filepath.Join(root, "user")
	_ = os.MkdirAll(sysDir, 0o755)
	_ = os.MkdirAll(userDir, 0o755)

	sample := `[Desktop Entry]
Type=Application
Name=Blender
Exec=/usr/bin/blender %f
Icon=blender
Categories=Graphics;3DGraphics;
`
	_ = os.WriteFile(filepath.Join(sysDir, "blender.desktop"), []byte(sample), 0o644)

	searchDirs := []string{userDir, sysDir}
	dst, err := ApplyDesktopOverrideIn(searchDirs, userDir, "blender", "dgpu", "renderroute", gpu.RoleDiscrete)
	if err != nil {
		t.Fatalf("ApplyDesktopOverrideIn failed: %v", err)
	}
	patchedRaw, _ := os.ReadFile(dst)
	patchedStr := string(patchedRaw)
	if !strings.Contains(patchedStr, "Exec=renderroute run dgpu -- /usr/bin/blender %f") {
		t.Errorf("missing wrapped Exec line in:\n%s", patchedStr)
	}
	if !strings.Contains(patchedStr, "PrefersNonDefaultGPU=true") {
		t.Errorf("expected PrefersNonDefaultGPU=true in:\n%s", patchedStr)
	}

	// Discover should detect PatchedGPU and return clean unwrapped Exec
	apps := DiscoverAppsFrom(searchDirs, nil, "blender")
	if len(apps) != 1 {
		t.Fatalf("expected 1 app, got %d", len(apps))
	}
	if apps[0].PatchedGPU != "dgpu" {
		t.Errorf("expected PatchedGPU=dgpu, got %q", apps[0].PatchedGPU)
	}
	if apps[0].Exec != "/usr/bin/blender %f" {
		t.Errorf("expected clean Exec, got %q", apps[0].Exec)
	}

	// Reset should remove the user override since a system copy exists
	changed, err := ResetDesktopOverrideIn(searchDirs, userDir, "blender")
	if err != nil || !changed {
		t.Fatalf("ResetDesktopOverrideIn failed: changed=%v err=%v", changed, err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("expected user override to be removed when system file exists")
	}
}

func TestPatchDesktopDisablesDBusActivatableAndWrapsActions(t *testing.T) {
	root := t.TempDir()
	userDir := filepath.Join(root, "user")
	_ = os.MkdirAll(userDir, 0o755)

	sample := `[Desktop Entry]
Type=Application
Name=Ghostty
Exec=/usr/bin/ghostty
DBusActivatable=true
Actions=new-window;

[Desktop Action new-window]
Name=New Window
Exec=/usr/bin/ghostty --gtk-single-instance=false
`
	userFile := filepath.Join(userDir, "com.mitchellh.ghostty.desktop")
	_ = os.WriteFile(userFile, []byte(sample), 0o644)

	searchDirs := []string{userDir}
	dst, err := ApplyDesktopOverrideIn(searchDirs, userDir, "com.mitchellh.ghostty", "dgpu", "renderroute", gpu.RoleDiscrete)
	if err != nil {
		t.Fatalf("ApplyDesktopOverrideIn failed: %v", err)
	}
	raw, _ := os.ReadFile(dst)
	patched := string(raw)

	if !strings.Contains(patched, "DBusActivatable=false") {
		t.Errorf("expected DBusActivatable=false in patched file:\n%s", patched)
	}
	if !strings.Contains(patched, "X-RenderRoute-Original-DBusActivatable=true") {
		t.Errorf("expected X-RenderRoute-Original-DBusActivatable=true in patched file:\n%s", patched)
	}
	if !strings.Contains(patched, "Exec=renderroute run dgpu -- /usr/bin/ghostty --gtk-single-instance=false") {
		t.Errorf("expected [Desktop Action new-window] Exec= to be wrapped:\n%s", patched)
	}

	// Re-patch to igpu: must remain idempotent and keep original DBusActivatable marker
	if _, err := ApplyDesktopOverrideIn(searchDirs, userDir, "com.mitchellh.ghostty", "igpu", "renderroute", gpu.RoleIntegrated); err != nil {
		t.Fatalf("second ApplyDesktopOverrideIn failed: %v", err)
	}
	raw2, _ := os.ReadFile(dst)
	repatched := string(raw2)
	if strings.Count(repatched, "renderroute run") != 2 {
		t.Errorf("expected exactly 2 wrapped Exec lines (entry + action), got:\n%s", repatched)
	}
	if !strings.Contains(repatched, "X-RenderRoute-Original-DBusActivatable=true") {
		t.Errorf("lost original DBusActivatable metadata on re-patch:\n%s", repatched)
	}

	// Reset user-only desktop file: must restore DBusActivatable=true and unwrap action Exec=
	changed, err := ResetDesktopOverrideIn(searchDirs, userDir, "com.mitchellh.ghostty")
	if err != nil || !changed {
		t.Fatalf("ResetDesktopOverrideIn failed: changed=%v err=%v", changed, err)
	}
	raw3, _ := os.ReadFile(dst)
	restored := string(raw3)
	if !strings.Contains(restored, "DBusActivatable=true") {
		t.Errorf("expected DBusActivatable=true to be restored:\n%s", restored)
	}
	if strings.Contains(restored, "renderroute run") {
		t.Errorf("expected all Exec= lines to be unwrapped after reset:\n%s", restored)
	}
	if !strings.Contains(restored, "Exec=/usr/bin/ghostty --gtk-single-instance=false") {
		t.Errorf("expected action Exec= to be restored cleanly:\n%s", restored)
	}
}

func TestFindAppMatchesProcessNamesAndFlatpak(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"dev.zed.Zed.desktop": `[Desktop Entry]
Type=Application
Name=Zed
Exec=/usr/bin/flatpak run --branch=stable --arch=x86_64 --command=zed-wrapper --file-forwarding dev.zed.Zed @@u %U @@
X-Flatpak=dev.zed.Zed
`,
		"dev.vencord.Vesktop.desktop": `[Desktop Entry]
Type=Application
Name=Vesktop
Exec=/usr/bin/flatpak run --branch=stable --arch=x86_64 --command=startvesktop --file-forwarding dev.vencord.Vesktop @@u %U @@
StartupWMClass=vesktop
X-Flatpak=dev.vencord.Vesktop
`,
		"app.zen_browser.zen.desktop": `[Desktop Entry]
Type=Application
Name=Zen Browser
Exec=/usr/bin/flatpak run --branch=stable --arch=x86_64 --command=launch-script.sh --file-forwarding app.zen_browser.zen @@u %u @@
StartupWMClass=zen
X-Flatpak=app.zen_browser.zen
`,
		"com.github.wwmm.easyeffects.desktop": `[Desktop Entry]
Type=Application
Name=Easy Effects
Exec=/usr/bin/flatpak run --branch=stable --arch=x86_64 --command=easyeffects com.github.wwmm.easyeffects
X-Flatpak=com.github.wwmm.easyeffects
`,
	}
	for name, content := range files {
		_ = os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644)
	}

	apps := DiscoverAppsFrom([]string{dir}, nil, "")
	cases := map[string]string{
		"zed-editor":  "dev.zed.Zed",
		"zed-wrapper": "dev.zed.Zed",
		"vesktop.bin": "dev.vencord.Vesktop",
		"zen":         "app.zen_browser.zen",
		"easyeffects": "com.github.wwmm.easyeffects",
	}
	for query, wantID := range cases {
		got, err := FindApp(apps, query)
		if err != nil {
			t.Errorf("FindApp(%q) failed: %v", query, err)
			continue
		}
		if got.ID != wantID {
			t.Errorf("FindApp(%q) = %q, want %q", query, got.ID, wantID)
		}
	}
}

func TestPatchDesktopKDERunOnDiscreteAndAutostartOverrides(t *testing.T) {
	root := t.TempDir()
	userDir := filepath.Join(root, "apps")
	autostartDir := filepath.Join(root, "autostart")
	_ = os.MkdirAll(userDir, 0o755)
	_ = os.MkdirAll(autostartDir, 0o755)

	steamDesktop := `[Desktop Entry]
Type=Application
Name=Steam
Exec=/usr/bin/steam %U
PrefersNonDefaultGPU=true
X-KDE-RunOnDiscreteGpu=true
`
	_ = os.WriteFile(filepath.Join(userDir, "steam.desktop"), []byte(steamDesktop), 0o644)

	dst, err := ApplyDesktopOverrideIn([]string{userDir}, userDir, "steam", "igpu", "/usr/bin/renderroute", gpu.RoleIntegrated)
	if err != nil {
		t.Fatalf("ApplyDesktopOverrideIn failed: %v", err)
	}
	patchedRaw, _ := os.ReadFile(dst)
	patched := string(patchedRaw)
	if !strings.Contains(patched, "PrefersNonDefaultGPU=false") {
		t.Errorf("expected PrefersNonDefaultGPU=false:\n%s", patched)
	}
	if !strings.Contains(patched, "X-KDE-RunOnDiscreteGpu=false") {
		t.Errorf("expected X-KDE-RunOnDiscreteGpu=false:\n%s", patched)
	}

	changed, err := ResetDesktopOverrideIn([]string{userDir}, userDir, "steam")
	if err != nil || !changed {
		t.Fatalf("ResetDesktopOverrideIn failed: changed=%v err=%v", changed, err)
	}
	restoredRaw, _ := os.ReadFile(dst)
	restored := string(restoredRaw)
	if !strings.Contains(restored, "PrefersNonDefaultGPU=true") || !strings.Contains(restored, "X-KDE-RunOnDiscreteGpu=true") {
		t.Errorf("expected original GPU hints restored:\n%s", restored)
	}

	// Autostart override: filename differs from Flatpak app ID ("vesktop.desktop" -> dev.vencord.Vesktop)
	vesktopAutostart := `[Desktop Entry]
Type=Application
Name=Vesktop
Exec=/usr/bin/flatpak run --branch=stable --arch=x86_64 --command=startvesktop --file-forwarding dev.vencord.Vesktop @@u %U @@
Icon=dev.vencord.Vesktop
`
	autostartPath := filepath.Join(autostartDir, "vesktop.desktop")
	_ = os.WriteFile(autostartPath, []byte(vesktopAutostart), 0o644)

	patchedFiles, err := ApplyAutostartOverridesIn(autostartDir, "dev.vencord.Vesktop", "vesktop.bin", "dgpu", "/usr/bin/renderroute", gpu.RoleDiscrete)
	if err != nil || len(patchedFiles) != 1 {
		t.Fatalf("ApplyAutostartOverridesIn failed: files=%v err=%v", patchedFiles, err)
	}
	asPatched, _ := os.ReadFile(autostartPath)
	if !strings.Contains(string(asPatched), "Exec=/usr/bin/renderroute run dgpu -- /usr/bin/flatpak run") {
		t.Errorf("expected autostart Exec to be wrapped:\n%s", string(asPatched))
	}

	// Reset must restore in place and NOT delete the autostart file
	resetFiles, err := ResetAutostartOverridesIn(autostartDir, "dev.vencord.Vesktop", "vesktop.bin")
	if err != nil || len(resetFiles) != 1 {
		t.Fatalf("ResetAutostartOverridesIn failed: files=%v err=%v", resetFiles, err)
	}
	asRestored, err := os.ReadFile(autostartPath)
	if err != nil {
		t.Fatalf("autostart file should still exist after reset: %v", err)
	}
	if strings.Contains(string(asRestored), "renderroute run") {
		t.Errorf("expected autostart Exec to be unwrapped after reset:\n%s", string(asRestored))
	}
}

func TestSteamGameShortcutsDoNotCollideWithSteamClientRule(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"steam.desktop": `[Desktop Entry]
Type=Application
Name=Steam
Exec=/usr/bin/steam %U
Icon=steam
`,
		"Detroit Become Human.desktop": `[Desktop Entry]
Type=Application
Name=Detroit: Become Human
Exec=steam steam://rungameid/1222140
Icon=steam_icon_1222140
`,
		"Deadlock.desktop": `[Desktop Entry]
Type=Application
Name=Deadlock
Exec=steam steam://rungameid/1422450
Icon=steam_icon_1422450
`,
	}
	for name, content := range files {
		_ = os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644)
	}

	cfg := config.Default()
	cfg.SetRule(config.Rule{Target: "steam", DesktopID: "steam", GPU: "igpu"})
	cfg.SetRule(config.Rule{Target: "Detroit Become Human", DesktopID: "Detroit Become Human", GPU: "dgpu"})

	apps := DiscoverAppsFrom([]string{dir}, cfg, "")
	byID := map[string]App{}
	for _, a := range apps {
		byID[a.ID] = a
	}

	if byID["steam"].SteamAppID != "" || byID["steam"].EffectiveGPU != "igpu" {
		t.Errorf("steam client entry unexpected: %+v", byID["steam"])
	}
	if byID["Detroit Become Human"].SteamAppID != "1222140" || byID["Detroit Become Human"].EffectiveGPU != "dgpu" {
		t.Errorf("Detroit Become Human entry unexpected: %+v", byID["Detroit Become Human"])
	}
	if byID["Detroit Become Human"].DefaultRuleTarget() != "Detroit Become Human" {
		t.Errorf("expected DefaultRuleTarget to use app ID for Steam game, got %q", byID["Detroit Become Human"].DefaultRuleTarget())
	}
	// Deadlock has no rule and must NOT inherit Steam's igpu rule via app.Binary=="steam"
	if byID["Deadlock"].SteamAppID != "1422450" || byID["Deadlock"].EffectiveGPU != "" {
		t.Errorf("Deadlock should not inherit steam client rule: %+v", byID["Deadlock"])
	}

	// FindApp must match steam_app_<appid> directly to the Steam game shortcut
	gotDeadlock, err := FindApp(apps, "steam_app_1422450")
	if err != nil || gotDeadlock.ID != "Deadlock" {
		t.Fatalf("FindApp(steam_app_1422450) = %+v, %v; want Deadlock", gotDeadlock, err)
	}
	// FindApp("steam") must match the Steam client, not a Steam game
	gotSteam, err := FindApp(apps, "steam")
	if err != nil || gotSteam.ID != "steam" {
		t.Fatalf("FindApp(steam) = %+v, %v; want steam", gotSteam, err)
	}
}
