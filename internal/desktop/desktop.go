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
	Path           string `json:"path"`                     // Active .desktop file path
	Categories     string `json:"categories,omitempty"`
	Terminal       bool   `json:"terminal,omitempty"`
	PatchedGPU     string `json:"patched_gpu,omitempty"`    // GPU selector if .desktop override is active
	RuleGPU        string `json:"rule_gpu,omitempty"`       // GPU selector from config rules
	EffectiveGPU   string `json:"effective_gpu,omitempty"`  // Combined GPU preference
}

// ApplicationDirs returns XDG application directories in precedence order (user overrides first).
func ApplicationDirs() []string {
	home, _ := os.UserHomeDir()
	var dirs []string
	if xdgData := os.Getenv("XDG_DATA_HOME"); xdgData != "" {
		dirs = append(dirs, filepath.Join(xdgData, "applications"))
	} else if home != "" {
		dirs = append(dirs, filepath.Join(home, ".local", "share", "applications"))
	}
	if home != "" {
		dirs = append(dirs, filepath.Join(home, ".local", "share", "flatpak", "exports", "share", "applications"))
	}
	dirs = append(dirs,
		"/var/lib/flatpak/exports/share/applications",
		"/usr/local/share/applications",
		"/usr/share/applications",
	)
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
			app, ok := parseDesktopFile(id, fullPath)
			if !ok {
				continue
			}
			seen[id] = true

			if cfg != nil {
				if r, matched := cfg.MatchRule(app.ID, app.Binary); matched {
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
	return app, true
}

// FindApp finds an app by exact ID, case-insensitive ID, exact Name, or binary name.
func FindApp(apps []App, query string) (*App, error) {
	q := strings.TrimSpace(strings.TrimSuffix(query, ".desktop"))
	if q == "" {
		return nil, errors.New("empty app identifier")
	}
	low := strings.ToLower(q)
	for i := range apps {
		if strings.EqualFold(apps[i].ID, q) {
			return &apps[i], nil
		}
	}
	for i := range apps {
		if strings.ToLower(apps[i].Name) == low || strings.ToLower(apps[i].Binary) == low {
			return &apps[i], nil
		}
	}
	for i := range apps {
		if strings.Contains(strings.ToLower(apps[i].ID), low) || strings.Contains(strings.ToLower(apps[i].Name), low) {
			return &apps[i], nil
		}
	}
	return nil, fmt.Errorf("desktop application %q not found", query)
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
	return ApplyDesktopOverrideIn(ApplicationDirs(), userDir, appID, gpuSelector, renderrouteBin, targetRole)
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

	patched := patchDesktopContent(string(raw), gpuSelector, renderrouteBin, targetRole)

	if err := os.MkdirAll(userDir, 0o755); err != nil {
		return "", err
	}
	dstPath := filepath.Join(userDir, appID+".desktop")
	if err := os.WriteFile(dstPath, []byte(patched), 0o644); err != nil {
		return "", err
	}
	return dstPath, nil
}

func patchDesktopContent(content, gpuSelector, renderrouteBin string, targetRole gpu.Role) string {
	lines := strings.Split(content, "\n")
	var out []string
	inEntry := false
	entrySeen := false
	wroteMeta := false
	wrotePrefers := false
	var origExec string

	// First pass: see if X-RenderRoute-Original-Exec already exists
	for _, l := range lines {
		if k, v, ok := strings.Cut(strings.TrimSpace(l), "="); ok && strings.TrimSpace(k) == "X-RenderRoute-Original-Exec" {
			origExec = strings.TrimSpace(v)
			break
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
				case "X-RenderRoute-GPU", "X-RenderRoute-Original-Exec":
					continue
				case "PrefersNonDefaultGPU":
					out = append(out, "PrefersNonDefaultGPU="+prefersVal)
					wrotePrefers = true
					continue
				case "Exec":
					if origExec == "" {
						origExec = UnwrapExec(val)
					}
					out = append(out, "Exec="+WrapExec(renderrouteBin, gpuSelector, origExec))
					continue
				}
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
	return ResetDesktopOverrideIn(ApplicationDirs(), userDir, appID)
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
	if !strings.Contains(content, managedHeaderPrefix) && !strings.Contains(content, "X-RenderRoute-GPU=") {
		return false, nil
	}

	// Check if a backing system/flatpak .desktop file exists
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

	if hasSystemCopy {
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

func unpatchDesktopContent(content string) string {
	lines := strings.Split(content, "\n")
	var origExec string
	for _, l := range lines {
		if k, v, ok := strings.Cut(strings.TrimSpace(l), "="); ok && strings.TrimSpace(k) == "X-RenderRoute-Original-Exec" {
			origExec = strings.TrimSpace(v)
			break
		}
	}
	var out []string
	inEntry := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, managedHeaderPrefix) {
			continue
		}
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			inEntry = trimmed == "[Desktop Entry]"
			out = append(out, line)
			continue
		}
		if inEntry {
			if k, v, ok := strings.Cut(trimmed, "="); ok {
				key := strings.TrimSpace(k)
				val := strings.TrimSpace(v)
				switch key {
				case "X-RenderRoute-GPU", "X-RenderRoute-Original-Exec":
					continue
				case "Exec":
					if origExec != "" {
						out = append(out, "Exec="+origExec)
					} else {
						out = append(out, "Exec="+UnwrapExec(val))
					}
					continue
				}
			}
		}
		out = append(out, line)
	}
	return strings.TrimLeft(strings.Join(out, "\n"), "\n")
}

// LaunchApp spawns a .desktop application detached with the target GPU environment.
func LaunchApp(app App, target gpu.GPU, allGPUs []gpu.GPU, extraEnv map[string]string) (int, error) {
	argv := buildLaunchArgv(app.Exec)
	if len(argv) == 0 {
		return 0, fmt.Errorf("empty Exec command for %s", app.ID)
	}

	routing := gpu.BuildRoutingEnv(target, allGPUs)
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
