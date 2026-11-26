package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/RidgetopAi/backstory/internal/embed"
	"github.com/RidgetopAi/backstory/internal/store"
)

// embedCheckTimeout bounds --on's checks: a device probe plus one or two
// test embeds, each well under a second warm (#19), more on a first-ever run.
const embedCheckTimeout = 2 * time.Minute

// embedTestDocument is what --on's test embed sends: no Record text.
const embedTestDocument = "backstory embed --on: test embed"

// runEmbed is `backstory embed [--on --runtime PATH --model PATH [--device
// DEV] [--allow-cpu] | --off]`, shaped like `backup auto`: with no flag it
// shows the setting. --on runs one test embed and saves nothing unless it
// passes; the daemon then embeds in the background.
func runEmbed(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("embed", flag.ContinueOnError)
	on := fs.Bool("on", false, "turn embeddings on, after one test embed")
	off := fs.Bool("off", false, "turn embeddings off (stored vectors are kept)")
	runtime := fs.String("runtime", "", "with --on: llama.cpp's llama-embedding binary")
	model := fs.String("model", "", "with --on: the EmbeddingGemma 2 GGUF model file")
	device := fs.String("device", "", "with --on: the GPU's name or PCI address, as --list-devices shows it (needed when more than one is listed)")
	allowCPU := fs.Bool("allow-cpu", false, "with --on: accept embedding on the CPU")
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "backstory embed: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	switch {
	case *on && *off:
		_, _ = fmt.Fprintln(stderr, "backstory embed: --on and --off together")
		return 2
	case *on && (*runtime == "" || *model == ""):
		_, _ = fmt.Fprintln(stderr, "backstory embed: --on needs --runtime PATH and --model PATH")
		return 2
	case !*on && (set["runtime"] || set["model"] || set["device"] || set["allow-cpu"]):
		_, _ = fmt.Fprintln(stderr, "backstory embed: --runtime, --model, --device and --allow-cpu go with --on")
		return 2
	}

	st, code := openStoreFor("embed", stderr)
	if st == nil {
		return code
	}
	defer func() { _ = st.Close() }()

	switch {
	case *on:
		return embedOn(st, *runtime, *model, *device, *allowCPU, stdout, stderr)
	case *off:
		if err := embed.TurnOff(st); err != nil {
			_, _ = fmt.Fprintln(stderr, "backstory embed:", err)
			return 1
		}
		_, _ = fmt.Fprintln(stdout, "embeddings off; stored vectors are kept")
		return 0
	}
	s, isOn, err := embed.LoadSetup(st)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "backstory embed:", err)
		return 1
	}
	if !isOn {
		_, _ = fmt.Fprintln(stdout, "embeddings: off")
		return 0
	}
	_, _ = fmt.Fprintf(stdout, "embeddings: on\nruntime: %s\nmodel: %s\ndevice: %s\n", s.Runtime, s.Model, s.Device)
	return 0
}

// embedOn checks the setup with one test embed and saves it only if it
// passes. Every refusal says why and ends in "(nothing saved)".
func embedOn(st *store.Store, runtime, model, device string, allowCPU bool, stdout, stderr io.Writer) int {
	refuse := func(err error) int {
		_, _ = fmt.Fprintf(stderr, "backstory embed: %v (nothing saved)\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), embedCheckTimeout)
	defer cancel()

	s := embed.Setup{Runtime: absPath(runtime), Model: absPath(model), Device: device}
	if err := embed.CheckFiles(s); err != nil {
		return refuse(err)
	}
	if s.Device == "" {
		devs, err := embed.ListDevices(ctx, s.Runtime)
		if err != nil {
			return refuse(err)
		}
		switch len(devs) {
		case 0:
			if !allowCPU {
				return refuse(fmt.Errorf("%w: no GPU is listed; pass --allow-cpu to embed on the CPU", embed.ErrNoGPUOffload))
			}
			s.Device = embed.DeviceCPU
		case 1:
			s.Device = devs[0].Name
		default:
			return refuse(fmt.Errorf("more than one GPU is listed; pick one with --device NAME or --device PCI-ADDRESS:\n%s", embed.DeviceList(devs)))
		}
	}
	if s.Device == embed.DeviceCPU && !allowCPU {
		return refuse(fmt.Errorf("%w: --device %s embeds on the CPU; pass --allow-cpu too", embed.ErrNoGPUOffload, embed.DeviceCPU))
	}

	b, err := testEmbed(ctx, s)
	if errors.Is(err, embed.ErrNoGPUOffload) && allowCPU {
		s.Device = embed.DeviceCPU
		b, err = testEmbed(ctx, s)
	} else if errors.Is(err, embed.ErrNoGPUOffload) {
		return refuse(fmt.Errorf("%w; pass --allow-cpu to embed on the CPU", err))
	}
	if err != nil {
		return refuse(err)
	}

	hash, err := embed.ModelHash(st, s.Model)
	if err != nil {
		return refuse(err)
	}
	if err := embed.SaveSetup(st, s); err != nil {
		return refuse(err)
	}
	dev := b.Device()
	_, _ = fmt.Fprintf(stdout, "embeddings on: %s (%s)\nfingerprint: %s\nthe daemon embeds Records in the background\n",
		dev.Name, s.Device, embed.Fingerprint(hash, dev.Backend, embed.Dim))
	return 0
}

// testEmbed resolves s's device and embeds one test document on it.
func testEmbed(ctx context.Context, s embed.Setup) (*embed.Batch, error) {
	b, err := embed.NewBatch(ctx, s)
	if err != nil {
		return nil, err
	}
	if _, err := b.Embed(ctx, []string{embedTestDocument}); err != nil {
		return nil, err
	}
	return b, nil
}

// absPath is p made absolute, or p when that fails (the file check then
// reports it).
func absPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}
