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
	GPUID       string   `json:"gpu_id"`
	GPUName     string   `json:"gpu_name"`
	GPUShort    string   `json:"gpu_short"`
	GPURole     gpu.Role `json:"gpu_role"`
	GPUIndex    int      `json:"gpu_index"`
	VRAMUsedMB  *float64 `json:"vram_used_mb,omitempty"`
	RuleGPU     string   `json:"rule_gpu,omitempty"`
	RuleMatches *bool    `json:"rule_matches,omitempty"`
	OwnedByUser bool     `json:"owned_by_user"`
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

			for _, fd := range fds {
				target, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
				if err != nil {
					continue
				}
				// Strip " (deleted)" suffix if present
				target = strings.TrimSuffix(target, " (deleted)")

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
						if mb, hasMem := parseDRMMemoryMB(fdInfo); hasMem && mb > vramByGPU[matched.ID] {
							vramByGPU[matched.ID] = mb
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

			for gpuID, g := range matchedGPUs {
				k := key{pid: pid, gpuID: gpuID}
				p := &GPUProcess{
					PID:         pid,
					Name:        name,
					Exe:         exe,
					Cmdline:     cmdline,
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
			p := &GPUProcess{
				PID:         np.pid,
				Name:        name,
				Exe:         exe,
				Cmdline:     cmdline,
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

	// Annotate rule matches and build sorted result
	out := make([]GPUProcess, 0, len(found))
	for _, p := range found {
		if cfg != nil {
			if rule, ok := cfg.MatchRule(p.Name, p.Exe); ok {
				p.RuleGPU = rule.GPU
				if resolved, err := gpu.Resolve(gpus, rule.GPU); err == nil {
					matches := resolved.ID == p.GPUID
					p.RuleMatches = &matches
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

func shouldSkipNoiseProcess(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "nvidia-smi", "renderroute":
		return true
	}
	return false
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

	// Resolve binary path
	bin := argv[0]
	if !filepath.IsAbs(bin) {
		if resolved, err := exec.LookPath(bin); err == nil {
			bin = resolved
		} else if exePath != "" {
			bin = exePath
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
