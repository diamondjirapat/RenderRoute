package gpu

import (
	"strings"
	"testing"
)

func TestBuildRoutingEnvNvidiaAndMesaConflictClearing(t *testing.T) {
	intel := GPU{
		Index:      0,
		ID:         "0000:00:02.0",
		Card:       "card1",
		PCISlot:    "0000:00:02.0",
		VendorID:   "0x8086",
		DeviceID:   "0xa78b",
		Vendor:     "intel",
		Driver:     "i915",
		Role:       RoleIntegrated,
		VulkanICDs: []string{"/usr/share/vulkan/icd.d/intel_icd.x86_64.json"},
	}
	nvidia := GPU{
		Index:      1,
		ID:         "0000:01:00.0",
		Card:       "card0",
		PCISlot:    "0000:01:00.0",
		VendorID:   "0x10de",
		DeviceID:   "0x28e1",
		Vendor:     "nvidia",
		Driver:     "nvidia",
		Role:       RoleDiscrete,
		VulkanICDs: []string{"/usr/share/vulkan/icd.d/nvidia_icd.x86_64.json"},
	}
	all := []GPU{intel, nvidia}

	// Test targeting NVIDIA when base env had Mesa vars
	nvEnv := BuildRoutingEnv(nvidia, all)
	if nvEnv.Set["__NV_PRIME_RENDER_OFFLOAD"] != "1" {
		t.Errorf("expected __NV_PRIME_RENDER_OFFLOAD=1")
	}
	if nvEnv.Set["__GLX_VENDOR_LIBRARY_NAME"] != "nvidia" {
		t.Errorf("expected __GLX_VENDOR_LIBRARY_NAME=nvidia")
	}
	appliedNV := nvEnv.Apply([]string{"PATH=/usr/bin", "DRI_PRIME=1", "MESA_VK_DEVICE_SELECT=8086:a78b!"}, nil)
	envMapNV := toMap(appliedNV)
	if _, exists := envMapNV["DRI_PRIME"]; exists {
		t.Errorf("DRI_PRIME should have been unset for NVIDIA target")
	}
	if _, exists := envMapNV["MESA_VK_DEVICE_SELECT"]; exists {
		t.Errorf("MESA_VK_DEVICE_SELECT should have been unset for NVIDIA target")
	}
	if envMapNV["VK_DRIVER_FILES"] != "/usr/share/vulkan/icd.d/nvidia_icd.x86_64.json" {
		t.Errorf("unexpected VK_DRIVER_FILES: %q", envMapNV["VK_DRIVER_FILES"])
	}

	// Test targeting Intel when base env had NVIDIA offload vars
	igpuEnv := BuildRoutingEnv(intel, all)
	if igpuEnv.Set["DRI_PRIME"] != "pci-0000_00_02_0" {
		t.Errorf("unexpected DRI_PRIME: %q", igpuEnv.Set["DRI_PRIME"])
	}
	if igpuEnv.Set["MESA_VK_DEVICE_SELECT"] != "8086:a78b!" {
		t.Errorf("unexpected MESA_VK_DEVICE_SELECT: %q", igpuEnv.Set["MESA_VK_DEVICE_SELECT"])
	}
	appliedIGPU := igpuEnv.Apply([]string{"__NV_PRIME_RENDER_OFFLOAD=1", "__VK_LAYER_NV_optimus=NVIDIA_only", "ROCR_VISIBLE_DEVICES=0", "HIP_VISIBLE_DEVICES=0", "VDPAU_DRIVER=radeonsi"}, map[string]string{"CUSTOM_FLAG": "yes"})
	envMapIGPU := toMap(appliedIGPU)
	if _, exists := envMapIGPU["__NV_PRIME_RENDER_OFFLOAD"]; exists {
		t.Errorf("__NV_PRIME_RENDER_OFFLOAD should have been unset for Intel target")
	}
	for _, k := range []string{"ROCR_VISIBLE_DEVICES", "HIP_VISIBLE_DEVICES", "VDPAU_DRIVER"} {
		if _, exists := envMapIGPU[k]; exists {
			t.Errorf("%s should have been unset for Intel target", k)
		}
	}
	if envMapIGPU["CUSTOM_FLAG"] != "yes" {
		t.Errorf("extra env was not preserved")
	}

	// Test NVIDIA target also clears inherited ROCm/HIP variables
	appliedNV2 := nvEnv.Apply([]string{"ROCR_VISIBLE_DEVICES=0", "HIP_VISIBLE_DEVICES=0"}, nil)
	envMapNV2 := toMap(appliedNV2)
	for _, k := range []string{"ROCR_VISIBLE_DEVICES", "HIP_VISIBLE_DEVICES"} {
		if _, exists := envMapNV2[k]; exists {
			t.Errorf("%s should have been unset for NVIDIA target", k)
		}
	}
}

func TestFlatpakDetectionAndEnvSanitization(t *testing.T) {
	if !IsFlatpakCommand([]string{"/usr/bin/flatpak", "run", "--branch=stable", "dev.zed.Zed"}) {
		t.Errorf("expected flatpak run to be detected")
	}
	if !IsFlatpakCommand([]string{"env", "FOO=bar", "flatpak", "run", "com.obsproject.Studio"}) {
		t.Errorf("expected env-wrapped flatpak run to be detected")
	}
	if IsFlatpakCommand([]string{"flatpak", "list"}) {
		t.Errorf("flatpak list should not be detected as flatpak run")
	}
	if IsFlatpakCommand([]string{"/usr/bin/blender"}) {
		t.Errorf("native binary should not be detected as flatpak run")
	}

	nvidia := GPU{
		Index:      1,
		ID:         "0000:01:00.0",
		Card:       "card0",
		PCISlot:    "0000:01:00.0",
		VendorID:   "0x10de",
		DeviceID:   "0x28e1",
		Vendor:     "nvidia",
		Driver:     "nvidia",
		Role:       RoleDiscrete,
		VulkanICDs: []string{"/usr/share/vulkan/icd.d/nvidia_icd.x86_64.json"},
		EGLVendors: []string{"/usr/share/glvnd/egl_vendor.d/10_nvidia.json"},
	}

	fpEnv := BuildRoutingEnv(nvidia, []GPU{nvidia}).ForFlatpak()
	applied := toMap(fpEnv.Apply([]string{
		"VK_DRIVER_FILES=/usr/share/vulkan/icd.d/intel_icd.x86_64.json",
		"VK_ICD_FILENAMES=/usr/share/vulkan/icd.d/intel_icd.x86_64.json",
		"__EGL_VENDOR_LIBRARY_FILENAMES=/usr/share/glvnd/egl_vendor.d/50_mesa.json",
	}, nil))

	for _, hostKey := range []string{"VK_DRIVER_FILES", "VK_ICD_FILENAMES", "__EGL_VENDOR_LIBRARY_FILENAMES"} {
		if _, exists := applied[hostKey]; exists {
			t.Errorf("ForFlatpak() should strip host path %s", hostKey)
		}
	}
	if applied["__NV_PRIME_RENDER_OFFLOAD"] != "1" || applied["__GLX_VENDOR_LIBRARY_NAME"] != "nvidia" {
		t.Errorf("ForFlatpak() should keep portable NVIDIA offload variables, got %+v", applied)
	}
}

func toMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, e := range env {
		if k, v, ok := strings.Cut(e, "="); ok {
			m[k] = v
		}
	}
	return m
}
