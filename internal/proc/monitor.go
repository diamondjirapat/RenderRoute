package proc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/diamondjirapat/RenderRoute/internal/config"
	"github.com/diamondjirapat/RenderRoute/internal/gpu"
)

// GPUProcess describes a running process currently utilizing one or more GPUs.
type GPUProcess struct {
	PID         int      `json:"pid"`
	Name        string   `json:"name"`
	Exe         string   `json:"exe,omitempty"`
	Cmdline     string   `json:"cmdline,omitempty"`
	DesktopID   string   `json:"desktop_id,omitempty"`
	GPUID       string   `json:"gpu_id"`
	GPUName     string   `json:"gpu_name"`
	GPUShort    string   `json:"gpu_short"`
	GPURole     gpu.Role `json:"gpu_role"`
	GPUIndex    int      `json:"gpu_index"`
	VRAMUsedMB      *float64 `json:"vram_used_mb,omitempty"`
	RuleGPU         string   `json:"rule_gpu,omitempty"`
	RuleMatches     *bool    `json:"rule_matches,omitempty"`
	RuleWarningCode string   `json:"rule_warning_code,omitempty"`
	RuleWarning     string   `json:"rule_warning,omitempty"`
	OwnedByUser     bool     `json:"owned_by_user"`
}

// ListGPUProcesses discovers active processes using each GPU via /proc/<pid>/fd,
// /proc/<pid>/fdinfo, and (when an NVIDIA GPU is awake) nvidia-smi.
func ListGPUProcesses(gpus []gpu.GPU, cfg *config.Config) []GPUProcess {
	return listGPUProcessesFrom("/proc", gpus, cfg, true)
}

