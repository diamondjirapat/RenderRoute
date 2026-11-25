package desktop

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/diamondjirapat/RenderRoute/internal/config"
	"github.com/diamondjirapat/RenderRoute/internal/gpu"
)

const managedHeaderPrefix = "# Managed-by: RenderRoute"

// App represents an installed .desktop application entry.
type App struct {
	ID             string `json:"id"`                       // e.g. "blender" or "org.mozilla.firefox"
	Name           string `json:"name"`                     // Display name
	Comment        string `json:"comment,omitempty"`        // Short description
	Icon           string `json:"icon,omitempty"`           // Icon name or path
	Exec           string `json:"exec"`                     // Raw Exec= string (unwrapped)
	Binary         string `json:"binary"`                   // Primary executable basename
	TryExec        string `json:"try_exec,omitempty"`       // TryExec basename if present
	StartupWMClass string `json:"startup_wm_class,omitempty"`
	FlatpakID      string `json:"flatpak_id,omitempty"`      // X-Flatpak app ID if present
	FlatpakCommand string `json:"flatpak_command,omitempty"` // --command= value from flatpak run if present
	SteamAppID     string `json:"steam_app_id,omitempty"`    // Steam AppID if Exec launches steam://rungameid/<id>
	Path           string `json:"path"`                      // Active .desktop file path
	Categories     string `json:"categories,omitempty"`
	Terminal       bool   `json:"terminal,omitempty"`
	PatchedGPU     string `json:"patched_gpu,omitempty"`   // GPU selector if .desktop override is active
	RuleGPU        string `json:"rule_gpu,omitempty"`      // GPU selector from config rules
	EffectiveGPU   string `json:"effective_gpu,omitempty"` // Combined GPU preference
}

// ApplicationDirs returns XDG application directories in precedence order (user overrides first):
// $XDG_DATA_HOME, then each entry of $XDG_DATA_DIRS (default /usr/local/share:/usr/share).
// Flatpak export directories are included even when the session did not add them to XDG_DATA_DIRS.
func ApplicationDirs() []string {
	home, _ := os.UserHomeDir()
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" && home != "" {
		dataHome = filepath.Join(home, ".local", "share")
	}
	dataDirsEnv := os.Getenv("XDG_DATA_DIRS")
	if strings.TrimSpace(dataDirsEnv) == "" {
		dataDirsEnv = "/usr/local/share:/usr/share"
	}
	var dataDirs []string
	for _, d := range strings.Split(dataDirsEnv, ":") {
		if d = strings.TrimSpace(d); d != "" && filepath.IsAbs(d) {
			dataDirs = append(dataDirs, d)
		}
	}
	return applicationDirsFrom(dataHome, dataDirs)
}

func applicationDirsFrom(dataHome string, dataDirs []string) []string {
	seen := map[string]bool{}
	var dirs []string
	add := func(base string) {
		if base == "" {
			return
		}
		d := filepath.Clean(filepath.Join(base, "applications"))
		if !seen[d] {
			seen[d] = true
			dirs = append(dirs, d)
		}
	}
	add(dataHome)
	// Flatpak exports take precedence over distro packages (matching Flatpak's own profile.d ordering).
	if dataHome != "" {
		add(filepath.Join(dataHome, "flatpak", "exports", "share"))
	}
	add("/var/lib/flatpak/exports/share")
	for _, d := range dataDirs {
		add(d)
	}
	return dirs
}

