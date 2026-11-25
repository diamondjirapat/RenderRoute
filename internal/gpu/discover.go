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

// GPUWarning describes a PCI graphics adapter that is visible on the bus but has no
// active DRM device in /sys/class/drm (for example, when a kernel driver probe or
// firmware load failed).
type GPUWarning struct {
	PCISlot  string `json:"pci_slot"`
	VendorID string `json:"vendor_id,omitempty"`
	DeviceID string `json:"device_id,omitempty"`
	Vendor   string `json:"vendor,omitempty"`
	Driver   string `json:"driver,omitempty"`
	Name     string `json:"name"`
	Role     Role   `json:"role"`
	Reason   string `json:"reason"`
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
		populateMetrics(gpus, sysDRMRoot, nil)
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
		// Integrated GPUs: Intel iGPUs sit on PCI bus 00 (00:02.0); AMD APUs are identified by
		// device ID / name; unknown vendors fall back to boot_vga.
		switch g.Vendor {
		case "nvidia":
			g.Role = RoleDiscrete
		case "intel":
			// Intel Arc discrete cards usually live on non-zero PCI bus (e.g., 03:00.0),
			// whereas Intel integrated UHD/Iris/Arc iGPU sits at 0000:00:02.0.
			// boot_vga is not used here: an Arc card is the boot VGA device on desktops without an iGPU.
			if strings.HasPrefix(g.PCISlot, "0000:00:") {
				g.Role = RoleIntegrated
			} else if g.PCISlot == "" && g.BootVGA {
				g.Role = RoleIntegrated
			} else {
				g.Role = RoleDiscrete
			}
		case "amd":
			// boot_vga is NOT a reliable iGPU signal for AMD: desktop firmware usually selects the
			// discrete card as boot VGA. Identify APUs by PCI device ID or APU/iGPU naming instead.
			if isAMDAPU(g.DeviceID, g.Name) {
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

	// If there are multiple GPUs and none marked integrated, promote boot_vga
	// (only for adapters whose role could not be determined from vendor-specific signals).
	hasIGPU := false
	for _, g := range gpus {
		if g.Role == RoleIntegrated {
			hasIGPU = true
			break
		}
	}
	if !hasIGPU && len(gpus) > 1 {
		for i := range gpus {
			if gpus[i].BootVGA && gpus[i].Role == RoleUnknown {
				gpus[i].Role = RoleIntegrated
				break
			}
		}
	}
}

// amdAPUDeviceIDs lists PCI device IDs of AMD APU integrated graphics (Raven through Strix/Krackan).
var amdAPUDeviceIDs = map[string]bool{
	"15dd": true, // Raven Ridge / Picasso
	"15d8": true, // Picasso / Raven2
	"1636": true, // Renoir
	"1638": true, // Cezanne
	"164c": true, // Lucienne
	"15e7": true, // Barcelo
	"163f": true, // Van Gogh
	"1681": true, // Rembrandt
	"1506": true, // Mendocino
	"164e": true, // Raphael
	"13c0": true, // Granite Ridge
	"15bf": true, // Phoenix1
	"15c8": true, // Phoenix2
	"1900": true, // Phoenix3 / Hawk Point
	"1901": true, // Phoenix4 / Hawk Point
	"150e": true, // Strix Point
	"1586": true, // Strix Halo
	"1114": true, // Krackan Point
}

var amdAPUNameRe = regexp.MustCompile(`(?i)\b(raven|picasso|renoir|cezanne|lucienne|barcelo|van ?gogh|rembrandt|mendocino|raphael|granite ridge|phoenix\d*|hawk ?point|strix|krackan)\b|\bradeon (?:\d{3,4}m|\d{4}s|graphics|vega (?:series|mobile)|vega \d+ graphics)\b`)

// isAMDAPU reports whether an AMD display adapter is an APU's integrated GPU.
func isAMDAPU(deviceID, name string) bool {
	id := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(deviceID)), "0x")
	if amdAPUDeviceIDs[id] {
		return true
	}
	return amdAPUNameRe.MatchString(name)
}