func listGPUProcessesFrom(procRoot string, gpus []gpu.GPU, cfg *config.Config, allowNvidiaSMI bool) []GPUProcess {
	if len(gpus) == 0 {
		return nil
	}

	myUID := os.Getuid()
	myPID := os.Getpid()

	// Lookup tables from device paths / PCI slots to GPU
	byDevPath := map[string]gpu.GPU{}
	byPCISlot := map[string]gpu.GPU{}
	nvidiaByOrdinal := map[int]gpu.GPU{}
	nvOrd := 0
	nvAwake := false
	for _, g := range gpus {
		if g.CardPath != "" {
			byDevPath[g.CardPath] = g
		}
		if g.RenderPath != "" {
			byDevPath[g.RenderPath] = g
		}
		if g.PCISlot != "" {
			byPCISlot[g.PCISlot] = g
		}
		if g.Driver == "nvidia" {
			nvidiaByOrdinal[nvOrd] = g
			byDevPath[fmt.Sprintf("/dev/nvidia%d", nvOrd)] = g
			nvOrd++
			if g.PowerState != "suspended" {
				nvAwake = true
			}
		}
	}

	// Track per-(pid, gpu_id) entries
	type key struct {
		pid   int
		gpuID string
	}
	found := map[key]*GPUProcess{}

	// 1. Scan /proc/<pid>/fd and /proc/<pid>/fdinfo
	procEntries, err := os.ReadDir(procRoot)
	if err == nil {
		for _, pe := range procEntries {
			if !pe.IsDir() {
				continue
			}
			pid, err := strconv.Atoi(pe.Name())
			if err != nil || pid <= 0 || pid == myPID {
				continue
			}

			pidDir := filepath.Join(procRoot, pe.Name())
			fdDir := filepath.Join(pidDir, "fd")
			fds, err := os.ReadDir(fdDir)
			if err != nil {
				// Permission denied for other users' processes — handled by nvidia-smi if applicable
				continue
			}

			matchedGPUs := map[string]gpu.GPU{}
			vramByGPU := map[string]float64{}
			hasDRMActivity := map[string]bool{}
			hasDRMStats := map[string]bool{}
			hasNvidiaCharDev := false

			for _, fd := range fds {
				target, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
				if err != nil {
					continue
				}
				// Strip " (deleted)" suffix if present
				target = strings.TrimSuffix(target, " (deleted)")

				if strings.HasPrefix(target, "/dev/nvidia") {
					hasNvidiaCharDev = true
				}

				var matched gpu.GPU
				ok := false
				if g, hit := byDevPath[target]; hit {
					matched = g
					ok = true
				}

				// Inspect fdinfo for drm-pdev / drm-memory-vram if DRM device
				if strings.HasPrefix(target, "/dev/dri/") {
					fdInfo := parseFDInfo(filepath.Join(pidDir, "fdinfo", fd.Name()))
					if pdev := gpu.NormalizePCISlot(fdInfo["drm-pdev"]); pdev != "" {
						if g, hit := byPCISlot[pdev]; hit {
							matched = g
							ok = true
						}
					}
					if ok {
						if hasDRMFDInfoStats(fdInfo) {
							hasDRMStats[matched.ID] = true
						}
						if mb, hasMem := parseDRMMemoryMB(fdInfo); hasMem && mb > 0 {
							if mb > vramByGPU[matched.ID] {
								vramByGPU[matched.ID] = mb
							}
							hasDRMActivity[matched.ID] = true
						}
						if hasAnyDRMMemory(fdInfo) || hasActiveDRMEngine(fdInfo) {
							hasDRMActivity[matched.ID] = true
						}
					}
				}

				if ok {
					matchedGPUs[matched.ID] = matched
				}
			}

			if len(matchedGPUs) == 0 {
				continue
			}

			name, exe, cmdline, owned := readProcMeta(pidDir, myUID)
			if shouldSkipNoiseProcess(name) {
				continue
			}
			desktopID := detectProcessDesktopID(pidDir, exe, cmdline)

			for gpuID, g := range matchedGPUs {
				// On proprietary NVIDIA drivers, real GPU contexts always open /dev/nvidia* character
				// devices (or are reported by nvidia-smi below). A process holding ONLY a leaked
				// non-CLOEXEC /dev/dri/renderD* FD (inherited from a launcher/compositor/terminal)
				// without any /dev/nvidia* FD or DRM activity is not actually using the NVIDIA GPU.
				if g.Driver == "nvidia" && !hasNvidiaCharDev && !hasDRMActivity[gpuID] {
					continue
				}
				// On DRM drivers that expose per-FD memory/engine statistics (e.g. i915, xe, amdgpu),
				// a process holding an idle probe FD with 0 memory allocated and 0 ns engine time
				// (such as Electron/Chromium browser processes probing render nodes) is not actively
				// rendering on that GPU.
				if g.Driver != "nvidia" && hasDRMStats[gpuID] && !hasDRMActivity[gpuID] {
					continue
				}
				k := key{pid: pid, gpuID: gpuID}
				p := &GPUProcess{
					PID:         pid,
					Name:        name,
					Exe:         exe,
					Cmdline:     cmdline,
					DesktopID:   desktopID,
					GPUID:       g.ID,
					GPUName:     g.Name,
					GPUShort:    g.ShortName,
					GPURole:     g.Role,
					GPUIndex:    g.Index,
					OwnedByUser: owned,
				}
				if mb, ok := vramByGPU[gpuID]; ok && mb > 0 {
					v := mb
					p.VRAMUsedMB = &v
				}
				found[k] = p
			}
		}
	}

	// 2. Enrich from nvidia-smi (only if NVIDIA GPU is awake!)
	if allowNvidiaSMI && nvAwake && len(nvidiaByOrdinal) > 0 {
		nvProcs := queryNvidiaSMIProcesses(nvidiaByOrdinal, byPCISlot)
		for _, np := range nvProcs {
			if np.pid == myPID {
				continue
			}
			k := key{pid: np.pid, gpuID: np.gpu.ID}
			if existing, ok := found[k]; ok {
				if np.vramMB > 0 {
					v := np.vramMB
					existing.VRAMUsedMB = &v
				}
				continue
			}
			pidDir := filepath.Join(procRoot, strconv.Itoa(np.pid))
			name, exe, cmdline, owned := readProcMeta(pidDir, myUID)
			if name == "" {
				name = filepath.Base(np.procName)
			}
			if exe == "" {
				exe = np.procName
			}
			if shouldSkipNoiseProcess(name) {
				continue
			}
			desktopID := detectProcessDesktopID(pidDir, exe, cmdline)
			p := &GPUProcess{
				PID:         np.pid,
				Name:        name,
				Exe:         exe,
				Cmdline:     cmdline,
				DesktopID:   desktopID,
				GPUID:       np.gpu.ID,
				GPUName:     np.gpu.Name,
				GPUShort:    np.gpu.ShortName,
				GPURole:     np.gpu.Role,
				GPUIndex:    np.gpu.Index,
				OwnedByUser: owned,
			}
			if np.vramMB > 0 {
				v := np.vramMB
				p.VRAMUsedMB = &v
			}
			found[k] = p
		}
	}

	// Annotate rule matches and diagnose bypass/mismatch causes
	out := make([]GPUProcess, 0, len(found))
	for _, p := range found {
		if cfg != nil {
			if rule, ok := cfg.MatchRule(p.Name, p.Exe, p.DesktopID); ok {
				p.RuleGPU = rule.GPU
				if resolved, err := gpu.Resolve(gpus, rule.GPU); err == nil {
					matches := resolved.ID == p.GPUID
					p.RuleMatches = &matches
					if !matches {
						pidDir := filepath.Join(procRoot, strconv.Itoa(p.PID))
						p.RuleWarningCode, p.RuleWarning = diagnoseRuleMismatch(pidDir, *p, rule, *resolved)
					}
				}
			}
		}
		out = append(out, *p)
	}

	// Sort: discrete GPUs first, then higher VRAM first, then name, then PID.
	sort.Slice(out, func(i, j int) bool {
		if (out[i].GPURole == gpu.RoleDiscrete) != (out[j].GPURole == gpu.RoleDiscrete) {
			return out[i].GPURole == gpu.RoleDiscrete
		}
		vi, vj := 0.0, 0.0
		if out[i].VRAMUsedMB != nil {
			vi = *out[i].VRAMUsedMB
		}
		if out[j].VRAMUsedMB != nil {
			vj = *out[j].VRAMUsedMB
		}
		if vi != vj {
			return vi > vj
		}
		if out[i].Name != out[j].Name {
			return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
		}
		return out[i].PID < out[j].PID
	})

	return out
}