// UserApplicationsDir returns ~/.local/share/applications (or $XDG_DATA_HOME/applications).
func UserApplicationsDir() (string, error) {
	if xdgData := os.Getenv("XDG_DATA_HOME"); xdgData != "" {
		return filepath.Join(xdgData, "applications"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "applications"), nil
}

// UserAutostartDir returns ~/.config/autostart (or $XDG_CONFIG_HOME/autostart).
func UserAutostartDir() (string, error) {
	if xdgCfg := os.Getenv("XDG_CONFIG_HOME"); xdgCfg != "" {
		return filepath.Join(xdgCfg, "autostart"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "autostart"), nil
}

// DiscoverApps lists visible desktop applications, respecting user overrides over system entries.
func DiscoverApps(cfg *config.Config, query string) []App {
	return DiscoverAppsFrom(ApplicationDirs(), cfg, query)
}

// DiscoverAppsFrom scans the provided directories in precedence order.
func DiscoverAppsFrom(dirs []string, cfg *config.Config, query string) []App {
	seen := map[string]bool{}
	var apps []App
	q := strings.ToLower(strings.TrimSpace(query))

	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".desktop") {
				continue
			}
			id := strings.TrimSuffix(e.Name(), ".desktop")
			if seen[id] {
				continue
			}
			fullPath := filepath.Join(dir, e.Name())
			// Per the XDG spec the highest-precedence file for a desktop ID wins even when it is
			// hidden (Hidden=true / NoDisplay=true is how users hide a system app), so mark the ID
			// as seen before deciding whether it is visible.
			seen[id] = true
			app, ok := parseDesktopFile(id, fullPath)
			if !ok {
				continue
			}

			if cfg != nil {
				if r, matched := cfg.MatchRule(app.RuleMatchCandidates()...); matched {
					app.RuleGPU = r.GPU
				}
			}
			if app.RuleGPU != "" {
				app.EffectiveGPU = app.RuleGPU
			} else if app.PatchedGPU != "" {
				app.EffectiveGPU = app.PatchedGPU
			}

			if q != "" {
				haystack := strings.ToLower(app.Name + " " + app.ID + " " + app.Binary + " " + app.Comment)
				if !strings.Contains(haystack, q) {
					continue
				}
			}
			apps = append(apps, app)
		}
	}

	sort.Slice(apps, func(i, j int) bool {
		// Pin apps with configured GPU rules first, then alphabetical by Name.
		hasI := apps[i].EffectiveGPU != ""
		hasJ := apps[j].EffectiveGPU != ""
		if hasI != hasJ {
			return hasI
		}
		ni := strings.ToLower(apps[i].Name)
		nj := strings.ToLower(apps[j].Name)
		if ni != nj {
			return ni < nj
		}
		return apps[i].ID < apps[j].ID
	})

	return apps
}

func parseDesktopFile(id, path string) (App, bool) {
	f, err := os.Open(path)
	if err != nil {
		return App{}, false
	}
	defer f.Close()

	inEntry := false
	var app App
	app.ID = id
	app.Path = path
	var entryType string
	noDisplay := false
	hidden := false
	var origExec string

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			inEntry = line == "[Desktop Entry]"
			continue
		}
		if !inEntry {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		switch k {
		case "Type":
			entryType = v
		case "Name":
			if app.Name == "" {
				app.Name = v
			}
		case "Comment":
			if app.Comment == "" {
				app.Comment = v
			}
		case "Icon":
			app.Icon = v
		case "Exec":
			app.Exec = v
		case "TryExec":
			app.TryExec = filepath.Base(v)
		case "StartupWMClass":
			app.StartupWMClass = v
		case "X-Flatpak":
			app.FlatpakID = v
		case "Categories":
			app.Categories = v
		case "Terminal":
			app.Terminal = strings.EqualFold(v, "true")
		case "NoDisplay":
			noDisplay = strings.EqualFold(v, "true")
		case "Hidden":
			hidden = strings.EqualFold(v, "true")
		case "X-RenderRoute-GPU":
			app.PatchedGPU = v
		case "X-RenderRoute-Original-Exec":
			origExec = v
		}
	}

	if entryType != "Application" || noDisplay || hidden || app.Name == "" || app.Exec == "" {
		return App{}, false
	}

	if origExec != "" {
		app.Exec = origExec
	} else {
		app.Exec = UnwrapExec(app.Exec)
	}
	app.Binary = extractPrimaryBinary(app.Exec)
	app.FlatpakCommand = extractFlatpakCommand(app.Exec)
	app.SteamAppID = extractSteamAppID(app.Exec)
	return app, true
}

// RuleMatchCandidates returns the rule lookup keys for this desktop application.
// Steam game shortcuts (Exec=steam steam://rungameid/<id>) intentionally omit app.Binary ("steam")
// so they do not inherit or collide with rules targeting the Steam client itself.
func (a App) RuleMatchCandidates() []string {
	if a.SteamAppID != "" {
		return []string{a.ID, "steam_app_" + a.SteamAppID, a.StartupWMClass}
	}
	return []string{a.ID, a.Binary, a.FlatpakID, a.StartupWMClass, a.FlatpakCommand}
}

// DefaultRuleTarget returns the preferred persistent rule key for this application.
func (a App) DefaultRuleTarget() string {
	if a.SteamAppID != "" {
		return a.ID
	}
	if a.Binary != "" {
		return a.Binary
	}
	return a.ID
}