var (
	// Specific model badges ("RTX 4050", "RX 7800", "Arc A770", "UHD 770", "Iris Xe") take precedence
	// over the generic "Radeon <x>" badge so "Radeon RX 7800 XT" becomes "RX 7800" rather than "Radeon RX".
	shortModelRe  = regexp.MustCompile(`(?i)\b(RTX|GTX|RX|Arc|UHD|Iris)\s*[A-Za-z0-9-]+`)
	shortRadeonRe = regexp.MustCompile(`(?i)\bRadeon\s*[A-Za-z0-9-]+`)
)

func makeShortName(g GPU) string {
	lower := strings.ToLower(g.Name)
	if m := shortModelRe.FindString(g.Name); m != "" {
		return strings.TrimSpace(m)
	}
	if m := shortRadeonRe.FindString(g.Name); m != "" {
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

// PopulateMetrics gathers live telemetry for already-discovered GPUs.
//
// nvidiaInUse limits nvidia-smi queries to NVIDIA GPUs (by ID) that have known active clients.
// Every nvidia-smi invocation initializes NVML and resets the runtime-PM autosuspend timer, so
// polling an awake-but-idle dGPU would keep it from ever suspending again. Pass nil to query
// every awake NVIDIA GPU (appropriate for one-shot, user-initiated commands).
func PopulateMetrics(gpus []GPU, nvidiaInUse map[string]bool) {
	populateMetrics(gpus, "/sys/class/drm", nvidiaInUse)
}

func populateMetrics(gpus []GPU, sysDRMRoot string, nvidiaInUse map[string]bool) {
	// Only invoke nvidia-smi for NVIDIA GPUs that are NOT runtime-suspended (avoid waking sleeping dGPU!).
	var nvSlots []string
	for _, g := range gpus {
		if g.Driver != "nvidia" || g.PowerState == "suspended" || g.PCISlot == "" {
			continue
		}
		if nvidiaInUse != nil && !nvidiaInUse[g.ID] {
			continue
		}
		nvSlots = append(nvSlots, g.PCISlot)
	}
	if len(nvSlots) > 0 {
		populateNvidiaSMIMetrics(gpus, nvSlots)
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

// NvidiaSMITimeout bounds each nvidia-smi invocation so a hung driver cannot stall status polling.
const NvidiaSMITimeout = 1500 * time.Millisecond

func populateNvidiaSMIMetrics(gpus []GPU, slots []string) {
	ctx, cancel := context.WithTimeout(context.Background(), NvidiaSMITimeout)
	defer cancel()
	// Restrict the query to the given GPUs so other (possibly suspended) NVIDIA GPUs are not woken.
	cmd := exec.CommandContext(ctx, "nvidia-smi",
		"--id="+strings.Join(slots, ","),
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

// DetectUnboundPCIGPUs scans /sys/bus/pci/devices for display-class PCI adapters that
// do not have a corresponding DRM device in discovered (never wakes suspended GPUs).
func DetectUnboundPCIGPUs(discovered []GPU) []GPUWarning {
	return DetectUnboundPCIGPUsFrom("/sys/bus/pci/devices", discovered, nil)
}

// DetectUnboundPCIGPUsFrom scans a custom PCI sysfs root for display-class devices missing
// from discovered. If lspciNames is nil, lspci is queried once only when an unbound GPU is found.
func DetectUnboundPCIGPUsFrom(sysPCIRoot string, discovered []GPU, lspciNames map[string]string) []GPUWarning {
	entries, err := os.ReadDir(sysPCIRoot)
	if err != nil {
		return nil
	}

	knownSlots := make(map[string]bool, len(discovered))
	for _, g := range discovered {
		if g.PCISlot != "" {
			knownSlots[NormalizePCISlot(g.PCISlot)] = true
		}
	}

	var warnings []GPUWarning
	for _, e := range entries {
		slot := NormalizePCISlot(e.Name())
		if slot == "" || knownSlots[slot] {
			continue
		}
		devSys := filepath.Join(sysPCIRoot, e.Name())
		pciClass := strings.ToLower(readTrimmed(filepath.Join(devSys, "class")))
		// PCI base class 0x03 = Display controller (VGA 0x0300xx, 3D 0x0302xx, Display 0x0380xx)
		if !strings.HasPrefix(pciClass, "0x03") {
			continue
		}

		if lspciNames == nil {
			lspciNames = queryLspciNames()
		}

		uevent := parseUevent(filepath.Join(devSys, "uevent"))
		vendorID := strings.ToLower(readTrimmed(filepath.Join(devSys, "vendor")))
		deviceID := strings.ToLower(readTrimmed(filepath.Join(devSys, "device")))
		driver := strings.ToLower(uevent["DRIVER"])
		bootVGA := readTrimmed(filepath.Join(devSys, "boot_vga")) == "1"

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

		name := lspciNames[slot]
		if name == "" {
			name = fallbackGPUName(vendor, driver, vendorID, deviceID)
		}

		candidate := GPU{
			PCISlot:  slot,
			VendorID: vendorID,
			DeviceID: deviceID,
			Vendor:   vendor,
			Driver:   driver,
			Name:     name,
			BootVGA:  bootVGA,
		}
		tmp := append([]GPU(nil), discovered...)
		tmp = append(tmp, candidate)
		classifyRoles(tmp)
		role := tmp[len(tmp)-1].Role

		var reason string
		switch {
		case driver == "":
			reason = "PCI display device has no kernel driver bound and no DRM node in /sys/class/drm"
		case driver == "nvidia":
			reason = `kernel driver "nvidia" is bound on PCI but created no DRM node in /sys/class/drm (driver/firmware version mismatch, failed probe, or nvidia_drm.modeset=0; check dmesg/journalctl)`
		default:
			reason = fmt.Sprintf("kernel driver %q is bound on PCI but created no DRM node in /sys/class/drm (check dmesg/journalctl)", driver)
		}

		warnings = append(warnings, GPUWarning{
			PCISlot:  slot,
			VendorID: vendorID,
			DeviceID: deviceID,
			Vendor:   vendor,
			Driver:   driver,
			Name:     name,
			Role:     role,
			Reason:   reason,
		})
	}

	sort.Slice(warnings, func(i, j int) bool {
		return warnings[i].PCISlot < warnings[j].PCISlot
	})
	return warnings
}

// FormatResolveError enriches a Resolve failure with diagnostic details if an uninitialized
// PCI GPU exists that matches the requested selector.
func FormatResolveError(selector string, err error, warnings []GPUWarning) error {
	if err == nil || len(warnings) == 0 {
		return err
	}
	low := strings.ToLower(strings.TrimSpace(selector))
	normPCI := NormalizePCISlot(low)

	var matched []GPUWarning
	for _, w := range warnings {
		switch {
		case (low == "dgpu" || low == "discrete" || low == "dedicated" || low == "external") && w.Role == RoleDiscrete:
			matched = append(matched, w)
		case (low == "igpu" || low == "integrated" || low == "internal") && w.Role == RoleIntegrated:
			matched = append(matched, w)
		case normPCI != "" && w.PCISlot == normPCI:
			matched = append(matched, w)
		case strings.EqualFold(w.Vendor, low) || strings.EqualFold(w.Driver, low):
			matched = append(matched, w)
		case low != "" && strings.Contains(strings.ToLower(w.Name), low):
			matched = append(matched, w)
		}
	}
	if len(matched) == 0 {
		matched = warnings
	}

	parts := make([]string, 0, len(matched))
	for _, w := range matched {
		parts = append(parts, fmt.Sprintf("%s [%s]: %s", w.Name, w.PCISlot, w.Reason))
	}
	return fmt.Errorf("%w (uninitialized PCI GPU detected: %s)", err, strings.Join(parts, "; "))
}