type nvProcEntry struct {
	pid      int
	procName string
	vramMB   float64
	gpu      gpu.GPU
}

func queryNvidiaSMIProcesses(byOrd map[int]gpu.GPU, bySlot map[string]gpu.GPU) []nvProcEntry {
	var out []nvProcEntry
	seen := map[string]bool{}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Query both compute and graphics processes via nvidia-smi --query-compute-apps (includes graphics on modern drivers)
	// with gpu_bus_id so multi-NVIDIA setups map accurately.
	cmd := exec.CommandContext(ctx, "nvidia-smi",
		"--query-compute-apps=gpu_bus_id,pid,process_name,used_gpu_memory",
		"--format=csv,noheader,nounits",
	)
	if raw, err := cmd.Output(); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			cols := strings.Split(line, ",")
			if len(cols) < 4 {
				continue
			}
			slot := gpu.NormalizePCISlot(cols[0])
			g, ok := bySlot[slot]
			if !ok {
				g, ok = byOrd[0]
				if !ok {
					continue
				}
			}
			pid, err := strconv.Atoi(strings.TrimSpace(cols[1]))
			if err != nil || pid <= 0 {
				continue
			}
			vram, _ := strconv.ParseFloat(strings.TrimSpace(cols[3]), 64)
			k := fmt.Sprintf("%d:%s", pid, g.ID)
			seen[k] = true
			out = append(out, nvProcEntry{
				pid:      pid,
				procName: strings.TrimSpace(cols[2]),
				vramMB:   vram,
				gpu:      g,
			})
		}
	}

	// Also try pmon -c 1 -s m to catch pure graphics-only apps on older drivers where compute-apps omits them
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	cmd2 := exec.CommandContext(ctx2, "nvidia-smi", "pmon", "-c", "1", "-s", "m")
	if raw, err := cmd2.Output(); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			fields := strings.Fields(line)
			// gpu pid type fb ccpm command
			if len(fields) < 6 {
				continue
			}
			gpuIdx, err1 := strconv.Atoi(fields[0])
			pid, err2 := strconv.Atoi(fields[1])
			if err1 != nil || err2 != nil || pid <= 0 {
				continue
			}
			g, ok := byOrd[gpuIdx]
			if !ok {
				continue
			}
			k := fmt.Sprintf("%d:%s", pid, g.ID)
			if seen[k] {
				continue
			}
			seen[k] = true
			vram, _ := strconv.ParseFloat(fields[3], 64)
			out = append(out, nvProcEntry{
				pid:      pid,
				procName: fields[len(fields)-1],
				vramMB:   vram,
				gpu:      g,
			})
		}
	}

	return out
}

