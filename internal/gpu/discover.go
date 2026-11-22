package gpu

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Role represents whether a GPU is integrated, discrete, or unknown.
type Role string

const (
	RoleIntegrated Role = "igpu"
	RoleDiscrete   Role = "dgpu"
	RoleUnknown    Role = "unknown"
)

// GPU describes a discovered Linux DRM graphics adapter and its live telemetry.
type GPU struct {
	Index            int      `json:"index"`
	ID               string   `json:"id"`          // Stable PCI slot ID, e.g. "0000:01:00.0"
	Card             string   `json:"card"`        // e.g. "card0"
	CardPath         string   `json:"card_path"`   // e.g. "/dev/dri/card0"
	RenderNode       string   `json:"render_node"` // e.g. "renderD129"
	RenderPath       string   `json:"render_path"` // e.g. "/dev/dri/renderD129"
	PCISlot          string   `json:"pci_slot"`    // Canonical "0000:01:00.0"
	VendorID         string   `json:"vendor_id"`   // e.g. "0x10de"
	DeviceID         string   `json:"device_id"`   // e.g. "0x28e1"
	Vendor           string   `json:"vendor"`      // "nvidia", "intel", "amd", "unknown"
	Driver           string   `json:"driver"`      // e.g. "nvidia", "i915", "xe", "amdgpu"
	Name             string   `json:"name"`        // Human-readable name
	ShortName        string   `json:"short_name"`  // Compact label for bar pills
	Role             Role     `json:"role"`        // "igpu", "dgpu", "unknown"
	BootVGA          bool     `json:"boot_vga"`
	PowerState       string   `json:"power_state"` // "active", "suspended", "unknown"
	VulkanICDs       []string `json:"vulkan_icds"`
	EGLVendors       []string `json:"egl_vendors"`
	UtilizationPct   *float64 `json:"utilization_pct,omitempty"`
	VRAMUsedMB       *float64 `json:"vram_used_mb,omitempty"`
	VRAMTotalMB      *float64 `json:"vram_total_mb,omitempty"`
	TemperatureC     *float64 `json:"temperature_c,omitempty"`
	PowerWatts       *float64 `json:"power_watts,omitempty"`
	ActiveProcCount  int      `json:"active_proc_count"`
}

var cardDirRe = regexp.MustCompile(`^card([0-9]+)$`)

// NormalizePCISlot converts variants like "00000000:01:00.0" or "01:00.0" to lowercase "0000:01:00.0".
func NormalizePCISlot(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return ""
	}
	parts := strings.Split(s, ":")
	switch len(parts) {
	case 2:
		// Short form "01:00.0" -> prepend default domain "0000"
		return fmt.Sprintf("0000:%s:%s", parts[0], parts[1])
	case 3:
		dom := parts[0]
		if len(dom) > 4 {
			dom = dom[len(dom)-4:]
		} else if len(dom) < 4 {
			dom = strings.Repeat("0", 4-len(dom)) + dom
		}
		return fmt.Sprintf("%s:%s:%s", dom, parts[1], parts[2])
	default:
		return s
	}
}

// VendorFromID maps a PCI vendor hex string to a canonical vendor identifier.
func VendorFromID(vendorID string) string {
	switch strings.ToLower(strings.TrimSpace(vendorID)) {
	case "0x10de", "10de":
		return "nvidia"
	case "0x8086", "8086":
		return "intel"
	case "0x1002", "1002", "0x1022", "1022":
		return "amd"
	case "0x1af4", "1af4":
		return "virtio"
	default:
		return "unknown"
	}
}

// Discover scans /sys/class/drm, enriches with lspci, Vulkan ICDs, and live telemetry.
// Set includeMetrics=true to also gather utilization/VRAM/temp (never wakes a suspended dGPU).
func Discover(includeMetrics bool) ([]GPU, error) {
	return DiscoverFrom("/sys/class/drm", "/dev/dri", includeMetrics)
}