// FindApp finds an app by exact ID/FlatpakID, exact Name/Binary/StartupWMClass/TryExec/FlatpakCommand,
// normalized executable stem (e.g. "vesktop.bin" -> "vesktop", "zed-editor" -> "zed"),
// or substring fallback.
func FindApp(apps []App, query string) (*App, error) {
	q := strings.TrimSpace(strings.TrimSuffix(query, ".desktop"))
	if q == "" {
		return nil, errors.New("empty app identifier")
	}
	if strings.Contains(q, "/") {
		q = filepath.Base(q)
	}
	low := strings.ToLower(q)

	// Tier 1: exact ID, X-Flatpak, or steam_app_<id> match
	for i := range apps {
		if strings.EqualFold(apps[i].ID, q) ||
			(apps[i].FlatpakID != "" && strings.EqualFold(apps[i].FlatpakID, q)) ||
			(apps[i].SteamAppID != "" && strings.EqualFold("steam_app_"+apps[i].SteamAppID, q)) {
			return &apps[i], nil
		}
	}

	// Tier 2: exact Name, Binary, StartupWMClass, TryExec, FlatpakCommand, or reverse-DNS leaf
	for i := range apps {
		if appMatchesExactToken(apps[i], low) {
			return &apps[i], nil
		}
	}

	// Tier 3: stripped executable stem (e.g. "vesktop.bin" -> "vesktop", "zed-editor" -> "zed")
	if stem := stripProcessSuffix(low); stem != "" && stem != low {
		for i := range apps {
			if appMatchesExactToken(apps[i], stem) {
				return &apps[i], nil
			}
		}
	}

	// Tier 4: substring match on ID or Name
	for i := range apps {
		if strings.Contains(strings.ToLower(apps[i].ID), low) || strings.Contains(strings.ToLower(apps[i].Name), low) {
			return &apps[i], nil
		}
	}
	return nil, fmt.Errorf("desktop application %q not found", query)
}

func appMatchesExactToken(app App, low string) bool {
	if low == "" {
		return false
	}
	binMatch := app.SteamAppID == "" && strings.ToLower(app.Binary) == low
	if strings.ToLower(app.Name) == low ||
		binMatch ||
		strings.ToLower(app.StartupWMClass) == low ||
		strings.ToLower(app.TryExec) == low ||
		strings.ToLower(app.FlatpakCommand) == low {
		return true
	}
	if leaf := reverseDNSLeaf(app.ID); leaf != "" && leaf == low {
		return true
	}
	if leaf := reverseDNSLeaf(app.FlatpakID); leaf != "" && leaf == low {
		return true
	}
	return false
}

func reverseDNSLeaf(id string) string {
	id = strings.TrimSpace(strings.TrimSuffix(id, ".desktop"))
	if !strings.Contains(id, ".") {
		return ""
	}
	parts := strings.Split(id, ".")
	last := strings.ToLower(strings.TrimSpace(parts[len(parts)-1]))
	if len(last) < 2 {
		return ""
	}
	return last
}

func stripProcessSuffix(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	for _, suf := range []string{
		".bin", "-bin", ".exe",
		"-editor", "-wrapper", "-wrapped",
		"-desktop", "-browser", "-client",
		"-gtk", "-qt", "-wayland", "-x11",
	} {
		if strings.HasSuffix(s, suf) && len(s) > len(suf)+1 {
			return strings.TrimSuffix(s, suf)
		}
	}
	return s
}

// UnwrapExec strips any existing "renderroute run <gpu> -- " prefix from an Exec= value.
func UnwrapExec(execLine string) string {
	s := strings.TrimSpace(execLine)
	tokens := SplitDesktopExec(s)
	if len(tokens) >= 4 {
		base := filepath.Base(tokens[0])
		if base == "renderroute" && tokens[1] == "run" {
			for i := 2; i < len(tokens); i++ {
				if tokens[i] == "--" && i+1 < len(tokens) {
					return JoinDesktopExec(tokens[i+1:])
				}
			}
		}
	}
	return s
}

// WrapExec prefixes an Exec= command cleanly with renderroute run <gpuSelector> -- ...
func WrapExec(renderrouteBin, gpuSelector, execLine string) string {
	clean := UnwrapExec(execLine)
	if renderrouteBin == "" {
		renderrouteBin = "renderroute"
	}
	return fmt.Sprintf("%s run %s -- %s", quoteIfNeeded(renderrouteBin), quoteIfNeeded(gpuSelector), clean)
}

