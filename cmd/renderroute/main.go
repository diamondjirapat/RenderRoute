package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/diamondjirapat/RenderRoute/internal/config"
	"github.com/diamondjirapat/RenderRoute/internal/desktop"
	"github.com/diamondjirapat/RenderRoute/internal/dms"
	"github.com/diamondjirapat/RenderRoute/internal/gpu"
	"github.com/diamondjirapat/RenderRoute/internal/proc"
)

const version = "1.0.0"

// StatusPayload is the unified JSON payload consumed by the DankMaterialShell widget plugin.
type StatusPayload struct {
	Version          string            `json:"version"`
	DefaultGPU       string            `json:"default_gpu"`
	DefaultGPUID     string            `json:"default_gpu_id,omitempty"`
	DefaultGPUShort  string            `json:"default_gpu_short,omitempty"`
	DGPUActive       bool              `json:"dgpu_active"`
	DGPUProcessCount int               `json:"dgpu_process_count"`
	TotalProcesses   int               `json:"total_processes"`
	GPUs             []gpu.GPU         `json:"gpus"`
	Processes        []proc.GPUProcess `json:"processes"`
	Rules            []config.Rule     `json:"rules"`
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "renderroute: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		printUsage()
		return nil
	}
	switch args[0] {
	case "-h", "--help", "help":
		printUsage()
		return nil
	case "-v", "--version", "version":
		fmt.Printf("renderroute %s\n", version)
		return nil
	case "gpus":
		return cmdGPUs(args[1:])
	case "ps":
		return cmdPS(args[1:])
	case "status":
		return cmdStatus(args[1:])
	case "env":
		return cmdEnv(args[1:])
	case "run":
		return cmdRun(args[1:])
	case "default":
		return cmdDefault(args[1:])
	case "rule", "rules":
		return cmdRule(args[1:])
	case "apps":
		return cmdApps(args[1:])
	case "launch":
		return cmdLaunch(args[1:])
	case "desktop":
		return cmdDesktop(args[1:])
	case "relaunch":
		return cmdRelaunch(args[1:])
	case "dms":
		return cmdDMS(args[1:])
	default:
		return fmt.Errorf("unknown command %q (run 'renderroute --help')", args[0])
	}
}

func printUsage() {
	fmt.Print(`RenderRoute - Linux per-process GPU routing utility & DankMaterialShell plugin backend

Usage:
  renderroute gpus [--json] [--no-metrics]
      List detected GPUs, render nodes, power state, and live telemetry.

  renderroute ps [--json]
      Show running processes currently utilizing each GPU.

  renderroute status [--json]
      Unified snapshot of GPUs, processes, rules, and defaults (used by DMS widget).

  renderroute env <gpu> [--shell|--json]
      Print environment variables needed to route a process to <gpu>.
      <gpu> can be: igpu, dgpu, auto, default, 0, 1, card0, nvidia, intel, amd, or PCI slot.

  renderroute run <gpu|--auto> [--detach] [--] <command> [args...]
      Execute a command bound to the specified GPU (or auto-select from saved rules).

  renderroute default [gpu] [--json]
      Show or set the default GPU selector.

  renderroute rule list [--json]
  renderroute rule set <target> <gpu> [--desktop-id <id>] [--name <label>] [--apply-desktop]
  renderroute rule remove <target> [--reset-desktop]
      Manage persistent per-process / per-app GPU rules.

  renderroute apps [--search <query>] [--json]
      List installed .desktop applications and their RenderRoute GPU assignments.

  renderroute launch <app-id-or-name> [--gpu <gpu>] [--json]
      Launch a desktop application on a specific GPU (or its rule/default GPU).

  renderroute desktop apply <app-id> <gpu>
  renderroute desktop reset <app-id>
      Patch/reset ~/.local/share/applications/<app-id>.desktop to route via RenderRoute.

  renderroute relaunch <pid> <gpu> [--force] [--json]
      Gracefully restart an existing user process on a different GPU.

  renderroute dms install [--symlink] [--source <dir>]
  renderroute dms uninstall | reload | status
      Install or manage the DankMaterialShell widget plugin.
`)
}