// DiscoverFrom scans DRM devices from custom sysfs/dev roots (useful for testing).
func DiscoverFrom(sysDRMRoot, devDRIRoot string, includeMetrics bool) ([]GPU, error) {
	entries, err := os.ReadDir(sysDRMRoot)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", sysDRMRoot, err)
	}

	renderByPCI := mapRenderNodes(sysDRMRoot, devDRIRoot)
	lspciNames := queryLspciNames()

	var gpus []GPU
	for _, e := range entries {
		name := e.Name()
		if !cardDirRe.MatchString(name) {
			continue
		}
		cardSys := filepath.Join(sysDRMRoot, name)
		devSys := filepath.Join(cardSys, "device")
		if _, err := os.Stat(devSys); err != nil {
			continue
		}

		uevent := parseUevent(filepath.Join(devSys, "uevent"))
		pciSlot := NormalizePCISlot(uevent["PCI_SLOT_NAME"])
		vendorID := strings.ToLower(readTrimmed(filepath.Join(devSys, "vendor")))
		deviceID := strings.ToLower(readTrimmed(filepath.Join(devSys, "device")))
		driver := strings.ToLower(uevent["DRIVER"])
		bootVGA := readTrimmed(filepath.Join(devSys, "boot_vga")) == "1"
		powerState := readTrimmed(filepath.Join(devSys, "power", "runtime_status"))
		if powerState == "" {
			powerState = "active"
		}

		vendor := VendorFromID(vendorID)
		if vendor == "unknown" {
			switch driver {
			case "nvidia", "nouveau":
				vendor = "nvidia"
			case "i915", "xe":
				vendor = "intel"
			case "amdgpu", "radeon":
				vendor = "amd"
			}
		}

		renderNode := renderByPCI[pciSlot]
		renderPath := ""
		if renderNode != "" {
			renderPath = filepath.Join(devDRIRoot, renderNode)
		}

		gpuName := lspciNames[pciSlot]
		if gpuName == "" {
			gpuName = fallbackGPUName(vendor, driver, vendorID, deviceID)
		}

		id := pciSlot
		if id == "" {
			id = name
		}

		g := GPU{
			ID:         id,
			Card:       name,
			CardPath:   filepath.Join(devDRIRoot, name),
			RenderNode: renderNode,
			RenderPath: renderPath,
			PCISlot:    pciSlot,
			VendorID:   vendorID,
			DeviceID:   deviceID,
			Vendor:     vendor,
			Driver:     driver,
			Name:       gpuName,
			BootVGA:    bootVGA,
			PowerState: powerState,
		}
		g.VulkanICDs = discoverVulkanICDs(g)
		g.EGLVendors = discoverEGLVendors(g)
		gpus = append(gpus, g)
	}

	classifyRoles(gpus)

	// Sort deterministically: iGPU first (index 0), then dGPU(s), tie-break by PCI slot then card name.
	sort.Slice(gpus, func(i, j int) bool {
		ri, rj := roleRank(gpus[i].Role), roleRank(gpus[j].Role)
		if ri != rj {
			return ri < rj
		}
		if gpus[i].PCISlot != gpus[j].PCISlot {
			return gpus[i].PCISlot < gpus[j].PCISlot
		}
		return gpus[i].Card < gpus[j].Card
	})

	for i := range gpus {
		gpus[i].Index = i
		gpus[i].ShortName = makeShortName(gpus[i])
	}

	if includeMetrics {
		populateMetrics(gpus, sysDRMRoot)
	}

	return gpus, nil
}

func roleRank(r Role) int {
	switch r {
	case RoleIntegrated:
		return 0
	case RoleDiscrete:
		return 1
	default:
		return 2
	}
}