func parseFDInfo(path string) map[string]string {
	res := map[string]string{}
	b, err := os.ReadFile(path)
	if err != nil {
		return res
	}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if ok {
			res[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return res
}

func parseDRMMemoryMB(info map[string]string) (float64, bool) {
	// Support AMD/Intel/Mesa fdinfo memory keys
	for _, k := range []string{"drm-resident-vram", "drm-memory-vram", "drm-resident-local0", "drm-memory-gtt"} {
		raw, ok := info[k]
		if !ok || raw == "" {
			continue
		}
		fields := strings.Fields(raw)
		if len(fields) == 0 {
			continue
		}
		val, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			continue
		}
		unit := ""
		if len(fields) > 1 {
			unit = strings.ToLower(fields[1])
		}
		switch unit {
		case "kib", "kb":
			return val / 1024.0, true
		case "mib", "mb":
			return val, true
		case "gib", "gb":
			return val * 1024.0, true
		case "b", "bytes":
			return val / (1024.0 * 1024.0), true
		default:
			return val / 1024.0, true
		}
	}
	return 0, false
}

func readProcMeta(pidDir string, myUID int) (name, exe, cmdline string, owned bool) {
	if st, err := os.Stat(pidDir); err == nil {
		if sys, ok := st.Sys().(*syscall.Stat_t); ok {
			owned = int(sys.Uid) == myUID
		}
	}
	if b, err := os.ReadFile(filepath.Join(pidDir, "comm")); err == nil {
		name = strings.TrimSpace(string(b))
	}
	if link, err := os.Readlink(filepath.Join(pidDir, "exe")); err == nil {
		exe = strings.TrimSuffix(link, " (deleted)")
	}
	if b, err := os.ReadFile(filepath.Join(pidDir, "cmdline")); err == nil && len(b) > 0 {
		parts := bytes.Split(bytes.TrimRight(b, "\x00"), []byte{0})
		args := make([]string, 0, len(parts))
		for _, p := range parts {
			if len(p) > 0 {
				args = append(args, string(p))
			}
		}
		cmdline = strings.Join(args, " ")
		if name == "" && len(args) > 0 {
			name = filepath.Base(args[0])
		}
	}
	return name, exe, cmdline, owned
}

func hasDRMFDInfoStats(info map[string]string) bool {
	if info["drm-driver"] == "" {
		return false
	}
	for k := range info {
		if strings.HasPrefix(k, "drm-engine-") ||
			strings.HasPrefix(k, "drm-total-") ||
			strings.HasPrefix(k, "drm-resident-") ||
			strings.HasPrefix(k, "drm-memory-") {
			return true
		}
	}
	return false
}

func hasAnyDRMMemory(info map[string]string) bool {
	for k, v := range info {
		if !strings.HasPrefix(k, "drm-total-") &&
			!strings.HasPrefix(k, "drm-resident-") &&
			!strings.HasPrefix(k, "drm-memory-") &&
			!strings.HasPrefix(k, "drm-shared-") &&
			!strings.HasPrefix(k, "drm-active-") {
			continue
		}
		fields := strings.Fields(v)
		if len(fields) == 0 {
			continue
		}
		if val, err := strconv.ParseFloat(fields[0], 64); err == nil && val > 0 {
			return true
		}
	}
	return false
}

func hasActiveDRMEngine(info map[string]string) bool {
	for k, v := range info {
		if !strings.HasPrefix(k, "drm-engine-") {
			continue
		}
		fields := strings.Fields(v)
		if len(fields) == 0 {
			continue
		}
		if val, err := strconv.ParseFloat(fields[0], 64); err == nil && val > 0 {
			return true
		}
	}
	return false
}

// detectProcessDesktopID attempts to identify the desktop/Flatpak/Steam application ID owning a process
// using /proc/<pid> metadata (FLATPAK_ID, .flatpak-info, systemd cgroup scopes, exe/cmdline paths).
func detectProcessDesktopID(pidDir, exe, cmdline string) string {
	// 1. Check /proc/<pid>/environ for FLATPAK_ID or FLATPAK_XDG_*_HOME=/.var/app/<id>/
	if rawEnv, err := os.ReadFile(filepath.Join(pidDir, "environ")); err == nil && len(rawEnv) > 0 {
		var varAppID string
		for _, entry := range bytes.Split(rawEnv, []byte{0}) {
			s := string(entry)
			if strings.HasPrefix(s, "FLATPAK_ID=") {
				if id := strings.TrimSpace(strings.TrimPrefix(s, "FLATPAK_ID=")); id != "" {
					return id
				}
			}
			if varAppID == "" && strings.HasPrefix(s, "FLATPAK_XDG_") {
				if _, val, ok := strings.Cut(s, "="); ok {
					varAppID = extractFlatpakIDFromPath(val)
				}
			}
		}
		if varAppID != "" {
			return varAppID
		}
	}

	// 2. Check /proc/<pid>/root/.flatpak-info ([Application] name=<id>)
	if b, err := os.ReadFile(filepath.Join(pidDir, "root", ".flatpak-info")); err == nil && len(b) > 0 {
		inApp := false
		for _, line := range strings.Split(string(b), "\n") {
			t := strings.TrimSpace(line)
			if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
				inApp = t == "[Application]"
				continue
			}
			if inApp && strings.HasPrefix(t, "name=") {
				if id := strings.TrimSpace(strings.TrimPrefix(t, "name=")); id != "" {
					return id
				}
			}
		}
	}

	// 3. Check /proc/<pid>/cgroup for Flatpak, Steam, DMS/desktop launcher, or autostart units
	if b, err := os.ReadFile(filepath.Join(pidDir, "cgroup")); err == nil && len(b) > 0 {
		if id := extractDesktopIDFromCgroup(string(b)); id != "" {
			return id
		}
	}

	// 4. Check exe and cmdline for /flatpak/app/<id>/ or /.var/app/<id>/
	if id := extractFlatpakIDFromPath(exe); id != "" {
		return id
	}
	if id := extractFlatpakIDFromPath(cmdline); id != "" {
		return id
	}
	return ""
}

