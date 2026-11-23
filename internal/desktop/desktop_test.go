package desktop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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