// classifyRoles determines igpu vs dgpu across all discovered adapters.
func classifyRoles(gpus []GPU) {
	for i := range gpus {
		g := &gpus[i]
		// Integrated GPUs typically sit on PCI bus 00:02.0 (Intel) or are boot_vga non-NVIDIA,
		// or AMD APUs with boot_vga on bus 00 / low bus.
		switch g.Vendor {
		case "nvidia":
			g.Role = RoleDiscrete
		case "intel":
			// Intel Arc discrete cards usually live on non-zero PCI bus (e.g., 03:00.0),
			// whereas Intel integrated UHD/Iris/Arc iGPU sits at 0000:00:02.0.
			if strings.HasPrefix(g.PCISlot, "0000:00:") {
				g.Role = RoleIntegrated
			} else if g.BootVGA {
				g.Role = RoleIntegrated
			} else {
				g.Role = RoleDiscrete
			}
		case "amd":
			if g.BootVGA && len(gpus) > 1 {
				g.Role = RoleIntegrated
			} else if strings.Contains(strings.ToLower(g.Name), "raphael") ||
				strings.Contains(strings.ToLower(g.Name), "phoenix") ||
				strings.Contains(strings.ToLower(g.Name), "rembrandt") ||
				strings.Contains(strings.ToLower(g.Name), "cezanne") ||
				strings.Contains(strings.ToLower(g.Name), "renoir") ||
				strings.Contains(strings.ToLower(g.Name), "vega") && g.BootVGA {
				g.Role = RoleIntegrated
			} else if g.BootVGA {
				g.Role = RoleIntegrated
			} else {
				g.Role = RoleDiscrete
			}
		default:
			if g.BootVGA {
				g.Role = RoleIntegrated
			} else {
				g.Role = RoleUnknown
			}
		}
	}

	// If there are multiple GPUs and none marked integrated, promote boot_vga.
	hasIGPU := false
	for _, g := range gpus {
		if g.Role == RoleIntegrated {
			hasIGPU = true
			break
		}
	}
	if !hasIGPU && len(gpus) > 1 {
		for i := range gpus {
			if gpus[i].BootVGA {
				gpus[i].Role = RoleIntegrated
				break
			}
		}
	}
}

func makeShortName(g GPU) string {
	lower := strings.ToLower(g.Name)
	// Extract concise model badge like "RTX 4050", "RX 7800", "Intel UHD", etc.
	reModel := regexp.MustCompile(`(?i)\b(RTX|GTX|RX|Arc|UHD|Iris|Radeon)\s*[A-Za-z0-9-]+`)
	if m := reModel.FindString(g.Name); m != "" {
		return strings.TrimSpace(m)
	}
	switch {
	case strings.Contains(lower, "nvidia"):
		return "NVIDIA"
	case strings.Contains(lower, "intel"):
		return "Intel"
	case strings.Contains(lower, "amd") || strings.Contains(lower, "radeon"):
		return "AMD"
	case g.Role == RoleIntegrated:
		return "iGPU"
	case g.Role == RoleDiscrete:
		return "dGPU"
	default:
		return g.Card
	}
}

func mapRenderNodes(sysDRMRoot, devDRIRoot string) map[string]string {
	out := make(map[string]string)

	// Strategy 1: /dev/dri/by-path/pci-<slot>-render
	byPath := filepath.Join(devDRIRoot, "by-path")
	if entries, err := os.ReadDir(byPath); err == nil {
		for _, e := range entries {
			n := e.Name()
			if !strings.HasPrefix(n, "pci-") || !strings.HasSuffix(n, "-render") {
				continue
			}
			slot := NormalizePCISlot(strings.TrimSuffix(strings.TrimPrefix(n, "pci-"), "-render"))
			target, err := os.Readlink(filepath.Join(byPath, n))
			if err != nil {
				continue
			}
			out[slot] = filepath.Base(target)
		}
	}

	// Strategy 2: fallback via /sys/class/drm/renderD*/device/uevent
	if entries, err := os.ReadDir(sysDRMRoot); err == nil {
		for _, e := range entries {
			if !strings.HasPrefix(e.Name(), "renderD") {
				continue
			}
			u := parseUevent(filepath.Join(sysDRMRoot, e.Name(), "device", "uevent"))
			slot := NormalizePCISlot(u["PCI_SLOT_NAME"])
			if slot != "" && out[slot] == "" {
				out[slot] = e.Name()
			}
		}
	}
	return out
}

