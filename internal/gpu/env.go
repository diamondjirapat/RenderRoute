package gpu

import (
	"fmt"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// RoutingEnv holds the environment changes needed to route a process to a specific GPU.
type RoutingEnv struct {
	GPU   GPU               `json:"gpu"`
	Set   map[string]string `json:"set"`
	Unset []string          `json:"unset"`
}

// BuildRoutingEnv computes OpenGL, EGL, Vulkan, VA-API, and compute environment variables
// to bind a process cleanly to the target GPU, while clearing conflicting variables from other GPUs.
func BuildRoutingEnv(target GPU, allGPUs []GPU) RoutingEnv {
	set := map[string]string{
		"RENDERROUTE_GPU":      target.ID,
		"RENDERROUTE_GPU_ROLE": string(target.Role),
		"RENDERROUTE_GPU_CARD": target.Card,
	}
	var unset []string

	if target.Driver == "nvidia" {
		// Proprietary NVIDIA PRIME Render Offload
		set["__NV_PRIME_RENDER_OFFLOAD"] = "1"
		set["__NV_PRIME_RENDER_OFFLOAD_PROVIDER"] = "NVIDIA-G0"
		set["__GLX_VENDOR_LIBRARY_NAME"] = "nvidia"
		set["__VK_LAYER_NV_optimus"] = "NVIDIA_only"

		// Determine NVIDIA CUDA device ordinal (0-based among NVIDIA GPUs)
		nvIdx := 0
		for _, g := range allGPUs {
			if g.Driver != "nvidia" {
				continue
			}
			if g.ID == target.ID {
				break
			}
			nvIdx++
		}
		set["CUDA_VISIBLE_DEVICES"] = fmt.Sprintf("%d", nvIdx)

		if len(target.VulkanICDs) > 0 {
			joined := strings.Join(target.VulkanICDs, ":")
			set["VK_DRIVER_FILES"] = joined
			set["VK_ICD_FILENAMES"] = joined
		}
		if len(target.EGLVendors) > 0 {
			set["__EGL_VENDOR_LIBRARY_FILENAMES"] = strings.Join(target.EGLVendors, ":")
		}

		// Clear Mesa-specific and AMD ROCm routing variables that might be inherited
		unset = []string{
			"DRI_PRIME",
			"HIP_VISIBLE_DEVICES",
			"LIBVA_DRIVER_NAME",
			"MESA_VK_DEVICE_SELECT",
			"ROCR_VISIBLE_DEVICES",
			"VDPAU_DRIVER",
		}
	} else {
		// Mesa-based GPUs (Intel i915/xe, AMD amdgpu/radeon, Nouveau, VirtIO)
		if target.PCISlot != "" {
			// Mesa canonical PCI-slot DRI_PRIME format: pci-0000_00_02_0
			pciPrime := "pci-" + strings.NewReplacer(":", "_", ".", "_").Replace(target.PCISlot)
			set["DRI_PRIME"] = pciPrime
		} else {
			set["DRI_PRIME"] = fmt.Sprintf("%d", target.Index)
		}

		set["__GLX_VENDOR_LIBRARY_NAME"] = "mesa"

		if len(target.VulkanICDs) > 0 {
			joined := strings.Join(target.VulkanICDs, ":")
			set["VK_DRIVER_FILES"] = joined
			set["VK_ICD_FILENAMES"] = joined
		}

		// Mesa Vulkan device-select layer strict selection ("vendor:device!")
		vHex := strings.TrimPrefix(strings.ToLower(target.VendorID), "0x")
		dHex := strings.TrimPrefix(strings.ToLower(target.DeviceID), "0x")
		if vHex != "" && dHex != "" {
			set["MESA_VK_DEVICE_SELECT"] = fmt.Sprintf("%s:%s!", vHex, dHex)
		}

		if len(target.EGLVendors) > 0 {
			set["__EGL_VENDOR_LIBRARY_FILENAMES"] = strings.Join(target.EGLVendors, ":")
		}

		// Explicitly strip NVIDIA offload variables if inherited
		unset = []string{
			"__NV_PRIME_RENDER_OFFLOAD",
			"__NV_PRIME_RENDER_OFFLOAD_PROVIDER",
			"__VK_LAYER_NV_optimus",
			"CUDA_VISIBLE_DEVICES",
		}

		switch target.Vendor {
		case "intel":
			set["LIBVA_DRIVER_NAME"] = "iHD"
			unset = append(unset, "HIP_VISIBLE_DEVICES", "ROCR_VISIBLE_DEVICES", "VDPAU_DRIVER")
		case "amd":
			set["LIBVA_DRIVER_NAME"] = "radeonsi"
			set["VDPAU_DRIVER"] = "radeonsi"
			amdIdx := 0
			for _, g := range allGPUs {
				if g.Vendor != "amd" {
					continue
				}
				if g.ID == target.ID {
					break
				}
				amdIdx++
			}
			set["ROCR_VISIBLE_DEVICES"] = fmt.Sprintf("%d", amdIdx)
			set["HIP_VISIBLE_DEVICES"] = fmt.Sprintf("%d", amdIdx)
		default:
			unset = append(unset, "HIP_VISIBLE_DEVICES", "ROCR_VISIBLE_DEVICES")
		}
	}

	sort.Strings(unset)
	return RoutingEnv{
		GPU:   target,
		Set:   set,
		Unset: unset,
	}
}

// IsFlatpakCommand reports whether argv launches an application via `flatpak run ...`
// (accounting for optional `env KEY=VAL ...` wrappers).
func IsFlatpakCommand(argv []string) bool {
	return flatpakRunIndex(argv) >= 0
}

// flatpakRunIndex returns the index of the "run" subcommand token in a `flatpak ... run ...`
// command (accounting for optional `env KEY=VAL ...` wrappers), or -1 if argv is not a flatpak run command.
func flatpakRunIndex(argv []string) int {
	for i := 0; i < len(argv); i++ {
		tok := argv[i]
		if i == 0 && filepath.Base(tok) == "env" {
			continue
		}
		if strings.Contains(tok, "=") && !strings.HasPrefix(tok, "/") {
			continue
		}
		if filepath.Base(tok) == "flatpak" {
			for j := i + 1; j < len(argv); j++ {
				if argv[j] == "run" {
					return j
				}
				if !strings.HasPrefix(argv[j], "-") {
					return -1
				}
			}
		}
		return -1
	}
	return -1
}

// ForFlatpak returns a copy of RoutingEnv safe for the host `flatpak` wrapper process
// by removing host-filesystem Vulkan/EGL manifest paths that do not exist inside the sandbox.
// Use InjectFlatpakEnvArgs to pass sandbox-valid EGL/Vulkan manifest paths via `flatpak run --env=`.
func (r RoutingEnv) ForFlatpak() RoutingEnv {
	hostOnly := []string{
		"VK_DRIVER_FILES",
		"VK_ICD_FILENAMES",
		"__EGL_VENDOR_LIBRARY_FILENAMES",
	}
	newSet := make(map[string]string, len(r.Set))
	for k, v := range r.Set {
		newSet[k] = v
	}
	unsetMap := make(map[string]bool, len(r.Unset)+len(hostOnly))
	for _, k := range r.Unset {
		unsetMap[k] = true
	}
	for _, k := range hostOnly {
		delete(newSet, k)
		unsetMap[k] = true
	}
	newUnset := make([]string, 0, len(unsetMap))
	for k := range unsetMap {
		newUnset = append(newUnset, k)
	}
	sort.Strings(newUnset)
	return RoutingEnv{
		GPU:   r.GPU,
		Set:   newSet,
		Unset: newUnset,
	}
}

// ForFlatpakSandbox returns a RoutingEnv whose EGL and Vulkan manifest paths point to the
// canonical locations inside Flatpak's freedesktop/GNOME/KDE runtime GL extensions
// (/usr/lib/<triplet>/GL/...). Flatpak strips host EGL/Vulkan path variables from the
// inherited environment, so these must be passed via `flatpak run --env=...` (see InjectFlatpakEnvArgs).
func (r RoutingEnv) ForFlatpakSandbox(arch string) RoutingEnv {
	base := r.ForFlatpak()

	newSet := make(map[string]string, len(base.Set)+3)
	for k, v := range base.Set {
		newSet[k] = v
	}
	unsetMap := make(map[string]bool, len(base.Unset))
	for _, k := range base.Unset {
		unsetMap[k] = true
	}

	if egl := flatpakEGLVendors(r.GPU, arch); len(egl) > 0 {
		newSet["__EGL_VENDOR_LIBRARY_FILENAMES"] = strings.Join(egl, ":")
		delete(unsetMap, "__EGL_VENDOR_LIBRARY_FILENAMES")
	}
	if vk := flatpakVulkanICDs(r.GPU, arch); len(vk) > 0 {
		joined := strings.Join(vk, ":")
		newSet["VK_DRIVER_FILES"] = joined
		newSet["VK_ICD_FILENAMES"] = joined
		delete(unsetMap, "VK_DRIVER_FILES")
		delete(unsetMap, "VK_ICD_FILENAMES")
	}

	newUnset := make([]string, 0, len(unsetMap))
	for k := range unsetMap {
		newUnset = append(newUnset, k)
	}
	sort.Strings(newUnset)
	return RoutingEnv{
		GPU:   r.GPU,
		Set:   newSet,
		Unset: newUnset,
	}
}

// InjectFlatpakEnvArgs rewrites a `flatpak run ...` command to explicitly pass `--unset-env=`
// and `--env=` flags with Flatpak-sandbox-valid EGL/Vulkan manifest paths. This is required
// because `flatpak run` strips `__EGL_VENDOR_LIBRARY_FILENAMES`, `VK_DRIVER_FILES`, and
// `VK_ICD_FILENAMES` from the host environment, which otherwise lets libglvnd default to
// `10_nvidia.json` even when an app is pinned to an integrated GPU.
func InjectFlatpakEnvArgs(argv []string, env RoutingEnv, extra map[string]string) []string {
	runIdx := flatpakRunIndex(argv)
	if runIdx < 0 {
		return argv
	}

	arch := flatpakArchFromArgv(argv[runIdx+1:])
	sandboxEnv := env.ForFlatpakSandbox(arch)

	mergedSet := make(map[string]string, len(sandboxEnv.Set)+len(extra))
	for k, v := range sandboxEnv.Set {
		mergedSet[k] = v
	}
	for k, v := range extra {
		mergedSet[k] = v
	}

	var injected []string
	for _, k := range sandboxEnv.Unset {
		if _, overridden := mergedSet[k]; overridden {
			continue
		}
		injected = append(injected, "--unset-env="+k)
	}

	setKeys := make([]string, 0, len(mergedSet))
	for k := range mergedSet {
		setKeys = append(setKeys, k)
	}
	sort.Strings(setKeys)
	for _, k := range setKeys {
		injected = append(injected, fmt.Sprintf("--env=%s=%s", k, mergedSet[k]))
	}

	out := make([]string, 0, len(argv)+len(injected))
	out = append(out, argv[:runIdx+1]...)
	out = append(out, injected...)
	out = append(out, argv[runIdx+1:]...)
	return out
}

func flatpakArchFromArgv(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		if strings.HasPrefix(a, "--arch=") {
			return strings.TrimPrefix(a, "--arch=")
		}
		if a == "--arch" && i+1 < len(args) {
			return args[i+1]
		}
		if !strings.HasPrefix(a, "-") {
			break
		}
	}
	return ""
}