// ApplyDesktopOverride writes a user-local .desktop override in ~/.local/share/applications
// that wraps Exec= with RenderRoute so launching the app from any desktop launcher uses the chosen GPU.
func ApplyDesktopOverride(appID, gpuSelector, renderrouteBin string, targetRole gpu.Role) (string, error) {
	userDir, err := UserApplicationsDir()
	if err != nil {
		return "", err
	}
	dst, err := ApplyDesktopOverrideIn(ApplicationDirs(), userDir, appID, gpuSelector, renderrouteBin, targetRole)
	if err == nil {
		refreshDesktopDatabase(userDir)
	}
	return dst, err
}

// ApplyDesktopOverrideIn performs the .desktop patch against custom search/output dirs (for testing).
func ApplyDesktopOverrideIn(searchDirs []string, userDir, appID, gpuSelector, renderrouteBin string, targetRole gpu.Role) (string, error) {
	appID = strings.TrimSuffix(strings.TrimSpace(appID), ".desktop")
	if appID == "" {
		return "", errors.New("empty app ID")
	}

	srcPath := ""
	for _, dir := range searchDirs {
		cand := filepath.Join(dir, appID+".desktop")
		if _, err := os.Stat(cand); err == nil {
			srcPath = cand
			break
		}
	}
	if srcPath == "" {
		return "", fmt.Errorf("cannot locate %s.desktop in XDG application directories", appID)
	}

	raw, err := os.ReadFile(srcPath)
	if err != nil {
		return "", err
	}
	content := string(raw)

	// If we are patching a pre-existing user file (not one RenderRoute generated from a system copy),
	// mark it so ResetDesktopOverride restores it in place instead of deleting the user's own edits.
	preexistingUserFile := filepath.Clean(filepath.Dir(srcPath)) == filepath.Clean(userDir) && !isManagedDesktopContent(content)
	patched := patchDesktopContentOpts(content, gpuSelector, renderrouteBin, targetRole, preexistingUserFile)

	if err := os.MkdirAll(userDir, 0o755); err != nil {
		return "", err
	}
	dstPath := filepath.Join(userDir, appID+".desktop")
	if err := os.WriteFile(dstPath, []byte(patched), 0o644); err != nil {
		return "", err
	}
	return dstPath, nil
}

// userOverrideKey marks a patched user-local .desktop file that existed before RenderRoute touched it.
const userOverrideKey = "X-RenderRoute-User-Override"

func isManagedDesktopContent(content string) bool {
	return strings.Contains(content, managedHeaderPrefix) || strings.Contains(content, "X-RenderRoute-GPU=")
}

func patchDesktopContent(content, gpuSelector, renderrouteBin string, targetRole gpu.Role) string {
	return patchDesktopContentOpts(content, gpuSelector, renderrouteBin, targetRole, false)
}