func queryLspciNames() map[string]string {
	res := make(map[string]string)
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, "lspci", "-D", "-mm")
	out, err := cmd.Output()
	if err != nil {
		return res
	}
	// Format: 0000:01:00.0 "VGA compatible controller" "NVIDIA Corporation" "AD107M [GeForce RTX 4050 Max-Q / Mobile]" ...
	re := regexp.MustCompile(`^(\S+)\s+"([^"]+)"\s+"([^"]+)"\s+"([^"]+)"`)
	for _, line := range strings.Split(string(out), "\n") {
		m := re.FindStringSubmatch(strings.TrimSpace(line))
		if len(m) != 5 {
			continue
		}
		class := strings.ToLower(m[2])
		if !strings.Contains(class, "vga") && !strings.Contains(class, "3d") && !strings.Contains(class, "display") {
			continue
		}
		slot := NormalizePCISlot(m[1])
		vendor := cleanVendorName(m[3])
		device := cleanDeviceName(m[4])
		if strings.Contains(strings.ToLower(device), strings.ToLower(vendor)) {
			res[slot] = device
		} else {
			res[slot] = strings.TrimSpace(vendor + " " + device)
		}
	}
	return res
}

func cleanVendorName(v string) string {
	v = strings.ReplaceAll(v, "Corporation", "")
	v = strings.ReplaceAll(v, "Advanced Micro Devices, Inc. [AMD/ATI]", "AMD")
	v = strings.ReplaceAll(v, "Advanced Micro Devices, Inc.", "AMD")
	return strings.TrimSpace(v)
}

func cleanDeviceName(d string) string {
	// Prefer bracketed marketing name if present, e.g. "AD107M [GeForce RTX 4050 Max-Q / Mobile]" -> "GeForce RTX 4050 Max-Q / Mobile"
	if i := strings.Index(d, "["); i >= 0 {
		if j := strings.Index(d[i:], "]"); j > 1 {
			inner := strings.TrimSpace(d[i+1 : i+j])
			if len(inner) >= 4 {
				return inner
			}
		}
	}
	return strings.TrimSpace(d)
}

func fallbackGPUName(vendor, driver, vendorID, deviceID string) string {
	switch vendor {
	case "nvidia":
		return fmt.Sprintf("NVIDIA GPU (%s:%s)", vendorID, deviceID)
	case "intel":
		return fmt.Sprintf("Intel Graphics (%s:%s)", vendorID, deviceID)
	case "amd":
		return fmt.Sprintf("AMD Radeon (%s:%s)", vendorID, deviceID)
	default:
		if driver != "" {
			return fmt.Sprintf("%s GPU (%s:%s)", strings.ToUpper(driver), vendorID, deviceID)
		}
		return fmt.Sprintf("GPU (%s:%s)", vendorID, deviceID)
	}
}

func discoverVulkanICDs(g GPU) []string {
	dirs := []string{"/etc/vulkan/icd.d", "/usr/share/vulkan/icd.d"}
	var prefixes []string
	switch {
	case g.Driver == "nvidia":
		prefixes = []string{"nvidia_icd"}
	case g.Driver == "nouveau":
		prefixes = []string{"nouveau_icd"}
	case g.Vendor == "intel":
		prefixes = []string{"intel_icd", "intel_hasvk_icd"}
	case g.Vendor == "amd":
		prefixes = []string{"radeon_icd", "amd_icd"}
	case g.Vendor == "virtio":
		prefixes = []string{"virtio_icd"}
	}
	if len(prefixes) == 0 {
		return nil
	}

	seen := map[string]bool{}
	var found []string
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if !strings.HasSuffix(name, ".json") {
				continue
			}
			for _, p := range prefixes {
				if strings.HasPrefix(name, p) {
					full := filepath.Join(dir, name)
					if !seen[full] {
						seen[full] = true
						found = append(found, full)
					}
					break
				}
			}
		}
	}
	sort.Strings(found)
	return found
}