func flatpakTripletAndArch(arch string) (triplet, vkArch string, include32 bool) {
	switch strings.ToLower(strings.TrimSpace(arch)) {
	case "x86_64", "amd64":
		return "x86_64-linux-gnu", "x86_64", true
	case "aarch64", "arm64":
		return "aarch64-linux-gnu", "aarch64", false
	case "i386", "i686", "386":
		return "i386-linux-gnu", "i686", false
	case "":
		switch runtime.GOARCH {
		case "arm64":
			return "aarch64-linux-gnu", "aarch64", false
		case "386":
			return "i386-linux-gnu", "i686", false
		default:
			return "x86_64-linux-gnu", "x86_64", true
		}
	default:
		return arch + "-linux-gnu", arch, false
	}
}

func flatpakEGLVendors(g GPU, arch string) []string {
	triplet, _, include32 := flatpakTripletAndArch(arch)
	manifest := "50_mesa.json"
	if g.Driver == "nvidia" {
		manifest = "10_nvidia.json"
	}
	paths := []string{fmt.Sprintf("/usr/lib/%s/GL/glvnd/egl_vendor.d/%s", triplet, manifest)}
	if include32 {
		paths = append(paths, fmt.Sprintf("/usr/lib/i386-linux-gnu/GL/glvnd/egl_vendor.d/%s", manifest))
	}
	return paths
}

