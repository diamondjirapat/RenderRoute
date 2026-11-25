package gpu

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClassifyRolesDoesNotTreatBootVGADiscreteAsIntegrated(t *testing.T) {
	cases := []struct {
		name  string
		gpus  []GPU
		roles []Role
	}{
		{
			name:  "single AMD dGPU as boot VGA",
			gpus:  []GPU{{Vendor: "amd", PCISlot: "0000:03:00.0", DeviceID: "0x747e", BootVGA: true, Name: "Radeon RX 7700 XT / 7800 XT"}},
			roles: []Role{RoleDiscrete},
		},
		{
			name: "AMD dGPU boot VGA + Raphael APU",
			gpus: []GPU{
				{Vendor: "amd", PCISlot: "0000:03:00.0", DeviceID: "0x747e", BootVGA: true, Name: "Radeon RX 7700 XT / 7800 XT"},
				{Vendor: "amd", PCISlot: "0000:10:00.0", DeviceID: "0x164e", Name: "Raphael"},
			},
			roles: []Role{RoleDiscrete, RoleIntegrated},
		},
		{
			name: "AMD dGPU + APU identified only by bracketed marketing name",
			gpus: []GPU{
				{Vendor: "amd", PCISlot: "0000:65:00.0", DeviceID: "0xffff", BootVGA: true, Name: "AMD Radeon 780M"},
				{Vendor: "amd", PCISlot: "0000:03:00.0", DeviceID: "0x73df", Name: "Radeon RX 6700 XT"},
			},
			roles: []Role{RoleIntegrated, RoleDiscrete},
		},
		{
			name: "AMD dGPU boot VGA + Intel iGPU",
			gpus: []GPU{
				{Vendor: "amd", PCISlot: "0000:03:00.0", DeviceID: "0x747e", BootVGA: true, Name: "Radeon RX 7800 XT"},
				{Vendor: "intel", PCISlot: "0000:00:02.0", Name: "Intel UHD 770"},
			},
			roles: []Role{RoleDiscrete, RoleIntegrated},
		},
		{
			name:  "Intel Arc dGPU as boot VGA without iGPU",
			gpus:  []GPU{{Vendor: "intel", PCISlot: "0000:03:00.0", BootVGA: true, Name: "Intel Arc A770"}},
			roles: []Role{RoleDiscrete},
		},
		{
			name: "laptop Intel iGPU + NVIDIA dGPU",
			gpus: []GPU{
				{Vendor: "intel", PCISlot: "0000:00:02.0", BootVGA: true, Name: "Intel UHD"},
				{Vendor: "nvidia", PCISlot: "0000:01:00.0", Name: "RTX 4050"},
			},
			roles: []Role{RoleIntegrated, RoleDiscrete},
		},
	}
	for _, tc := range cases {
		classifyRoles(tc.gpus)
		for i, want := range tc.roles {
			if tc.gpus[i].Role != want {
				t.Errorf("%s: GPU %d (%s) role = %s, want %s", tc.name, i, tc.gpus[i].Name, tc.gpus[i].Role, want)
			}
		}
	}
}

