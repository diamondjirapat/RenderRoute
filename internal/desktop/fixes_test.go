package desktop

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/diamondjirapat/RenderRoute/internal/gpu"
)

func TestResetPreservesPreexistingUserDesktopOverride(t *testing.T) {
	root := t.TempDir()
	sysDir := filepath.Join(root, "usr_share_applications")
	userDir := filepath.Join(root, "local_applications")
	_ = os.MkdirAll(sysDir, 0o755)
	_ = os.MkdirAll(userDir, 0o755)
	_ = os.WriteFile(filepath.Join(sysDir, "blender.desktop"), []byte("[Desktop Entry]\nType=Application\nName=Blender\nExec=blender %f\n"), 0o644)
	// User's own hand-edited override (custom flag) that predates RenderRoute.
	userContent := "[Desktop Entry]\nType=Application\nName=Blender (custom)\nExec=blender --python-use-system-env %f\n"
	userFile := filepath.Join(userDir, "blender.desktop")
	_ = os.WriteFile(userFile, []byte(userContent), 0o644)
	searchDirs := []string{userDir, sysDir}

	if _, err := ApplyDesktopOverrideIn(searchDirs, userDir, "blender", "dgpu", "renderroute", gpu.RoleDiscrete); err != nil {
		t.Fatal(err)
	}
	// Re-pinning must keep the marker.
	if _, err := ApplyDesktopOverrideIn(searchDirs, userDir, "blender", "igpu", "renderroute", gpu.RoleIntegrated); err != nil {
		t.Fatal(err)
	}
	patched, _ := os.ReadFile(userFile)
	if !strings.Contains(string(patched), userOverrideKey+"=true") || !strings.Contains(string(patched), "--python-use-system-env") {
		t.Fatalf("expected patched user file with override marker and user flags, got:\n%s", patched)
	}

	changed, err := ResetDesktopOverrideIn(searchDirs, userDir, "blender")
	if err != nil || !changed {
		t.Fatalf("reset failed: changed=%v err=%v", changed, err)
	}
	restored, err := os.ReadFile(userFile)
	if err != nil {
		t.Fatalf("user's own override was deleted on reset: %v", err)
	}
	got := string(restored)
	if strings.Contains(got, "RenderRoute") || !strings.Contains(got, "Exec=blender --python-use-system-env %f") || !strings.Contains(got, "Name=Blender (custom)") {
		t.Errorf("user override not restored faithfully:\n%s", got)
	}

	// A RenderRoute-generated override (copied from the system file) is still removed on reset.
	_ = os.Remove(userFile)
	if _, err := ApplyDesktopOverrideIn(searchDirs, userDir, "blender", "dgpu", "renderroute", gpu.RoleDiscrete); err != nil {
		t.Fatal(err)
	}
	if changed, err := ResetDesktopOverrideIn(searchDirs, userDir, "blender"); err != nil || !changed {
		t.Fatalf("reset failed: changed=%v err=%v", changed, err)
	}
	if _, err := os.Stat(userFile); !os.IsNotExist(err) {
		t.Errorf("generated override should be removed when a system copy exists")
	}
}

func TestHiddenUserOverrideMasksSystemEntry(t *testing.T) {
	root := t.TempDir()
	sysDir := filepath.Join(root, "sys")
	userDir := filepath.Join(root, "user")
	_ = os.MkdirAll(sysDir, 0o755)
	_ = os.MkdirAll(userDir, 0o755)
	_ = os.WriteFile(filepath.Join(sysDir, "htop.desktop"), []byte("[Desktop Entry]\nType=Application\nName=Htop\nExec=htop\n"), 0o644)
	_ = os.WriteFile(filepath.Join(userDir, "htop.desktop"), []byte("[Desktop Entry]\nType=Application\nName=Htop\nExec=htop\nNoDisplay=true\n"), 0o644)
	_ = os.WriteFile(filepath.Join(sysDir, "vim.desktop"), []byte("[Desktop Entry]\nType=Application\nName=Vim\nExec=vim\n"), 0o644)

	apps := DiscoverAppsFrom([]string{userDir, sysDir}, nil, "")
	if len(apps) != 1 || apps[0].ID != "vim" {
		t.Errorf("expected only vim (htop hidden by user override), got %+v", apps)
	}
}

func TestApplicationDirsHonorsXDGDataDirs(t *testing.T) {
	got := applicationDirsFrom("/home/u/.local/share", []string{"/run/current-system/sw/share", "/var/lib/flatpak/exports/share", "/usr/share"})
	want := []string{
		"/home/u/.local/share/applications",
		"/home/u/.local/share/flatpak/exports/share/applications",
		"/var/lib/flatpak/exports/share/applications",
		"/run/current-system/sw/share/applications",
		"/usr/share/applications",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("applicationDirsFrom =\n%v\nwant\n%v", got, want)
	}

	t.Setenv("XDG_DATA_HOME", "/xdg/home")
	t.Setenv("XDG_DATA_DIRS", "/opt/apps/share:relative/ignored")
	dirs := ApplicationDirs()
	if dirs[0] != "/xdg/home/applications" || dirs[len(dirs)-1] != "/opt/apps/share/applications" {
		t.Errorf("ApplicationDirs ignored XDG env: %v", dirs)
	}
}
