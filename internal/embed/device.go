package embed

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// DeviceCPU is the embed.device match for embedding on the CPU, which only
// `embed --on --allow-cpu` stores.
const DeviceCPU = "cpu"

// Device is one device the runtime lists.
type Device struct {
	// Arg is what -dev takes ("Vulkan1"). It is a backend index, which
	// changes between boots (#19), so it is never stored.
	Arg string
	// Name is the description --list-devices prints.
	Name string
	// PCI is the device's PCI address, when known: --list-devices doesn't
	// print it, so only a PCI match or a run's log line fills it in.
	PCI string
	// Backend is Arg's backend in lower case ("vulkan", "rocm", "cpu"),
	// part of the fingerprint.
	Backend string
}

// cpuDevice is the device for DeviceCPU.
var cpuDevice = Device{Arg: "none", Name: "CPU", Backend: "cpu"}

// preferredBackend wins when one card is listed by several backends, as with
// HIP installed beside Vulkan: Vulkan is the runtime's backend (#16, #19).
const preferredBackend = "vulkan"

var (
	listedDeviceLine = regexp.MustCompile(`^\s+([A-Za-z_]+[0-9]+): (.*) \([0-9]+ MiB, [0-9]+ MiB free\)$`)
	usingDeviceLine  = regexp.MustCompile(`using device (\S+) \((.*)\) \(([^()]*)\) - `)
	backendIndex     = regexp.MustCompile(`^[A-Za-z_]+[0-9]+$`)
	pciAddress       = regexp.MustCompile(`^(?:([0-9a-f]{4}):)?([0-9a-f]{2}:[0-9a-f]{2}\.[0-7])$`)
)

// backendOf is "Vulkan1"'s backend, "vulkan".
func backendOf(arg string) string {
	return strings.ToLower(strings.TrimRight(arg, "0123456789"))
}

// ListDevices runs `runtime --list-devices` and parses its list.
func ListDevices(ctx context.Context, runtime string) ([]Device, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, runtime, "--list-devices") //nolint:gosec // runtime is the Human's chosen llama-embedding
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("embed: %s --list-devices: %w%s", runtime, err, errorLines(stderr.Bytes()))
	}
	var devs []Device
	sc := bufio.NewScanner(&stdout)
	for sc.Scan() {
		if m := listedDeviceLine.FindStringSubmatch(sc.Text()); m != nil {
			devs = append(devs, Device{Arg: m[1], Name: m[2], Backend: backendOf(m[1])})
		}
	}
	return devs, sc.Err()
}

// DeviceList is devs as the indented lines a refusal shows.
func DeviceList(devs []Device) string {
	if len(devs) == 0 {
		return "  (none)"
	}
	lines := make([]string, len(devs))
	for i, d := range devs {
		lines[i] = "  " + d.Arg + ": " + d.Name
		if d.PCI != "" {
			lines[i] += " (" + d.PCI + ")"
		}
	}
	return strings.Join(lines, "\n")
}

// normalPCI is match as a full lower-case PCI address, or "" if it isn't one.
func normalPCI(match string) string {
	m := pciAddress.FindStringSubmatch(strings.ToLower(match))
	if m == nil {
		return ""
	}
	domain := m[1]
	if domain == "" {
		domain = "0000"
	}
	return domain + ":" + m[2]
}

// nameMatches reports whether match names d: its full name, or its name
// without the driver's trailing " (...)", ignoring case.
func nameMatches(d Device, match string) bool {
	if strings.EqualFold(d.Name, match) {
		return true
	}
	base, _, ok := strings.Cut(d.Name, " (")
	return ok && strings.EqualFold(base, match)
}

// ResolveDevice resolves s.Device, a stable match (a device name, a PCI
// address or DeviceCPU), to the device the runtime lists for it now. A PCI
// match costs one short probe run of the runtime on s.Model, because
// --list-devices doesn't print PCI addresses. It refuses, with the devices
// listed, when nothing or more than one device matches, and it refuses a
// backend index ("Vulkan1") outright.
func ResolveDevice(ctx context.Context, s Setup) (Device, error) {
	match := strings.TrimSpace(s.Device)
	switch {
	case match == "":
		return Device{}, fmt.Errorf("embed: no device given")
	case strings.EqualFold(match, DeviceCPU):
		return cpuDevice, nil
	}
	devs, err := ListDevices(ctx, s.Runtime)
	if err != nil {
		return Device{}, err
	}
	if backendIndex.MatchString(match) {
		for _, d := range devs {
			if strings.EqualFold(d.Arg, match) {
				return Device{}, fmt.Errorf("embed: %q is a backend index, which changes between boots; give the device's name or PCI address instead:\n%s", match, DeviceList(devs))
			}
		}
	}
	var found []Device
	if pci := normalPCI(match); pci != "" {
		if devs, err = probePCI(ctx, s, devs); err != nil {
			return Device{}, err
		}
		for _, d := range devs {
			if d.PCI == pci {
				found = append(found, d)
			}
		}
	} else {
		for _, d := range devs {
			if nameMatches(d, match) {
				found = append(found, d)
			}
		}
	}
	if len(found) > 1 {
		var preferred []Device
		for _, d := range found {
			if d.Backend == preferredBackend {
				preferred = append(preferred, d)
			}
		}
		if len(preferred) > 0 {
			found = preferred
		}
	}
	switch len(found) {
	case 0:
		return Device{}, fmt.Errorf("embed: no device matches %q:\n%s", match, DeviceList(devs))
	case 1:
		return found[0], nil
	}
	return Device{}, fmt.Errorf("embed: %q matches more than one device; give its PCI address instead:\n%s", match, DeviceList(devs))
}

// probePCI fills in each device's PCI address from the `using device` lines
// of one run on every listed device, with no layers offloaded and an empty
// prompt (no Record text).
func probePCI(ctx context.Context, s Setup, devs []Device) ([]Device, error) {
	if len(devs) == 0 {
		return devs, nil
	}
	args := make([]string, len(devs))
	for i, d := range devs {
		args[i] = d.Arg
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, s.Runtime, //nolint:gosec // runtime and model are the Human's chosen files
		"-m", s.Model, "-dev", strings.Join(args, ","), "-ngl", "0", "-fit", "off", "-lv", logVerbosity,
		"--embd-output-format", "json", "-f", "/dev/null")
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("embed: reading the devices' PCI addresses: %w%s", err, errorLines(stderr.Bytes()))
	}
	used := usedDevices(stderr.Bytes())
	out := make([]Device, len(devs))
	for i, d := range devs {
		out[i] = d
		for _, u := range used {
			if u.Arg == d.Arg {
				out[i].PCI = u.PCI
			}
		}
	}
	return out, nil
}

// usedDevices are the devices a run's log says it used.
func usedDevices(stderr []byte) []Device {
	var used []Device
	for _, m := range usingDeviceLine.FindAllSubmatch(stderr, -1) {
		arg := string(m[1])
		used = append(used, Device{Arg: arg, Name: string(m[2]), PCI: strings.ToLower(string(m[3])), Backend: backendOf(arg)})
	}
	return used
}

// errorLines is the runtime's error-level log lines (" E "), for a failure
// message. They name what went wrong, never the prompt.
func errorLines(stderr []byte) string {
	var b strings.Builder
	for _, line := range strings.Split(string(stderr), "\n") {
		if _, msg, ok := strings.Cut(line, " E "); ok {
			b.WriteString(": " + strings.TrimSpace(msg))
		}
	}
	return b.String()
}