func TestMakeShortNamePrefersSpecificModel(t *testing.T) {
	cases := map[string]string{
		"Radeon RX 7700 XT / 7800 XT":     "RX 7700",
		"AMD Radeon 780M":                 "Radeon 780M",
		"NVIDIA GeForce RTX 4050 Max-Q":   "RTX 4050",
		"Intel UHD Graphics 770":          "UHD Graphics",
		"Intel Arc A770":                  "Arc A770",
		"Radeon Vega Series / Radeon Veg": "Radeon Vega",
	}
	for in, want := range cases {
		if got := makeShortName(GPU{Name: in}); got != want {
			t.Errorf("makeShortName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildRoutingEnvCUDAOrderAndClearsMissingManifests(t *testing.T) {
	nv := GPU{ID: "0000:01:00.0", PCISlot: "0000:01:00.0", Vendor: "nvidia", Driver: "nvidia", Role: RoleDiscrete}
	intel := GPU{ID: "0000:00:02.0", PCISlot: "0000:00:02.0", Vendor: "intel", Driver: "i915", Role: RoleIntegrated, VendorID: "0x8086", DeviceID: "0xa78b"}
	all := []GPU{intel, nv}

	nvEnv := BuildRoutingEnv(nv, all)
	if nvEnv.Set["CUDA_DEVICE_ORDER"] != "PCI_BUS_ID" || nvEnv.Set["CUDA_VISIBLE_DEVICES"] != "0" {
		t.Errorf("expected PCI-ordered CUDA device 0, got %+v", nvEnv.Set)
	}

	// Parent pinned to NVIDIA leaked its manifests; an iGPU target without discovered manifests must clear them.
	applied := toMap(BuildRoutingEnv(intel, all).Apply([]string{
		"VK_DRIVER_FILES=/usr/share/vulkan/icd.d/nvidia_icd.json",
		"VK_ICD_FILENAMES=/usr/share/vulkan/icd.d/nvidia_icd.json",
		"__EGL_VENDOR_LIBRARY_FILENAMES=/usr/share/glvnd/egl_vendor.d/10_nvidia.json",
		"CUDA_DEVICE_ORDER=PCI_BUS_ID",
	}, nil))
	for _, k := range []string{"VK_DRIVER_FILES", "VK_ICD_FILENAMES", "__EGL_VENDOR_LIBRARY_FILENAMES", "CUDA_DEVICE_ORDER"} {
		if _, ok := applied[k]; ok {
			t.Errorf("%s should be cleared for iGPU target without its own manifests", k)
		}
	}
}

func TestROCmIndexFromKFDTopology(t *testing.T) {
	root := t.TempDir()
	write := func(node, props string) {
		dir := filepath.Join(root, node)
		_ = os.MkdirAll(dir, 0o755)
		_ = os.WriteFile(filepath.Join(dir, "properties"), []byte(props), 0o644)
	}
	// node 0: CPU; node 1: dGPU at 03:00.0 (location_id 0x300 = 768); node 2: APU at c1:00.0 (0xc100 = 49408)
	write("0", "cpu_cores_count 16\nsimd_count 0\nlocation_id 0\ndomain 0\n")
	write("1", "simd_count 240\nlocation_id 768\ndomain 0\n")
	write("2", "simd_count 24\nlocation_id 49408\ndomain 0\n")
	old := kfdTopologyRoot
	kfdTopologyRoot = root
	defer func() { kfdTopologyRoot = old }()

	apu := GPU{ID: "0000:c1:00.0", PCISlot: "0000:c1:00.0", Vendor: "amd", Driver: "amdgpu", Role: RoleIntegrated}
	dgpu := GPU{ID: "0000:03:00.0", PCISlot: "0000:03:00.0", Vendor: "amd", Driver: "amdgpu", Role: RoleDiscrete}
	all := []GPU{apu, dgpu} // discovery order (iGPU first) differs from KFD order

	if got := BuildRoutingEnv(dgpu, all).Set["ROCR_VISIBLE_DEVICES"]; got != "0" {
		t.Errorf("dGPU ROCR_VISIBLE_DEVICES = %q, want 0 (KFD order)", got)
	}
	if got := BuildRoutingEnv(apu, all).Set["HIP_VISIBLE_DEVICES"]; got != "1" {
		t.Errorf("APU HIP_VISIBLE_DEVICES = %q, want 1 (KFD order)", got)
	}

	// Unknown to KFD: must not guess an ordinal; clear inherited values instead.
	other := GPU{ID: "0000:05:00.0", PCISlot: "0000:05:00.0", Vendor: "amd", Driver: "amdgpu", Role: RoleDiscrete}
	env := BuildRoutingEnv(other, append(all, other))
	if _, ok := env.Set["ROCR_VISIBLE_DEVICES"]; ok {
		t.Errorf("must not set ROCR_VISIBLE_DEVICES for a GPU missing from KFD topology")
	}
	applied := toMap(env.Apply([]string{"ROCR_VISIBLE_DEVICES=1"}, nil))
	if _, ok := applied["ROCR_VISIBLE_DEVICES"]; ok {
		t.Errorf("inherited ROCR_VISIBLE_DEVICES should be cleared")
	}
}