func flatpakVulkanICDs(g GPU, arch string) []string {
	triplet, vkArch, include32 := flatpakTripletAndArch(arch)
	var names64, names32 []string
	switch {
	case g.Driver == "nvidia":
		names64 = []string{"nvidia_icd.json"}
		names32 = []string{"nvidia_icd.json"}
	case g.Driver == "nouveau":
		names64 = []string{fmt.Sprintf("nouveau_icd.%s.json", vkArch)}
		names32 = []string{"nouveau_icd.i686.json"}
	case g.Vendor == "intel":
		names64 = []string{
			fmt.Sprintf("intel_icd.%s.json", vkArch),
			fmt.Sprintf("intel_hasvk_icd.%s.json", vkArch),
		}
		names32 = []string{
			"intel_icd.i686.json",
			"intel_hasvk_icd.i686.json",
		}
	case g.Vendor == "amd":
		names64 = []string{fmt.Sprintf("radeon_icd.%s.json", vkArch)}
		names32 = []string{"radeon_icd.i686.json"}
	case g.Vendor == "virtio":
		names64 = []string{fmt.Sprintf("virtio_icd.%s.json", vkArch)}
		names32 = []string{"virtio_icd.i686.json"}
	default:
		return nil
	}

	paths := make([]string, 0, len(names64)+len(names32))
	for _, n := range names64 {
		paths = append(paths, fmt.Sprintf("/usr/lib/%s/GL/vulkan/icd.d/%s", triplet, n))
	}
	if include32 {
		for _, n := range names32 {
			paths = append(paths, fmt.Sprintf("/usr/lib/i386-linux-gnu/GL/vulkan/icd.d/%s", n))
		}
	}
	return paths
}

// Apply merges RoutingEnv into a base os.Environ()-style slice, applying Unset then Set.
func (r RoutingEnv) Apply(base []string, extra map[string]string) []string {
	unsetSet := make(map[string]bool, len(r.Unset))
	for _, k := range r.Unset {
		unsetSet[k] = true
	}

	merged := make(map[string]string, len(base)+len(r.Set)+len(extra))
	for _, entry := range base {
		k, v, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if unsetSet[k] {
			continue
		}
		merged[k] = v
	}
	for k, v := range r.Set {
		merged[k] = v
	}
	for k, v := range extra {
		merged[k] = v
	}

	keys := make([]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+merged[k])
	}
	return out
}

// FormatShell formats the routing environment as POSIX shell export/unset statements.
func (r RoutingEnv) FormatShell() string {
	var b strings.Builder
	for _, k := range r.Unset {
		fmt.Fprintf(&b, "unset %s\n", k)
	}
	keys := make([]string, 0, len(r.Set))
	for k := range r.Set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "export %s=%s\n", k, shellQuote(r.Set[k]))
	}
	return b.String()
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