func patchDesktopContentOpts(content, gpuSelector, renderrouteBin string, targetRole gpu.Role, userOverride bool) string {
	lines := strings.Split(content, "\n")
	var out []string
	inEntry := false
	inAction := false
	entrySeen := false
	wroteMeta := false
	wrotePrefers := false
	var origExec string
	origDBusActivatable := false
	origPrefers := ""
	origKDERunDiscrete := ""

	// First pass: preserve original metadata if re-patching an already managed file
	for _, l := range lines {
		if k, v, ok := strings.Cut(strings.TrimSpace(l), "="); ok {
			key := strings.TrimSpace(k)
			val := strings.TrimSpace(v)
			switch key {
			case "X-RenderRoute-Original-Exec":
				origExec = val
			case "X-RenderRoute-Original-DBusActivatable":
				if strings.EqualFold(val, "true") {
					origDBusActivatable = true
				}
			case "X-RenderRoute-Original-PrefersNonDefaultGPU":
				origPrefers = val
			case "X-RenderRoute-Original-KDE-RunOnDiscreteGpu":
				origKDERunDiscrete = val
			case userOverrideKey:
				if strings.EqualFold(val, "true") {
					userOverride = true
				}
			}
		}
	}

	prefersVal := "false"
	if targetRole == gpu.RoleDiscrete {
		prefersVal = "true"
	}

	flushMeta := func() {
		if wroteMeta {
			return
		}
		if !wrotePrefers {
			out = append(out, "PrefersNonDefaultGPU="+prefersVal)
			wrotePrefers = true
		}
		out = append(out, "X-RenderRoute-GPU="+gpuSelector)
		if origExec != "" {
			out = append(out, "X-RenderRoute-Original-Exec="+origExec)
		}
		if origDBusActivatable {
			out = append(out, "X-RenderRoute-Original-DBusActivatable=true")
		}
		if origPrefers != "" {
			out = append(out, "X-RenderRoute-Original-PrefersNonDefaultGPU="+origPrefers)
		}
		if origKDERunDiscrete != "" {
			out = append(out, "X-RenderRoute-Original-KDE-RunOnDiscreteGpu="+origKDERunDiscrete)
		}
		if userOverride {
			out = append(out, userOverrideKey+"=true")
		}
		wroteMeta = true
	}

	out = append(out, fmt.Sprintf("%s (gpu=%s)", managedHeaderPrefix, gpuSelector))

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, managedHeaderPrefix) {
			continue
		}
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			if inEntry {
				flushMeta()
			}
			inEntry = trimmed == "[Desktop Entry]"
			inAction = strings.HasPrefix(trimmed, "[Desktop Action ")
			if inEntry {
				entrySeen = true
			}
			out = append(out, line)
			continue
		}
		if inEntry {
			if k, v, ok := strings.Cut(trimmed, "="); ok {
				key := strings.TrimSpace(k)
				val := strings.TrimSpace(v)
				switch key {
				case "X-RenderRoute-GPU",
					"X-RenderRoute-Original-Exec",
					"X-RenderRoute-Original-DBusActivatable",
					"X-RenderRoute-Original-PrefersNonDefaultGPU",
					"X-RenderRoute-Original-KDE-RunOnDiscreteGpu",
					userOverrideKey:
					continue
				case "PrefersNonDefaultGPU":
					if origPrefers == "" && !strings.Contains(content, "X-RenderRoute-GPU=") {
						origPrefers = val
					}
					out = append(out, "PrefersNonDefaultGPU="+prefersVal)
					wrotePrefers = true
					continue
				case "X-KDE-RunOnDiscreteGpu":
					if origKDERunDiscrete == "" && !strings.Contains(content, "X-RenderRoute-GPU=") {
						origKDERunDiscrete = val
					}
					out = append(out, "X-KDE-RunOnDiscreteGpu="+prefersVal)
					continue
				case "DBusActivatable":
					// D-Bus activation bypasses Exec= completely; disable it so launchers invoke RenderRoute.
					if strings.EqualFold(val, "true") {
						origDBusActivatable = true
					}
					out = append(out, "DBusActivatable=false")
					continue
				case "Exec":
					if origExec == "" {
						origExec = UnwrapExec(val)
					}
					out = append(out, "Exec="+WrapExec(renderrouteBin, gpuSelector, origExec))
					continue
				}
			}
		} else if inAction {
			if k, v, ok := strings.Cut(trimmed, "="); ok && strings.TrimSpace(k) == "Exec" {
				out = append(out, "Exec="+WrapExec(renderrouteBin, gpuSelector, strings.TrimSpace(v)))
				continue
			}
		}
		out = append(out, line)
	}
	if entrySeen && inEntry {
		flushMeta()
	}

	result := strings.Join(out, "\n")
	if !strings.HasSuffix(result, "\n") {
		result += "\n"
	}
	return result
}

// ResetDesktopOverride removes a RenderRoute-managed user .desktop override (if a system file exists)
// or restores its original Exec= line in place (if it only exists in the user directory).
func ResetDesktopOverride(appID string) (bool, error) {
	userDir, err := UserApplicationsDir()
	if err != nil {
		return false, err
	}
	changed, err := ResetDesktopOverrideIn(ApplicationDirs(), userDir, appID)
	if err == nil && changed {
		refreshDesktopDatabase(userDir)
	}
	return changed, err
}

// ResetDesktopOverrideIn resets an override with custom search/user directories (for testing).
func ResetDesktopOverrideIn(searchDirs []string, userDir, appID string) (bool, error) {
	appID = strings.TrimSuffix(strings.TrimSpace(appID), ".desktop")
	userFile := filepath.Join(userDir, appID+".desktop")
	raw, err := os.ReadFile(userFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}

	content := string(raw)
	if !isManagedDesktopContent(content) {
		return false, nil
	}

	// Check if a backing system/flatpak .desktop file exists. A user file that pre-dated RenderRoute
	// (marked with X-RenderRoute-User-Override) is always restored in place, never deleted.
	hasSystemCopy := false
	for _, dir := range searchDirs {
		if filepath.Clean(dir) == filepath.Clean(userDir) {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, appID+".desktop")); err == nil {
			hasSystemCopy = true
			break
		}
	}

	if hasSystemCopy && !desktopHasUserOverrideMarker(content) {
		if err := os.Remove(userFile); err != nil {
			return false, err
		}
		return true, nil
	}

	// User-only .desktop file: restore original Exec= and strip RenderRoute metadata
	restored := unpatchDesktopContent(content)
	if err := os.WriteFile(userFile, []byte(restored), 0o644); err != nil {
		return false, err
	}
	return true, nil
}

