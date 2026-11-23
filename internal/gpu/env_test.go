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

func TestInjectFlatpakEnvArgs(t *testing.T) {
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
		EGLVendors: []string{"/usr/share/glvnd/egl_vendor.d/50_mesa.json"},
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
	all := []GPU{intel, nvidia}

	// Targeting Intel iGPU on x86_64 Flatpak must inject sandbox Mesa EGL + Intel Vulkan ICDs and unset NVIDIA offload vars
	igpuRouting := BuildRoutingEnv(intel, all)
	orig := []string{"/usr/bin/flatpak", "run", "--branch=stable", "--arch=x86_64", "app.zen_browser.zen"}
	got := InjectFlatpakEnvArgs(orig, igpuRouting, map[string]string{"EXTRA_VAR": "1"})

	if got[0] != "/usr/bin/flatpak" || got[1] != "run" {
		t.Fatalf("unexpected prefix: %v", got[:2])
	}
	if got[len(got)-1] != "app.zen_browser.zen" {
		t.Fatalf("expected app ID preserved at end, got %v", got)
	}
	joined := strings.Join(got, "\n")
	for _, want := range []string{
		"--env=__EGL_VENDOR_LIBRARY_FILENAMES=/usr/lib/x86_64-linux-gnu/GL/glvnd/egl_vendor.d/50_mesa.json:/usr/lib/i386-linux-gnu/GL/glvnd/egl_vendor.d/50_mesa.json",
		"--env=VK_DRIVER_FILES=/usr/lib/x86_64-linux-gnu/GL/vulkan/icd.d/intel_icd.x86_64.json:/usr/lib/x86_64-linux-gnu/GL/vulkan/icd.d/intel_hasvk_icd.x86_64.json:/usr/lib/i386-linux-gnu/GL/vulkan/icd.d/intel_icd.i686.json:/usr/lib/i386-linux-gnu/GL/vulkan/icd.d/intel_hasvk_icd.i686.json",
		"--env=DRI_PRIME=pci-0000_00_02_0",
		"--env=EXTRA_VAR=1",
		"--unset-env=__NV_PRIME_RENDER_OFFLOAD",
		"--unset-env=CUDA_VISIBLE_DEVICES",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing expected flatpak arg %q in:\n%s", want, joined)
		}
	}

	// Targeting NVIDIA dGPU on aarch64 Flatpak must inject aarch64 nvidia_icd.json and 10_nvidia.json
	nvRouting := BuildRoutingEnv(nvidia, all)
	gotNV := InjectFlatpakEnvArgs([]string{"flatpak", "run", "--arch=aarch64", "dev.vencord.Vesktop"}, nvRouting, nil)
	joinedNV := strings.Join(gotNV, "\n")
	for _, want := range []string{
		"--env=__EGL_VENDOR_LIBRARY_FILENAMES=/usr/lib/aarch64-linux-gnu/GL/glvnd/egl_vendor.d/10_nvidia.json",
		"--env=VK_DRIVER_FILES=/usr/lib/aarch64-linux-gnu/GL/vulkan/icd.d/nvidia_icd.json",
		"--env=__NV_PRIME_RENDER_OFFLOAD=1",
		"--unset-env=DRI_PRIME",
	} {
		if !strings.Contains(joinedNV, want) {
			t.Errorf("missing expected NVIDIA flatpak arg %q in:\n%s", want, joinedNV)
		}
	}

	// Non-flatpak command must be returned unchanged
	native := []string{"/usr/bin/dolphin", "%u"}
	gotNative := InjectFlatpakEnvArgs(native, igpuRouting, nil)
	if len(gotNative) != len(native) || gotNative[0] != native[0] {
		t.Errorf("non-flatpak argv was modified: %v", gotNative)
	}
}

func TestFlatpakAppIDAndZedNoEscape(t *testing.T) {
	cases := []struct {
		argv []string
		want string
	}{
		{
			argv: []string{"/usr/bin/flatpak", "run", "--branch=stable", "--arch=x86_64", "--command=zed-wrapper", "--file-forwarding", "dev.zed.Zed", "@@u", "%U", "@@"},
			want: "dev.zed.Zed",
		},
		{
			argv: []string{"env", "FOO=1", "flatpak", "run", "--command", "startvesktop", "dev.vencord.Vesktop/x86_64/stable"},
			want: "dev.vencord.Vesktop",
		},
		{
			argv: []string{"/usr/bin/dolphin", "%u"},
			want: "",
		},
	}
	for _, tc := range cases {
		if got := FlatpakAppID(tc.argv); got != tc.want {
			t.Errorf("FlatpakAppID(%v) = %q, want %q", tc.argv, got, tc.want)
		}
	}

	intel := GPU{
		Index:    0,
		ID:       "0000:00:02.0",
		Card:     "card1",
		PCISlot:  "0000:00:02.0",
		VendorID: "0x8086",
		DeviceID: "0xa78b",
		Vendor:   "intel",
		Driver:   "i915",
		Role:     RoleIntegrated,
	}
	routing := BuildRoutingEnv(intel, []GPU{intel})

	// Zed Flatpak must receive ZED_FLATPAK_NO_ESCAPE=1 so it stays inside the sandbox where GPU env applies
	zedArgv := []string{"/usr/bin/flatpak", "run", "--branch=stable", "--arch=x86_64", "--command=zed-wrapper", "dev.zed.Zed"}
	gotZed := strings.Join(InjectFlatpakEnvArgs(zedArgv, routing, nil), "\n")
	if !strings.Contains(gotZed, "--env=ZED_FLATPAK_NO_ESCAPE=1") {
		t.Errorf("expected --env=ZED_FLATPAK_NO_ESCAPE=1 for dev.zed.Zed, got:\n%s", gotZed)
	}

	// Explicit extra_env override must win over default ZED_FLATPAK_NO_ESCAPE=1
	gotOverride := strings.Join(InjectFlatpakEnvArgs(zedArgv, routing, map[string]string{"ZED_FLATPAK_NO_ESCAPE": "0"}), "\n")
	if !strings.Contains(gotOverride, "--env=ZED_FLATPAK_NO_ESCAPE=0") {
		t.Errorf("expected explicit override ZED_FLATPAK_NO_ESCAPE=0 to win, got:\n%s", gotOverride)
	}

	// Non-Zed Flatpak must NOT receive ZED_FLATPAK_NO_ESCAPE
	zenArgv := []string{"/usr/bin/flatpak", "run", "--arch=x86_64", "app.zen_browser.zen"}
	gotZen := strings.Join(InjectFlatpakEnvArgs(zenArgv, routing, nil), "\n")
	if strings.Contains(gotZen, "ZED_FLATPAK_NO_ESCAPE") {
		t.Errorf("unexpected ZED_FLATPAK_NO_ESCAPE for non-Zed flatpak:\n%s", gotZen)
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