func extractDesktopIDFromCgroup(cgroupContent string) string {
	for _, line := range strings.Split(cgroupContent, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// cgroup v2 line format: "0::/user.slice/.../<unit>"
		if idx := strings.LastIndex(line, "/"); idx >= 0 {
			line = line[idx+1:]
		}
		unit := strings.TrimSpace(line)

		// Flatpak scope: app-flatpak-<escaped-app-id>-<instance>.scope
		if strings.HasPrefix(unit, "app-flatpak-") && strings.HasSuffix(unit, ".scope") {
			rest := strings.TrimSuffix(strings.TrimPrefix(unit, "app-flatpak-"), ".scope")
			if dash := strings.LastIndex(rest, "-"); dash > 0 {
				rest = rest[:dash]
			}
			rest = strings.TrimSuffix(unescapeSystemdUnit(rest), ".desktop")
			if strings.Contains(rest, ".") {
				return rest
			}
		}

		// Steam game scope: app-steam-app<appid>-<pid>.scope
		if strings.HasPrefix(unit, "app-steam-app") && strings.HasSuffix(unit, ".scope") {
			rest := strings.TrimSuffix(strings.TrimPrefix(unit, "app-steam-app"), ".scope")
			var digits strings.Builder
			for i := 0; i < len(rest); i++ {
				if rest[i] >= '0' && rest[i] <= '9' {
					digits.WriteByte(rest[i])
				} else {
					break
				}
			}
			if digits.Len() > 0 {
				return "steam_app_" + digits.String()
			}
		}

		// Standard freedesktop/systemd desktop launcher scopes:
		// app-<launcher>-<escaped-desktop-id>-<random>.scope
		for _, prefix := range []string{"app-dms-", "app-gnome-", "app-kde-", "app-uwsm-", "app-xdg-"} {
			if strings.HasPrefix(unit, prefix) && strings.HasSuffix(unit, ".scope") {
				rest := strings.TrimSuffix(strings.TrimPrefix(unit, prefix), ".scope")
				if dash := strings.LastIndex(rest, "-"); dash > 0 {
					rest = rest[:dash]
				}
				rest = strings.TrimSpace(strings.TrimSuffix(unescapeSystemdUnit(rest), ".desktop"))
				if rest != "" {
					return rest
				}
			}
		}

		// XDG autostart systemd user service: app-<escaped-desktop-id>@autostart.service
		if strings.HasPrefix(unit, "app-") && strings.HasSuffix(unit, "@autostart.service") {
			rest := strings.TrimSuffix(strings.TrimPrefix(unit, "app-"), "@autostart.service")
			rest = strings.TrimSpace(strings.TrimSuffix(unescapeSystemdUnit(rest), ".desktop"))
			if rest != "" {
				return rest
			}
		}
	}
	return ""
}