func discoverEGLVendors(g GPU) []string {
	dirs := []string{"/etc/glvnd/egl_vendor.d", "/usr/share/glvnd/egl_vendor.d"}
	var substr string
	switch {
	case g.Driver == "nvidia":
		substr = "nvidia"
	default:
		substr = "mesa"
	}
	var found []string
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".json") && strings.Contains(strings.ToLower(e.Name()), substr) {
				found = append(found, filepath.Join(dir, e.Name()))
			}
		}
	}
	sort.Strings(found)
	return found
}

func populateMetrics(gpus []GPU, sysDRMRoot string) {
	// Only invoke nvidia-smi if an NVIDIA GPU is NOT runtime-suspended (avoid waking sleeping dGPU!).
	needNvidiaSMI := false
	for _, g := range gpus {
		if g.Driver == "nvidia" && g.PowerState != "suspended" {
			needNvidiaSMI = true
			break
		}
	}
	if needNvidiaSMI {
		populateNvidiaSMIMetrics(gpus)
	}

	// Sysfs telemetry for AMD / Intel GPUs
	for i := range gpus {
		g := &gpus[i]
		if g.Driver == "nvidia" || g.PowerState == "suspended" {
			continue
		}
		devSys := filepath.Join(sysDRMRoot, g.Card, "device")

		if v, err := readFloatFile(filepath.Join(devSys, "gpu_busy_percent")); err == nil {
			g.UtilizationPct = &v
		}
		if used, err := readFloatFile(filepath.Join(devSys, "mem_info_vram_used")); err == nil {
			mb := used / (1024 * 1024)
			g.VRAMUsedMB = &mb
		}
		if total, err := readFloatFile(filepath.Join(devSys, "mem_info_vram_total")); err == nil {
			mb := total / (1024 * 1024)
			g.VRAMTotalMB = &mb
		}
		if temp := readHwmonTempC(filepath.Join(devSys, "hwmon")); temp != nil {
			g.TemperatureC = temp
		}
		if power := readHwmonPowerW(filepath.Join(devSys, "hwmon")); power != nil {
			g.PowerWatts = power
		}
	}
}

func populateNvidiaSMIMetrics(gpus []GPU) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=pci.bus_id,name,utilization.gpu,memory.used,memory.total,temperature.gpu,power.draw",
		"--format=csv,noheader,nounits",
	)
	out, err := cmd.Output()
	if err != nil {
		return
	}
	bySlot := map[string]*GPU{}
	for i := range gpus {
		bySlot[gpus[i].PCISlot] = &gpus[i]
	}

	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		cols := strings.Split(line, ",")
		if len(cols) < 7 {
			continue
		}
		slot := NormalizePCISlot(cols[0])
		g := bySlot[slot]
		if g == nil {
			continue
		}
		if name := strings.TrimSpace(cols[1]); name != "" && !strings.HasPrefix(name, "[") {
			g.Name = name
			g.ShortName = makeShortName(*g)
		}
		if v, ok := parseOptionalFloat(cols[2]); ok {
			g.UtilizationPct = &v
		}
		if v, ok := parseOptionalFloat(cols[3]); ok {
			g.VRAMUsedMB = &v
		}
		if v, ok := parseOptionalFloat(cols[4]); ok {
			g.VRAMTotalMB = &v
		}
		if v, ok := parseOptionalFloat(cols[5]); ok {
			g.TemperatureC = &v
		}
		if v, ok := parseOptionalFloat(cols[6]); ok {
			g.PowerWatts = &v
		}
	}
}