func cmdGPUs(args []string) error {
	asJSON := false
	metrics := true
	for _, a := range args {
		switch a {
		case "--json":
			asJSON = true
		case "--no-metrics":
			metrics = false
		default:
			return fmt.Errorf("unknown flag %q", a)
		}
	}

	gpus, err := gpu.Discover(metrics)
	if err != nil {
		return err
	}
	if asJSON {
		return writeJSON(gpus)
	}
	if len(gpus) == 0 {
		fmt.Println("No GPUs detected.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "IDX\tROLE\tCARD\tRENDER\tPCI\tDRIVER\tPOWER\tTEMP\tVRAM\tNAME")
	for _, g := range gpus {
		temp := "-"
		if g.TemperatureC != nil {
			temp = fmt.Sprintf("%.0f°C", *g.TemperatureC)
		}
		vram := "-"
		if g.VRAMUsedMB != nil && g.VRAMTotalMB != nil {
			vram = fmt.Sprintf("%.0f/%.0fMB", *g.VRAMUsedMB, *g.VRAMTotalMB)
		}
		render := g.RenderNode
		if render == "" {
			render = "-"
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			g.Index, g.Role, g.Card, render, g.PCISlot, g.Driver, g.PowerState, temp, vram, g.Name)
	}
	return w.Flush()
}

func cmdPS(args []string) error {
	asJSON := false
	for _, a := range args {
		if a == "--json" {
			asJSON = true
		} else {
			return fmt.Errorf("unknown flag %q", a)
		}
	}
	gpus, err := gpu.Discover(false)
	if err != nil {
		return err
	}
	cfg, _ := config.Load()
	procs := proc.ListGPUProcesses(gpus, cfg)
	if asJSON {
		if procs == nil {
			procs = []proc.GPUProcess{}
		}
		return writeJSON(procs)
	}
	if len(procs) == 0 {
		fmt.Println("No active GPU processes detected.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PID\tPROCESS\tGPU\tROLE\tVRAM\tRULE")
	for _, p := range procs {
		vram := "-"
		if p.VRAMUsedMB != nil {
			vram = fmt.Sprintf("%.0fMB", *p.VRAMUsedMB)
		}
		ruleStr := "-"
		if p.RuleGPU != "" {
			ruleStr = p.RuleGPU
			if p.RuleMatches != nil && !*p.RuleMatches {
				ruleStr += " (mismatch)"
			}
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n", p.PID, p.Name, p.GPUShort, p.GPURole, vram, ruleStr)
	}
	return w.Flush()
}

func cmdStatus(args []string) error {
	asJSON := false
	for _, a := range args {
		if a == "--json" {
			asJSON = true
		} else {
			return fmt.Errorf("unknown flag %q", a)
		}
	}

	gpus, err := gpu.Discover(true)
	if err != nil {
		return err
	}
	cfg, _ := config.Load()
	procs := proc.ListGPUProcesses(gpus, cfg)
	if procs == nil {
		procs = []proc.GPUProcess{}
	}

	counts := map[string]int{}
	dgpuProcs := 0
	for _, p := range procs {
		counts[p.GPUID]++
		if p.GPURole == gpu.RoleDiscrete {
			dgpuProcs++
		}
	}
	dgpuActive := false
	for i := range gpus {
		gpus[i].ActiveProcCount = counts[gpus[i].ID]
		if gpus[i].Role == gpu.RoleDiscrete && (gpus[i].PowerState == "active" || gpus[i].ActiveProcCount > 0) {
			dgpuActive = true
		}
	}

	defSel := cfg.DefaultGPU
	defID := ""
	defShort := defSel
	if resolved, err := resolveWithConfig(gpus, cfg, defSel); err == nil {
		defID = resolved.ID
		defShort = resolved.ShortName
	}

	payload := StatusPayload{
		Version:          version,
		DefaultGPU:       defSel,
		DefaultGPUID:     defID,
		DefaultGPUShort:  defShort,
		DGPUActive:       dgpuActive,
		DGPUProcessCount: dgpuProcs,
		TotalProcesses:   len(procs),
		GPUs:             gpus,
		Processes:        procs,
		Rules:            cfg.SortedRules(),
	}
	if payload.GPUs == nil {
		payload.GPUs = []gpu.GPU{}
	}
	if payload.Rules == nil {
		payload.Rules = []config.Rule{}
	}

	if asJSON {
		return writeJSON(payload)
	}

	fmt.Printf("Default GPU: %s (%s)\n", payload.DefaultGPUShort, payload.DefaultGPU)
	fmt.Printf("GPUs: %d | Active GPU processes: %d (dGPU: %d, dGPU awake: %v) | Saved rules: %d\n",
		len(payload.GPUs), payload.TotalProcesses, payload.DGPUProcessCount, payload.DGPUActive, len(payload.Rules))
	return nil
}

func cmdEnv(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: renderroute env <gpu> [--shell|--json]")
	}
	selector := ""
	asJSON := false
	asShell := false
	for _, a := range args {
		switch a {
		case "--json":
			asJSON = true
		case "--shell":
			asShell = true
		default:
			if selector == "" {
				selector = a
			} else {
				return fmt.Errorf("unexpected argument %q", a)
			}
		}
	}
	if selector == "" {
		return errors.New("missing GPU selector")
	}

	gpus, err := gpu.Discover(false)
	if err != nil {
		return err
	}
	cfg, _ := config.Load()
	target, err := resolveWithConfig(gpus, cfg, selector)
	if err != nil {
		return err
	}
	env := gpu.BuildRoutingEnv(*target, gpus)
	if asJSON {
		return writeJSON(env)
	}
	if asShell || !asJSON {
		fmt.Print(env.FormatShell())
	}
	return nil
}

func cmdRun(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: renderroute run <gpu|--auto> [--detach] [--] <command> [args...]")
	}

	selector := ""
	autoMode := false
	detach := false
	var cmdArgs []string

	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			cmdArgs = args[i+1:]
			break
		}
		if selector == "" && !autoMode {
			switch a {
			case "--auto", "auto":
				autoMode = true
				continue
			case "--detach", "-d":
				detach = true
				continue
			default:
				if strings.HasPrefix(a, "-") {
					return fmt.Errorf("unknown flag %q", a)
				}
				selector = a
				continue
			}
		}
		if a == "--detach" || a == "-d" {
			detach = true
			continue
		}
		cmdArgs = args[i:]
		break
	}

	if len(cmdArgs) == 0 {
		return errors.New("missing command to run (e.g. renderroute run dgpu -- glxgears)")
	}

	gpus, err := gpu.Discover(false)
	if err != nil {
		return err
	}
	cfg, _ := config.Load()

	var extraEnv map[string]string
	if autoMode || strings.EqualFold(selector, "auto") {
		if r, ok := cfg.MatchRule(cmdArgs[0], filepath.Base(cmdArgs[0])); ok {
			selector = r.GPU
			extraEnv = r.ExtraEnv
		} else {
			selector = cfg.DefaultGPU
		}
	} else {
		if r, ok := cfg.MatchRule(cmdArgs[0], filepath.Base(cmdArgs[0])); ok {
			extraEnv = r.ExtraEnv
		}
	}

	target, err := resolveWithConfig(gpus, cfg, selector)
	if err != nil {
		return err
	}

	binPath, err := exec.LookPath(cmdArgs[0])
	if err != nil {
		return fmt.Errorf("executable %q not found in PATH: %w", cmdArgs[0], err)
	}

	routing := gpu.BuildRoutingEnv(*target, gpus)
	finalEnv := routing.Apply(os.Environ(), extraEnv)

	if detach {
		cmd := exec.Command(binPath, cmdArgs[1:]...)
		cmd.Env = finalEnv
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
		if err == nil {
			cmd.Stdin = devNull
			cmd.Stdout = devNull
			cmd.Stderr = devNull
			defer devNull.Close()
		}
		if err := cmd.Start(); err != nil {
			return err
		}
		pid := cmd.Process.Pid
		_ = cmd.Process.Release()
		fmt.Printf("Launched %s (pid %d) on %s (%s)\n", cmdArgs[0], pid, target.ShortName, target.ID)
		return nil
	}

	// Replace current process with target executable directly (zero wrapper overhead)
	return syscall.Exec(binPath, cmdArgs, finalEnv)
}