func unescapeSystemdUnit(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && s[i+1] == 'x' {
			if v, err := strconv.ParseUint(s[i+2:i+4], 16, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func extractFlatpakIDFromPath(s string) string {
	for _, marker := range []string{"/flatpak/app/", "/.var/app/"} {
		idx := strings.Index(s, marker)
		if idx < 0 {
			continue
		}
		rest := s[idx+len(marker):]
		end := strings.IndexAny(rest, "/ \t\n:")
		if end > 0 {
			rest = rest[:end]
		}
		rest = strings.TrimSpace(rest)
		if strings.Contains(rest, ".") {
			return rest
		}
	}
	return ""
}

func shouldSkipNoiseProcess(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "nvidia-smi", "renderroute":
		return true
	}
	return false
}

// diagnoseRuleMismatch inspects /proc/<pid>/cgroup and /proc/<pid>/environ to explain
// why a running process is not using its pinned GPU rule.
func diagnoseRuleMismatch(pidDir string, p GPUProcess, rule config.Rule, targetGPU gpu.GPU) (code, warning string) {
	cgroupRaw, _ := os.ReadFile(filepath.Join(pidDir, "cgroup"))
	cgroup := string(cgroupRaw)
	envMap, envReadable := readProcEnvironMap(pidDir)

	// 1. Flatpak app escaped the sandbox via flatpak-spawn --host (stripping sandbox GPU env vars)
	if strings.Contains(cgroup, "flatpak-session-helper.service") ||
		(envReadable && envMap["FLATPAK_ID"] == "" && envMap["RENDERROUTE_GPU"] == "" &&
			(envMap["FLATPAK_XDG_DATA_HOME"] != "" || envMap["FLATPAK_XDG_CONFIG_HOME"] != "")) {
		return "flatpak_host_escape", "Escaped Flatpak sandbox via flatpak-spawn --host (GPU env stripped). Restart app so RenderRoute's sandbox override applies."
	}

	// 2. Steam game running inside a Steam app scope inherited Steam's GPU environment
	if strings.HasPrefix(p.DesktopID, "steam_app_") || strings.Contains(cgroup, "app-steam-app") {
		return "steam_child_inherited", fmt.Sprintf("Steam game inherited Steam's GPU (%s). Set Steam Launch Options: renderroute run %s -- %%command%%", p.GPUShort, rule.GPU)
	}

	if envReadable {
		rrGPU := envMap["RENDERROUTE_GPU"]
		rrRole := envMap["RENDERROUTE_GPU_ROLE"]

		// 3. Process inherited RenderRoute env for a DIFFERENT GPU from a parent process
		if rrGPU != "" && !strings.EqualFold(rrGPU, targetGPU.ID) {
			inherited := rrRole
			if inherited == "" {
				inherited = rrGPU
			}
			return "parent_env_inherited", fmt.Sprintf("Inherited RenderRoute GPU env (%s) from a parent process; launch standalone or wrap with 'renderroute run %s --'", inherited, rule.GPU)
		}

		// 4. Process WAS launched with RenderRoute for the target GPU, yet still opened the wrong GPU
		if rrGPU != "" && strings.EqualFold(rrGPU, targetGPU.ID) {
			return "env_ignored", fmt.Sprintf("RenderRoute env was set for %s, but app still uses %s (it may ignore GPU env vars or follow compositor primary GPU)", rule.GPU, p.GPUShort)
		}

		// 5. Process has no RenderRoute env at all
		if rrGPU == "" {
			if !rule.DesktopPatched {
				return "no_desktop_override", "No .desktop launcher is patched for this rule — pin the app in Apps & Run or launch via 'renderroute run --auto --'"
			}
			return "launched_without_renderroute", "Launched outside RenderRoute (keybind, terminal, or pre-existing single-instance process) — relaunch to apply rule"
		}
	}

	if !rule.DesktopPatched {
		return "no_desktop_override", "No .desktop launcher is patched for this rule — pin the app in Apps & Run or launch via 'renderroute run --auto --'"
	}
	return "rule_mismatch", fmt.Sprintf("Running on %s instead of pinned %s — restart app to apply rule", p.GPUShort, rule.GPU)
}

func readProcEnvironMap(pidDir string) (map[string]string, bool) {
	raw, err := os.ReadFile(filepath.Join(pidDir, "environ"))
	if err != nil {
		return nil, false
	}
	res := make(map[string]string)
	for _, entry := range bytes.Split(raw, []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		if k, v, ok := strings.Cut(string(entry), "="); ok {
			res[k] = v
		}
	}
	return res, true
}

// RelaunchResult describes the result of restarting a running process on a target GPU.
type RelaunchResult struct {
	OldPID  int      `json:"old_pid"`
	NewPID  int      `json:"new_pid"`
	Command []string `json:"command"`
	GPUID   string   `json:"gpu_id"`
	GPUName string   `json:"gpu_name"`
}

// RelaunchProcess restarts an existing user-owned process on the target GPU.
// Because Linux cannot hot-migrate open OpenGL/Vulkan contexts between physical GPUs,
// this reads the process argv & cwd, terminates it gracefully, and re-launches it detached
// with the target GPU routing environment.
func RelaunchProcess(pid int, target gpu.GPU, allGPUs []gpu.GPU, extraEnv map[string]string, force bool) (*RelaunchResult, error) {
	if pid <= 1 || pid == os.Getpid() {
		return nil, fmt.Errorf("refusing to relaunch pid %d", pid)
	}
	pidDir := filepath.Join("/proc", strconv.Itoa(pid))
	st, err := os.Stat(pidDir)
	if err != nil {
		return nil, fmt.Errorf("process %d not found: %w", pid, err)
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok && int(sys.Uid) != os.Getuid() {
		return nil, fmt.Errorf("process %d is owned by uid %d (not current user %d)", pid, sys.Uid, os.Getuid())
	}

	rawCmd, err := os.ReadFile(filepath.Join(pidDir, "cmdline"))
	if err != nil || len(rawCmd) == 0 {
		return nil, fmt.Errorf("cannot read cmdline for pid %d", pid)
	}
	rawParts := bytes.Split(bytes.TrimRight(rawCmd, "\x00"), []byte{0})
	argv := make([]string, 0, len(rawParts))
	for _, p := range rawParts {
		argv = append(argv, string(p))
	}
	if len(argv) == 0 || argv[0] == "" {
		return nil, fmt.Errorf("empty command line for pid %d", pid)
	}

	exePath, _ := os.Readlink(filepath.Join(pidDir, "exe"))
	exePath = strings.TrimSuffix(exePath, " (deleted)")
	cwd, _ := os.Readlink(filepath.Join(pidDir, "cwd"))
	desktopID := detectProcessDesktopID(pidDir, exePath, strings.Join(argv, " "))

	// Resolve binary path; for Flatpak sandboxed processes (whose /app/... path does not exist on host),
	// relaunch cleanly via `flatpak run <app-id>`.
	bin := argv[0]
	if !filepath.IsAbs(bin) {
		if resolved, err := exec.LookPath(bin); err == nil {
			bin = resolved
		} else if exePath != "" {
			bin = exePath
		}
	}
	if _, statErr := os.Stat(bin); statErr != nil && desktopID != "" {
		if flatpakBin, err := exec.LookPath("flatpak"); err == nil {
			argv = []string{flatpakBin, "run", desktopID}
			bin = flatpakBin
			cwd = ""
		}
	}

	// Terminate gracefully
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return nil, fmt.Errorf("SIGTERM pid %d: %w", pid, err)
	}

	deadline := time.Now().Add(3 * time.Second)
	exited := false
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			exited = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !exited {
		if force {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			time.Sleep(150 * time.Millisecond)
		} else {
			return nil, fmt.Errorf("process %d did not exit after SIGTERM (re-run with --force to SIGKILL)", pid)
		}
	}

	routing := gpu.BuildRoutingEnv(target, allGPUs)
	if gpu.IsFlatpakCommand(argv) {
		argv = gpu.InjectFlatpakEnvArgs(argv, routing, extraEnv)
		bin = argv[0]
		routing = routing.ForFlatpak()
	}
	cmd := exec.Command(bin, argv[1:]...)
	if cwd != "" {
		cmd.Dir = cwd
	}
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
		return nil, fmt.Errorf("relaunch %s: %w", bin, err)
	}
	newPID := cmd.Process.Pid
	_ = cmd.Process.Release()

	return &RelaunchResult{
		OldPID:  pid,
		NewPID:  newPID,
		Command: argv,
		GPUID:   target.ID,
		GPUName: target.Name,
	}, nil
}