func readHwmonTempC(hwmonDir string) *float64 {
	entries, err := os.ReadDir(hwmonDir)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if v, err := readFloatFile(filepath.Join(hwmonDir, e.Name(), "temp1_input")); err == nil {
			c := v / 1000.0
			return &c
		}
	}
	return nil
}

func readHwmonPowerW(hwmonDir string) *float64 {
	entries, err := os.ReadDir(hwmonDir)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		for _, fname := range []string{"power1_average", "power1_input"} {
			if v, err := readFloatFile(filepath.Join(hwmonDir, e.Name(), fname)); err == nil {
				w := v / 1_000_000.0
				return &w
			}
		}
	}
	return nil
}

func parseOptionalFloat(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "n/a") || strings.Contains(strings.ToLower(s), "not supported") {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func readFloatFile(path string) (float64, error) {
	s := readTrimmed(path)
	if s == "" {
		return 0, errors.New("empty")
	}
	return strconv.ParseFloat(s, 64)
}

func readTrimmed(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(bytes.TrimRight(b, "\x00")))
}

func parseUevent(path string) map[string]string {
	res := make(map[string]string)
	b, err := os.ReadFile(path)
	if err != nil {
		return res
	}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok {
			res[k] = v
		}
	}
	return res
}

// Resolve selects a GPU by flexible selector:
// index ("0", "1"), role ("igpu", "dgpu", "integrated", "discrete"),
// vendor ("nvidia", "intel", "amd"), driver ("i915", "amdgpu"),
// card ("card0"), render node ("renderD128"), PCI slot ("0000:01:00.0" or "01:00.0"),
// or case-insensitive name substring.
func Resolve(gpus []GPU, selector string) (*GPU, error) {
	if len(gpus) == 0 {
		return nil, errors.New("no GPUs detected on this system")
	}
	sel := strings.TrimSpace(selector)
	if sel == "" {
		return &gpus[0], nil
	}
	low := strings.ToLower(sel)

	// 1. Numeric index
	if idx, err := strconv.Atoi(low); err == nil {
		for i := range gpus {
			if gpus[i].Index == idx {
				return &gpus[i], nil
			}
		}
		return nil, fmt.Errorf("GPU index %d out of range (0..%d)", idx, len(gpus)-1)
	}

	// 2. Role aliases
	switch low {
	case "igpu", "integrated", "internal":
		for i := range gpus {
			if gpus[i].Role == RoleIntegrated {
				return &gpus[i], nil
			}
		}
		return nil, errors.New("no integrated GPU found")
	case "dgpu", "discrete", "dedicated", "external":
		for i := range gpus {
			if gpus[i].Role == RoleDiscrete {
				return &gpus[i], nil
			}
		}
		return nil, errors.New("no discrete GPU found")
	}

	// 3. Exact PCI slot, ID, card, or render node
	normPCI := NormalizePCISlot(low)
	for i := range gpus {
		g := &gpus[i]
		if strings.EqualFold(g.ID, sel) ||
			(g.PCISlot != "" && g.PCISlot == normPCI) ||
			strings.EqualFold(g.Card, sel) ||
			strings.EqualFold(g.CardPath, sel) ||
			(g.RenderNode != "" && (strings.EqualFold(g.RenderNode, sel) || strings.EqualFold(g.RenderPath, sel))) {
			return g, nil
		}
	}

	// 4. Vendor or driver exact match
	for i := range gpus {
		g := &gpus[i]
		if strings.EqualFold(g.Vendor, low) || strings.EqualFold(g.Driver, low) {
			return g, nil
		}
	}

	// 5. Name substring match
	for i := range gpus {
		g := &gpus[i]
		if strings.Contains(strings.ToLower(g.Name), low) || strings.Contains(strings.ToLower(g.ShortName), low) {
			return g, nil
		}
	}

	return nil, fmt.Errorf("no GPU matching %q", selector)
}
