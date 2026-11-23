package gpu

import (
	"fmt"
	"path/filepath"
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
					return true
				}
				if !strings.HasPrefix(argv[j], "-") {
					return false
				}
			}
		}
		return false
	}
	return false
}

// ForFlatpak returns a copy of RoutingEnv safe for Flatpak sandboxes by removing
// host-filesystem Vulkan/EGL manifest paths that do not exist inside the sandbox.
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