func desktopHasUserOverrideMarker(content string) bool {
	for _, l := range strings.Split(content, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(l), "="); ok && strings.TrimSpace(k) == userOverrideKey {
			return strings.EqualFold(strings.TrimSpace(v), "true")
		}
	}
	return false
}

func unpatchDesktopContent(content string) string {
	lines := strings.Split(content, "\n")
	var origExec string
	origDBusActivatable := false
	origPrefers := ""
	origKDERunDiscrete := ""
	for _, l := range lines {
		if k, v, ok := strings.Cut(strings.TrimSpace(l), "="); ok {
			key := strings.TrimSpace(k)
			val := strings.TrimSpace(v)
			switch key {
			case "X-RenderRoute-Original-Exec":
				origExec = val
			case "X-RenderRoute-Original-DBusActivatable":
				if strings.EqualFold(val, "true") {
					origDBusActivatable = true
				}
			case "X-RenderRoute-Original-PrefersNonDefaultGPU":
				origPrefers = val
			case "X-RenderRoute-Original-KDE-RunOnDiscreteGpu":
				origKDERunDiscrete = val
			}
		}
	}
	var out []string
	inEntry := false
	inAction := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, managedHeaderPrefix) {
			continue
		}
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			inEntry = trimmed == "[Desktop Entry]"
			inAction = strings.HasPrefix(trimmed, "[Desktop Action ")
			out = append(out, line)
			continue
		}
		if inEntry {
			if k, v, ok := strings.Cut(trimmed, "="); ok {
				key := strings.TrimSpace(k)
				val := strings.TrimSpace(v)
				switch key {
				case "X-RenderRoute-GPU",
					"X-RenderRoute-Original-Exec",
					"X-RenderRoute-Original-DBusActivatable",
					"X-RenderRoute-Original-PrefersNonDefaultGPU",
					"X-RenderRoute-Original-KDE-RunOnDiscreteGpu",
					userOverrideKey:
					continue
				case "PrefersNonDefaultGPU":
					if origPrefers != "" {
						out = append(out, "PrefersNonDefaultGPU="+origPrefers)
					}
					continue
				case "X-KDE-RunOnDiscreteGpu":
					if origKDERunDiscrete != "" {
						out = append(out, "X-KDE-RunOnDiscreteGpu="+origKDERunDiscrete)
						continue
					}
				case "DBusActivatable":
					if origDBusActivatable {
						out = append(out, "DBusActivatable=true")
						continue
					}
				case "Exec":
					if origExec != "" {
						out = append(out, "Exec="+origExec)
					} else {
						out = append(out, "Exec="+UnwrapExec(val))
					}
					continue
				}
			}
		} else if inAction {
			if k, v, ok := strings.Cut(trimmed, "="); ok && strings.TrimSpace(k) == "Exec" {
				out = append(out, "Exec="+UnwrapExec(strings.TrimSpace(v)))
				continue
			}
		}
		out = append(out, line)
	}
	return strings.TrimLeft(strings.Join(out, "\n"), "\n")
}

// ApplyAutostartOverrides patches any matching ~/.config/autostart/*.desktop files in place
// so autostarted applications honor the pinned GPU rule on login.
func ApplyAutostartOverrides(appID, targetBinary, gpuSelector, renderrouteBin string, targetRole gpu.Role) ([]string, error) {
	dir, err := UserAutostartDir()
	if err != nil {
		return nil, err
	}
	return ApplyAutostartOverridesIn(dir, appID, targetBinary, gpuSelector, renderrouteBin, targetRole)
}

// ApplyAutostartOverridesIn patches matching autostart .desktop files inside autostartDir (for testing).
func ApplyAutostartOverridesIn(autostartDir, appID, targetBinary, gpuSelector, renderrouteBin string, targetRole gpu.Role) ([]string, error) {
	matches, err := findMatchingAutostartFiles(autostartDir, appID, targetBinary)
	if err != nil || len(matches) == 0 {
		return nil, err
	}
	var patched []string
	for _, path := range matches {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		updated := patchDesktopContent(string(raw), gpuSelector, renderrouteBin, targetRole)
		if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
			return patched, err
		}
		patched = append(patched, path)
	}
	return patched, nil
}