func cmdDefault(args []string) error {
	asJSON := false
	newSel := ""
	for _, a := range args {
		if a == "--json" {
			asJSON = true
		} else if newSel == "" {
			newSel = a
		} else {
			return fmt.Errorf("unexpected argument %q", a)
		}
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	gpus, err := gpu.Discover(false)
	if err != nil {
		return err
	}

	if newSel != "" {
		resolved, err := gpu.Resolve(gpus, newSel)
		if err != nil {
			return err
		}
		// Persist canonical role ("igpu"/"dgpu") if user passed role, else stable PCI ID
		low := strings.ToLower(newSel)
		if low == "igpu" || low == "dgpu" {
			cfg.DefaultGPU = low
		} else {
			cfg.DefaultGPU = resolved.ID
		}
		if err := cfg.Save(); err != nil {
			return err
		}
	}

	resolved, _ := resolveWithConfig(gpus, cfg, cfg.DefaultGPU)
	if asJSON {
		out := map[string]any{
			"default_gpu": cfg.DefaultGPU,
			"gpu":         resolved,
		}
		return writeJSON(out)
	}
	if resolved != nil {
		fmt.Printf("Default GPU: %s -> %s [%s, %s]\n", cfg.DefaultGPU, resolved.Name, resolved.Role, resolved.ID)
	} else {
		fmt.Printf("Default GPU: %s\n", cfg.DefaultGPU)
	}
	return nil
}

func cmdRule(args []string) error {
	if len(args) == 0 {
		args = []string{"list"}
	}
	switch args[0] {
	case "list", "ls":
		asJSON := false
		for _, a := range args[1:] {
			if a == "--json" {
				asJSON = true
			}
		}
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		rules := cfg.SortedRules()
		if asJSON {
			return writeJSON(rules)
		}
		if len(rules) == 0 {
			fmt.Println("No GPU rules configured.")
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "TARGET\tGPU\tDESKTOP ID\tPATCHED")
		for _, r := range rules {
			did := r.DesktopID
			if did == "" {
				did = "-"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%v\n", r.Target, r.GPU, did, r.DesktopPatched)
		}
		return w.Flush()

	case "set", "add":
		var pos []string
		desktopID := ""
		displayName := ""
		applyDesktop := false
		for i := 1; i < len(args); i++ {
			switch args[i] {
			case "--desktop-id":
				if i+1 >= len(args) {
					return errors.New("--desktop-id requires a value")
				}
				i++
				desktopID = args[i]
			case "--name":
				if i+1 >= len(args) {
					return errors.New("--name requires a value")
				}
				i++
				displayName = args[i]
			case "--apply-desktop":
				applyDesktop = true
			default:
				pos = append(pos, args[i])
			}
		}
		if len(pos) < 2 {
			return errors.New("usage: renderroute rule set <target> <gpu> [--desktop-id <id>] [--apply-desktop]")
		}
		targetName := pos[0]
		gpuSel := pos[1]

		gpus, err := gpu.Discover(false)
		if err != nil {
			return err
		}
		resolved, err := gpu.Resolve(gpus, gpuSel)
		if err != nil {
			return err
		}

		storedSel := strings.ToLower(gpuSel)
		if storedSel != "igpu" && storedSel != "dgpu" {
			storedSel = resolved.ID
		}

		cfg, err := config.Load()
		if err != nil {
			return err
		}

		patched := false
		if applyDesktop {
			if desktopID == "" {
				apps := desktop.DiscoverApps(cfg, "")
				if found, err := desktop.FindApp(apps, targetName); err == nil {
					desktopID = found.ID
					if displayName == "" {
						displayName = found.Name
					}
				}
			}
			if desktopID != "" {
				binPath := selfBinaryForDesktop()
				if _, err := desktop.ApplyDesktopOverride(desktopID, storedSel, binPath, resolved.Role); err == nil {
					patched = true
				}
			}
		}

		cfg.SetRule(config.Rule{
			Target:         targetName,
			DisplayName:    displayName,
			GPU:            storedSel,
			DesktopID:      desktopID,
			DesktopPatched: patched,
		})
		if err := cfg.Save(); err != nil {
			return err
		}
		fmt.Printf("Rule saved: %s -> %s (%s)", config.NormalizeKey(targetName), storedSel, resolved.ShortName)
		if patched {
			fmt.Printf(" [desktop override applied: %s.desktop]", desktopID)
		}
		fmt.Println()
		return nil

	case "remove", "rm", "delete":
		resetDesktop := true
		target := ""
		for _, a := range args[1:] {
			switch a {
			case "--no-reset-desktop":
				resetDesktop = false
			case "--reset-desktop":
				resetDesktop = true
			default:
				if target == "" {
					target = a
				}
			}
		}
		if target == "" {
			return errors.New("usage: renderroute rule remove <target>")
		}
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		removed, ok := cfg.RemoveRule(target)
		if !ok {
			return fmt.Errorf("no rule found for %q", target)
		}
		if err := cfg.Save(); err != nil {
			return err
		}
		if resetDesktop && removed.DesktopID != "" {
			_, _ = desktop.ResetDesktopOverride(removed.DesktopID)
		}
		fmt.Printf("Removed rule for %s\n", removed.Target)
		return nil

	default:
		return fmt.Errorf("unknown rule subcommand %q (expected list, set, remove)", args[0])
	}
}

func cmdApps(args []string) error {
	asJSON := false
	query := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			asJSON = true
		case "--search", "-s":
			if i+1 >= len(args) {
				return errors.New("--search requires a query")
			}
			i++
			query = args[i]
		default:
			if query == "" && !strings.HasPrefix(args[i], "-") {
				query = args[i]
			} else {
				return fmt.Errorf("unknown flag %q", args[i])
			}
		}
	}

	cfg, _ := config.Load()
	apps := desktop.DiscoverApps(cfg, query)
	if asJSON {
		if apps == nil {
			apps = []desktop.App{}
		}
		return writeJSON(apps)
	}
	if len(apps) == 0 {
		fmt.Println("No matching desktop applications found.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tBINARY\tGPU RULE")
	for _, a := range apps {
		g := a.EffectiveGPU
		if g == "" {
			g = "-"
		} else if a.PatchedGPU != "" {
			g += " (desktop)"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", a.ID, a.Name, a.Binary, g)
	}
	return w.Flush()
}

func cmdLaunch(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: renderroute launch <app-id-or-name> [--gpu <gpu>] [--json]")
	}
	appQuery := ""
	gpuSel := ""
	asJSON := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--gpu", "-g":
			if i+1 >= len(args) {
				return errors.New("--gpu requires a value")
			}
			i++
			gpuSel = args[i]
		case "--json":
			asJSON = true
		default:
			if appQuery == "" {
				appQuery = args[i]
			} else {
				return fmt.Errorf("unexpected argument %q", args[i])
			}
		}
	}
	if appQuery == "" {
		return errors.New("missing application name or ID")
	}

	cfg, _ := config.Load()
	apps := desktop.DiscoverApps(cfg, "")
	app, err := desktop.FindApp(apps, appQuery)
	if err != nil {
		return err
	}

	gpus, err := gpu.Discover(false)
	if err != nil {
		return err
	}

	var extraEnv map[string]string
	if r, ok := cfg.MatchRule(app.ID, app.Binary); ok {
		if gpuSel == "" {
			gpuSel = r.GPU
		}
		extraEnv = r.ExtraEnv
	}
	if gpuSel == "" {
		gpuSel = cfg.DefaultGPU
	}

	target, err := resolveWithConfig(gpus, cfg, gpuSel)
	if err != nil {
		return err
	}

	pid, err := desktop.LaunchApp(*app, *target, gpus, extraEnv)
	if err != nil {
		return err
	}

	if asJSON {
		return writeJSON(map[string]any{
			"app_id":   app.ID,
			"app_name": app.Name,
			"pid":      pid,
			"gpu_id":   target.ID,
			"gpu_name": target.Name,
		})
	}
	fmt.Printf("Launched %s (pid %d) on %s (%s)\n", app.Name, pid, target.ShortName, target.ID)
	return nil
}

