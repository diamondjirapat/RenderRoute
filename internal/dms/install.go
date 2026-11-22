package dms

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const PluginID = "renderRoute"

// PluginsDir returns the DankMaterialShell user plugins directory (~/.config/dms/plugins).
func PluginsDir() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "dms", "plugins"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	// Prefer ~/.config/dms/plugins (DankMaterialShell 2.x standard; ~/.config/DankMaterialShell is often a symlink to it)
	dmsDir := filepath.Join(home, ".config", "dms", "plugins")
	if _, err := os.Stat(filepath.Join(home, ".config", "dms")); err == nil {
		return dmsDir, nil
	}
	legacy := filepath.Join(home, ".config", "DankMaterialShell", "plugins")
	if _, err := os.Stat(filepath.Join(home, ".config", "DankMaterialShell")); err == nil {
		return legacy, nil
	}
	return dmsDir, nil
}

// PluginTargetDir returns the installed plugin path (~/.config/dms/plugins/renderRoute).
func PluginTargetDir() (string, error) {
	base, err := PluginsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, PluginID), nil
}

// FindPluginSource locates the dms-plugin directory relative to the executable or working directory.
func FindPluginSource() (string, error) {
	candidates := []string{}
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(wd, "dms-plugin"))
	}
	if exe, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(exeDir, "dms-plugin"),
			filepath.Join(exeDir, "..", "dms-plugin"),
			filepath.Join(exeDir, "..", "share", "renderroute", "dms-plugin"),
		)
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".local", "share", "renderroute", "dms-plugin"))
	}

	for _, c := range candidates {
		if st, err := os.Stat(filepath.Join(c, "plugin.json")); err == nil && !st.IsDir() {
			abs, err := filepath.Abs(c)
			if err == nil {
				return abs, nil
			}
			return c, nil
		}
	}
	return "", fmt.Errorf("could not locate dms-plugin directory (checked cwd, binary siblings, and ~/.local/share/renderroute/dms-plugin)")
}

// Install installs the DMS plugin either as a symlink (dev mode) or a directory copy.
func Install(srcDir string, useSymlink bool) (string, error) {
	if srcDir == "" {
		var err error
		srcDir, err = FindPluginSource()
		if err != nil {
			return "", err
		}
	}
	dstDir, err := PluginTargetDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(dstDir), 0o755); err != nil {
		return "", err
	}

	// Remove any existing symlink or directory at dstDir
	if _, err := os.Lstat(dstDir); err == nil {
		if err := os.RemoveAll(dstDir); err != nil {
			return "", fmt.Errorf("remove existing plugin at %s: %w", dstDir, err)
		}
	}

	if useSymlink {
		absSrc, err := filepath.Abs(srcDir)
		if err != nil {
			return "", err
		}
		if err := os.Symlink(absSrc, dstDir); err != nil {
			return "", err
		}
		return dstDir, nil
	}

	if err := copyDir(srcDir, dstDir); err != nil {
		return "", err
	}
	return dstDir, nil
}

// Uninstall removes the installed DMS plugin directory or symlink.
func Uninstall() (string, error) {
	dstDir, err := PluginTargetDir()
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(dstDir); os.IsNotExist(err) {
		return dstDir, nil
	}
	if err := os.RemoveAll(dstDir); err != nil {
		return "", err
	}
	return dstDir, nil
}

// Reload asks a running DankMaterialShell instance to reload the renderRoute plugin.
func Reload() (string, error) {
	dmsBin, err := exec.LookPath("dms")
	if err != nil {
		return "", fmt.Errorf("dms CLI not found in PATH")
	}
	out, err := exec.Command(dmsBin, "ipc", "call", "plugins", "reload", PluginID).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func copyDir(src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		sPath := filepath.Join(src, e.Name())
		dPath := filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := copyDir(sPath, dPath); err != nil {
				return err
			}
			continue
		}
		if err := copyFile(sPath, dPath); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