// ResetAutostartOverrides restores any RenderRoute-patched ~/.config/autostart/*.desktop files in place
// without removing the user's autostart entries.
func ResetAutostartOverrides(appID, targetBinary string) ([]string, error) {
	dir, err := UserAutostartDir()
	if err != nil {
		return nil, err
	}
	return ResetAutostartOverridesIn(dir, appID, targetBinary)
}

// ResetAutostartOverridesIn restores matching patched autostart files inside autostartDir (for testing).
func ResetAutostartOverridesIn(autostartDir, appID, targetBinary string) ([]string, error) {
	matches, err := findMatchingAutostartFiles(autostartDir, appID, targetBinary)
	if err != nil || len(matches) == 0 {
		return nil, err
	}
	var reset []string
	for _, path := range matches {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		content := string(raw)
		if !strings.Contains(content, managedHeaderPrefix) && !strings.Contains(content, "X-RenderRoute-GPU=") {
			continue
		}
		restored := unpatchDesktopContent(content)
		if err := os.WriteFile(path, []byte(restored), 0o644); err != nil {
			return reset, err
		}
		reset = append(reset, path)
	}
	return reset, nil
}

func findMatchingAutostartFiles(autostartDir, appID, targetBinary string) ([]string, error) {
	entries, err := os.ReadDir(autostartDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	want := map[string]bool{}
	addWant := func(s string) {
		s = strings.ToLower(strings.TrimSpace(strings.TrimSuffix(s, ".desktop")))
		if s == "" {
			return
		}
		want[s] = true
		if leaf := reverseDNSLeaf(s); leaf != "" {
			want[leaf] = true
		}
		if stem := stripProcessSuffix(s); stem != "" {
			want[stem] = true
		}
	}
	addWant(appID)
	addWant(targetBinary)
	if len(want) == 0 {
		return nil, nil
	}

	var matched []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".desktop") {
			continue
		}
		fullPath := filepath.Join(autostartDir, e.Name())
		fileID := strings.ToLower(strings.TrimSuffix(e.Name(), ".desktop"))
		if want[fileID] {
			matched = append(matched, fullPath)
			continue
		}
		raw, err := os.ReadFile(fullPath)
		if err != nil {
			continue
		}
		if autostartContentMatches(string(raw), want) {
			matched = append(matched, fullPath)
		}
	}
	sort.Strings(matched)
	return matched, nil
}

func autostartContentMatches(content string, want map[string]bool) bool {
	inEntry := false
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			inEntry = trimmed == "[Desktop Entry]"
			continue
		}
		if !inEntry {
			continue
		}
		k, v, ok := strings.Cut(trimmed, "=")
		if !ok {
			continue
		}
		key := strings.TrimSpace(k)
		val := strings.TrimSpace(v)
		switch key {
		case "X-Flatpak", "X-XDP-Autostart", "StartupWMClass", "Icon":
			if want[strings.ToLower(val)] {
				return true
			}
		case "Exec", "X-RenderRoute-Original-Exec":
			unwrapped := UnwrapExec(val)
			bin := strings.ToLower(extractPrimaryBinary(unwrapped))
			if bin != "" && want[bin] {
				return true
			}
			fpCmd := strings.ToLower(extractFlatpakCommand(unwrapped))
			if fpCmd != "" && want[fpCmd] {
				return true
			}
		}
	}
	return false
}

func refreshDesktopDatabase(dir string) {
	if bin, err := exec.LookPath("update-desktop-database"); err == nil {
		_ = exec.Command(bin, dir).Run()
	}
}

