package embed_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RidgetopAi/backstory/internal/embed"
)

// The two cards #19 saw on the Human's machine, in this boot's order: the
// iGPU is Vulkan0 and the RX 7800 XT is Vulkan1.
const (
	iGPU = "Vulkan0|AMD Ryzen 5 7600X 6-Core Processor (RADV RAPHAEL_MENDOCINO)|0000:13:00.0"
	dGPU = "Vulkan1|AMD Radeon RX 7800 XT (RADV NAVI32)|0000:03:00.0"
)

// stubRuntime is testdata/llama-embedding's absolute path.
func stubRuntime(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("testdata", "llama-embedding"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// stubDevices makes the stub list these ARG|DESC|PCI devices.
func stubDevices(t *testing.T, devices ...string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "devices")
	if err := os.WriteFile(p, []byte(strings.Join(devices, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STUB_DEVICES", p)
}

// stubModel is a stand-in model file.
func stubModel(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "embeddinggemma-2-Q8_0.gguf")
	if err := os.WriteFile(p, []byte("GGUF stand-in"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func resolve(t *testing.T, match string) (embed.Device, error) {
	t.Helper()
	return embed.ResolveDevice(context.Background(), embed.Setup{Runtime: stubRuntime(t), Model: stubModel(t), Device: match})
}

func TestListDevicesReadsTheRuntimesList(t *testing.T) {
	stubDevices(t, iGPU, dGPU)
	devs, err := embed.ListDevices(context.Background(), stubRuntime(t))
	if err != nil {
		t.Fatal(err)
	}
	want := []embed.Device{
		{Arg: "Vulkan0", Name: "AMD Ryzen 5 7600X 6-Core Processor (RADV RAPHAEL_MENDOCINO)", Backend: "vulkan"},
		{Arg: "Vulkan1", Name: "AMD Radeon RX 7800 XT (RADV NAVI32)", Backend: "vulkan"},
	}
	if len(devs) != len(want) {
		t.Fatalf("devices = %+v, want %+v", devs, want)
	}
	for i := range want {
		if devs[i] != want[i] {
			t.Errorf("device %d = %+v, want %+v", i, devs[i], want[i])
		}
	}

	t.Setenv("STUB_DEVICES", "")
	if devs, err := embed.ListDevices(context.Background(), stubRuntime(t)); err != nil || len(devs) != 0 {
		t.Fatalf("no devices: %+v, %v", devs, err)
	}
}

func TestResolveDeviceByName(t *testing.T) {
	stubDevices(t, iGPU, dGPU)
	for _, match := range []string{
		"AMD Radeon RX 7800 XT (RADV NAVI32)", // the full name --list-devices prints
		"AMD Radeon RX 7800 XT",               // without the driver's suffix
		"amd radeon rx 7800 xt",
	} {
		d, err := resolve(t, match)
		if err != nil {
			t.Fatalf("%q: %v", match, err)
		}
		if d.Arg != "Vulkan1" || d.Backend != "vulkan" {
			t.Errorf("%q resolved to %+v, want Vulkan1", match, d)
		}
	}
}

// Vulkan indices change between boots (#19): the PCI address follows the card.
func TestResolveDeviceByPCIFollowsTheCardAcrossBoots(t *testing.T) {
	stubDevices(t, iGPU, dGPU)
	d, err := resolve(t, "0000:03:00.0")
	if err != nil {
		t.Fatal(err)
	}
	if d.Arg != "Vulkan1" || d.PCI != "0000:03:00.0" {
		t.Fatalf("resolved to %+v, want Vulkan1", d)
	}

	stubDevices(t,
		"Vulkan0|AMD Radeon RX 7800 XT (RADV NAVI32)|0000:03:00.0",
		"Vulkan1|AMD Ryzen 5 7600X 6-Core Processor (RADV RAPHAEL_MENDOCINO)|0000:13:00.0")
	for _, match := range []string{"0000:03:00.0", "03:00.0", "0000:03:00.0 "} {
		d, err := resolve(t, match)
		if err != nil {
			t.Fatalf("%q after reboot: %v", match, err)
		}
		if d.Arg != "Vulkan0" {
			t.Errorf("%q after reboot resolved to %+v, want Vulkan0", match, d)
		}
	}
}

// With HIP installed beside Vulkan, one card is listed twice. The runtime's
// backend is Vulkan (#16, #19), so the Vulkan entry wins.
func TestResolveDevicePrefersVulkanForTheSameCard(t *testing.T) {
	stubDevices(t,
		"ROCm0|AMD Radeon RX 7800 XT|0000:03:00.0",
		"ROCm1|AMD Ryzen 5 7600X 6-Core Processor|0000:13:00.0",
		iGPU, dGPU)
	for _, match := range []string{"AMD Radeon RX 7800 XT", "0000:03:00.0"} {
		d, err := resolve(t, match)
		if err != nil {
			t.Fatalf("%q: %v", match, err)
		}
		if d.Arg != "Vulkan1" {
			t.Errorf("%q resolved to %+v, want Vulkan1", match, d)
		}
	}
}

func TestResolveDeviceRefusesWithAReason(t *testing.T) {
	stubDevices(t, iGPU, dGPU,
		"Vulkan2|AMD Radeon RX 7800 XT (RADV NAVI32)|0000:05:00.0")
	for _, tc := range []struct{ match, want string }{
		{"NVIDIA GeForce RTX 4090", "no device matches"},
		{"0000:07:00.0", "no device matches"},
		{"Vulkan1", "backend index"},
		{"AMD Radeon RX 7800 XT", "matches more than one device"},
		{"", "no device"},
	} {
		_, err := resolve(t, tc.match)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: err = %v, want %q", tc.match, err, tc.want)
			continue
		}
		if tc.match != "" && tc.match != "Vulkan1" && !strings.Contains(err.Error(), "Vulkan1: AMD Radeon RX 7800 XT (RADV NAVI32)") {
			t.Errorf("%q: refusal does not list the devices: %v", tc.match, err)
		}
	}
}

func TestResolveDeviceCPU(t *testing.T) {
	stubDevices(t, iGPU, dGPU)
	d, err := resolve(t, embed.DeviceCPU)
	if err != nil {
		t.Fatal(err)
	}
	if d.Arg != "none" || d.Backend != "cpu" {
		t.Fatalf("cpu resolved to %+v", d)
	}
}