func cmdDesktop(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: renderroute desktop <apply <app-id> <gpu> | reset <app-id>>")
	}
	switch args[0] {
	case "apply":
		if len(args) < 3 {
			return errors.New("usage: renderroute desktop apply <app-id> <gpu>")
		}
		appID := args[1]
		gpuSel := args[2]
		gpus, err := gpu.Discover(false)
		if err != nil {
			return err
		}
		resolved, err := gpu.Resolve(gpus, gpuSel)
		if err != nil {
			return err
		}
		storedSel := strings.ToLower(gpuSel)
		if storedSel != "igpu" && storedSel != "dgpu" {
			storedSel = resolved.ID
		}
		dst, err := desktop.ApplyDesktopOverride(appID, storedSel, selfBinaryForDesktop(), resolved.Role)
		if err != nil {
			return err
		}
		fmt.Printf("Patched desktop launcher: %s (GPU -> %s)\n", dst, storedSel)
		return nil

	case "reset":
		appID := args[1]
		changed, err := desktop.ResetDesktopOverride(appID)
		if err != nil {
			return err
		}
		if changed {
			fmt.Printf("Reset RenderRoute override for %s.desktop\n", strings.TrimSuffix(appID, ".desktop"))
		} else {
			fmt.Printf("No RenderRoute override found for %s.desktop\n", strings.TrimSuffix(appID, ".desktop"))
		}
		return nil

	default:
		return fmt.Errorf("unknown desktop subcommand %q (expected apply or reset)", args[0])
	}
}