// LaunchApp spawns a .desktop application detached with the target GPU environment.
func LaunchApp(app App, target gpu.GPU, allGPUs []gpu.GPU, extraEnv map[string]string) (int, error) {
	argv := buildLaunchArgv(app.Exec)
	if len(argv) == 0 {
		return 0, fmt.Errorf("empty Exec command for %s", app.ID)
	}

	routing := gpu.BuildRoutingEnv(target, allGPUs)
	if gpu.IsFlatpakCommand(argv) {
		argv = gpu.InjectFlatpakEnvArgs(argv, routing, extraEnv)
		routing = routing.ForFlatpak()
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = routing.Apply(os.Environ(), extraEnv)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err == nil {
		cmd.Stdin = devNull
		cmd.Stdout = devNull
		cmd.Stderr = devNull
		defer devNull.Close()
	}

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("launch %s: %w", app.ID, err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	return pid, nil
}

// buildLaunchArgv splits an Exec= string and strips freedesktop field codes (%u, %U, %f, %F, %i, %c, %k).
func buildLaunchArgv(execLine string) []string {
	tokens := SplitDesktopExec(UnwrapExec(execLine))
	out := make([]string, 0, len(tokens))
	for _, tok := range tokens {
		switch tok {
		case "%f", "%F", "%u", "%U", "%d", "%D", "%n", "%N", "%i", "%c", "%k", "%v", "%m":
			continue
		}
		tok = strings.ReplaceAll(tok, "%%", "%")
		if tok != "" {
			out = append(out, tok)
		}
	}
	return out
}

func extractSteamAppID(execLine string) string {
	for _, tok := range SplitDesktopExec(execLine) {
		low := strings.ToLower(tok)
		for _, prefix := range []string{"steam://rungameid/", "steam://run/"} {
			if strings.HasPrefix(low, prefix) {
				rest := tok[len(prefix):]
				var digits strings.Builder
				for i := 0; i < len(rest); i++ {
					if rest[i] >= '0' && rest[i] <= '9' {
						digits.WriteByte(rest[i])
					} else {
						break
					}
				}
				if digits.Len() > 0 {
					return digits.String()
				}
			}
		}
	}
	return ""
}

func extractFlatpakCommand(execLine string) string {
	tokens := SplitDesktopExec(execLine)
	for i := 0; i < len(tokens); i++ {
		if filepath.Base(tokens[i]) == "flatpak" && i+1 < len(tokens) && tokens[i+1] == "run" {
			for j := i + 2; j < len(tokens); j++ {
				tok := tokens[j]
				if tok == "--" {
					break
				}
				if strings.HasPrefix(tok, "--command=") {
					return filepath.Base(strings.TrimPrefix(tok, "--command="))
				}
				if tok == "--command" && j+1 < len(tokens) {
					return filepath.Base(tokens[j+1])
				}
				if !strings.HasPrefix(tok, "-") {
					break
				}
			}
			break
		}
	}
	return ""
}

func extractPrimaryBinary(execLine string) string {
	tokens := SplitDesktopExec(execLine)
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		// Skip env wrappers like "env FOO=bar cmd"
		if i == 0 && filepath.Base(tok) == "env" {
			continue
		}
		if strings.Contains(tok, "=") && !strings.HasPrefix(tok, "/") {
			continue
		}
		// Skip flatpak run <app-id> -> return the app-id or binary
		if filepath.Base(tok) == "flatpak" && i+1 < len(tokens) && tokens[i+1] == "run" {
			for j := i + 2; j < len(tokens); j++ {
				if !strings.HasPrefix(tokens[j], "-") {
					return tokens[j]
				}
			}
		}
		return filepath.Base(tok)
	}
	return ""
}

// SplitDesktopExec tokenizes a freedesktop Exec= string honoring quotes and escapes.
func SplitDesktopExec(s string) []string {
	var tokens []string
	var cur strings.Builder
	inSingle := false
	inDouble := false
	escaped := false

	for _, r := range s {
		if escaped {
			cur.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' && !inSingle {
			escaped = true
			continue
		}
		if r == '\'' && !inDouble {
			inSingle = !inSingle
			continue
		}
		if r == '"' && !inSingle {
			inDouble = !inDouble
			continue
		}
		if (r == ' ' || r == '\t') && !inSingle && !inDouble {
			if cur.Len() > 0 {
				tokens = append(tokens, cur.String())
				cur.Reset()
			}
			continue
		}
		cur.WriteRune(r)
	}
	if cur.Len() > 0 {
		tokens = append(tokens, cur.String())
	}
	return tokens
}

// JoinDesktopExec joins tokens back into a valid Exec= string.
func JoinDesktopExec(tokens []string) string {
	quoted := make([]string, len(tokens))
	for i, t := range tokens {
		quoted[i] = quoteIfNeeded(t)
	}
	return strings.Join(quoted, " ")
}

func quoteIfNeeded(s string) string {
	if s == "" {
		return `""`
	}
	if !strings.ContainsAny(s, " \t\"'\\$") {
		return s
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}