func cmdRelaunch(args []string) error {
	var pos []string
	force := false
	asJSON := false
	for _, a := range args {
		switch a {
		case "--force", "-f":
			force = true
		case "--json":
			asJSON = true
		default:
			pos = append(pos, a)
		}
	}
	if len(pos) < 2 {
		return errors.New("usage: renderroute relaunch <pid> <gpu> [--force] [--json]")
	}
	pid, err := strconv.Atoi(pos[0])
	if err != nil {
		return fmt.Errorf("invalid PID %q", pos[0])
	}
	gpus, err := gpu.Discover(false)
	if err != nil {
		return err
	}
	cfg, _ := config.Load()
	target, err := resolveWithConfig(gpus, cfg, pos[1])
	if err != nil {
		return err
	}
	res, err := proc.RelaunchProcess(pid, *target, gpus, nil, force)
	if err != nil {
		return err
	}
	if asJSON {
		return writeJSON(res)
	}
	fmt.Printf("Relaunched pid %d -> new pid %d on %s (%s)\n", res.OldPID, res.NewPID, res.GPUName, res.GPUID)
	return nil
}

func cmdDMS(args []string) error {
	if len(args) == 0 {
		args = []string{"status"}
	}
	switch args[0] {
	case "install":
		useSymlink := false
		srcDir := ""
		for i := 1; i < len(args); i++ {
			switch args[i] {
			case "--symlink", "-s":
				useSymlink = true
			case "--source":
				if i+1 >= len(args) {
					return errors.New("--source requires a directory path")
				}
				i++
				srcDir = args[i]
			}
		}
		dst, err := dms.Install(srcDir, useSymlink)
		if err != nil {
			return err
		}
		fmt.Printf("Installed DankMaterialShell plugin to %s\n", dst)
		if out, err := dms.Reload(); err == nil {
			if out != "" {
				fmt.Println(out)
			}
			fmt.Println("Reloaded plugin in DankMaterialShell.")
		} else {
			fmt.Println("Tip: Open DMS Settings -> Plugins -> Scan for plugins, and enable RenderRoute.")
		}
		return nil

	case "uninstall":
		dst, err := dms.Uninstall()
		if err != nil {
			return err
		}
		fmt.Printf("Removed DankMaterialShell plugin at %s\n", dst)
		return nil

	case "reload":
		out, err := dms.Reload()
		if err != nil {
			return fmt.Errorf("reload DMS plugin: %w (%s)", err, out)
		}
		if out != "" {
			fmt.Println(out)
		} else {
			fmt.Println("Reloaded RenderRoute plugin in DankMaterialShell.")
		}
		return nil

	case "status":
		dst, err := dms.PluginTargetDir()
		if err != nil {
			return err
		}
		info, err := os.Lstat(dst)
		if err != nil {
			fmt.Printf("DankMaterialShell plugin: NOT installed (%s)\n", dst)
			return nil
		}
		mode := "directory"
		if info.Mode()&os.ModeSymlink != 0 {
			link, _ := os.Readlink(dst)
			mode = "symlink -> " + link
		}
		fmt.Printf("DankMaterialShell plugin: installed at %s (%s)\n", dst, mode)
		return nil

	default:
		return fmt.Errorf("unknown dms subcommand %q (expected install, uninstall, reload, status)", args[0])
	}
}

func resolveWithConfig(gpus []gpu.GPU, cfg *config.Config, selector string) (*gpu.GPU, error) {
	if selector == "" || strings.EqualFold(selector, "default") {
		if cfg != nil && cfg.DefaultGPU != "" {
			selector = cfg.DefaultGPU
		}
	}
	g, err := gpu.Resolve(gpus, selector)
	if err != nil && (selector == "igpu" || selector == "dgpu") && len(gpus) > 0 {
		// Graceful fallback on single-GPU machines when default is "igpu"/"dgpu"
		return &gpus[0], nil
	}
	return g, err
}

func selfBinaryForDesktop() string {
	if p, err := exec.LookPath("renderroute"); err == nil {
		return p
	}
	if exe, err := os.Executable(); err == nil {
		return exe
	}
	return "renderroute"
}

func writeJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
