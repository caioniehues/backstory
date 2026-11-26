package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/RidgetopAi/backstory/internal/drain"
	"github.com/RidgetopAi/backstory/internal/embed"
	"github.com/RidgetopAi/backstory/internal/store"
)

// embedCheckTimeout bounds --on's checks: a device probe plus one or two
// test embeds, each well under a second warm (#19), more on a first-ever run.
const embedCheckTimeout = 2 * time.Minute

// embedTestDocument is what --on's test embed sends: no Record text.
const embedTestDocument = "backstory embed --on: test embed"

// catchUpPoll is how often --catch-up looks at the daemon's progress while
// the daemon holds the drain lease.
var catchUpPoll = time.Second

// runEmbed is `backstory embed [--on --runtime PATH --model PATH [--device
// DEV] [--allow-cpu] | --off | --catch-up | --drop]`, shaped like `backup
// auto`: with no flag it shows the setting. --on runs one test embed and
// saves nothing unless it passes; the daemon then embeds in the background.
// --catch-up drains the queue in the foreground; --drop deletes every
// vector.
func runEmbed(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("embed", flag.ContinueOnError)
	on := fs.Bool("on", false, "turn embeddings on, after one test embed")
	off := fs.Bool("off", false, "turn embeddings off (stored vectors are kept)")
	catchUp := fs.Bool("catch-up", false, "embed every queued Record now, in the foreground, with progress")
	drop := fs.Bool("drop", false, "delete every stored vector (secure-delete plus a WAL checkpoint)")
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
	modes := 0
	for _, m := range []bool{*on, *off, *catchUp, *drop} {
		if m {
			modes++
		}
	}
	switch {
	case modes > 1:
		_, _ = fmt.Fprintln(stderr, "backstory embed: give one of --on, --off, --catch-up and --drop")
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
	case *catchUp:
		return embedCatchUp(st, stdout, stderr)
	case *drop:
		return embedDrop(st, stdout, stderr)
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

// embedCatchUp is --catch-up: it drains the queue in the foreground with a
// progress line per batch. While another drainer (the daemon) holds the
// lease it doesn't drain: it prints that drainer's progress until the queue
// is empty, or until the lease is free and it can drain the rest itself.
func embedCatchUp(st *store.Store, stdout, stderr io.Writer) int {
	if _, on, err := embed.LoadSetup(st); err != nil {
		_, _ = fmt.Fprintln(stderr, "backstory embed:", err)
		return 1
	} else if !on {
		_, _ = fmt.Fprintln(stderr, "backstory embed: embeddings are off; turn them on with `backstory embed --on`")
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	d := drain.New(drain.Config{Store: st, Holder: os.Getpid(), Open: drain.OpenBatch(st),
		Progress: func(p drain.Progress) { _, _ = fmt.Fprintln(stdout, embedProgress(p.Counts)) }})
	last := ""
	for {
		out, err := d.Pass(ctx)
		switch out {
		case drain.Drained:
			c := store.EmbedCounts{}
			if r, ok, _ := drain.ReadReport(st); ok && r.Fingerprint != "" {
				c, _ = st.EmbedCounts(r.Fingerprint)
			}
			_, _ = fmt.Fprintf(stdout, "caught up: %d of %d embedded, %d skipped\n", c.Embedded, c.Embedded+c.Queued, d.Skipped())
			return 0
		case drain.Off:
			_, _ = fmt.Fprintln(stderr, "backstory embed: embeddings were turned off; stopped")
			return 1
		case drain.Failed:
			_, _ = fmt.Fprintln(stderr, "backstory embed --catch-up:", err)
			return 1
		}

		// Deferred: follow the lease holder's progress under its fingerprint.
		l, held, err := st.DrainLease(time.Now())
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "backstory embed:", err)
			return 1
		}
		line := "waiting for the drain lease to be free"
		if r, ok, err := drain.ReadReport(st); err == nil && ok && r.Fingerprint != "" && held {
			c, err := st.EmbedCounts(r.Fingerprint)
			if err != nil {
				_, _ = fmt.Fprintln(stderr, "backstory embed:", err)
				return 1
			}
			if c.Queued == 0 {
				_, _ = fmt.Fprintf(stdout, "caught up by the daemon (pid %d): %d of %d embedded\n", l.Holder, c.Embedded, c.Embedded)
				return 0
			}
			line = fmt.Sprintf("the daemon (pid %d) is draining: %s", l.Holder, embedProgress(c))
		} else if held {
			line = fmt.Sprintf("the daemon (pid %d) holds the drain lease; waiting", l.Holder)
		}
		if line != last {
			_, _ = fmt.Fprintln(stdout, line)
			last = line
		}
		select {
		case <-ctx.Done():
			_, _ = fmt.Fprintln(stderr, "backstory embed --catch-up: interrupted")
			return 1
		case <-time.After(catchUpPoll):
		}
	}
}

// embedProgress is "N of M embedded, Q queued".
func embedProgress(c store.EmbedCounts) string {
	return fmt.Sprintf("%d of %d embedded, %d queued", c.Embedded, c.Embedded+c.Queued, c.Queued)
}

// embedDrop is --drop: every vector deleted, with secure-delete and a WAL
// checkpoint. Records are untouched.
func embedDrop(st *store.Store, stdout, stderr io.Writer) int {
	n, err := st.DropRecordEmbeddings()
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "backstory embed:", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "dropped %d vectors (secure-delete plus a WAL checkpoint)\n", n)
	if _, on, err := embed.LoadSetup(st); err == nil && on {
		_, _ = fmt.Fprintln(stdout, "embeddings are still on, so the daemon embeds every Record again; `backstory embed --off` stops it")
	}
	return 0
}

// printEmbeddingsLine prints status's `embeddings:` line: off, or on with
// the drain's short fingerprint, embedded vs queued, skipped and the last
// failure, or why the runtime didn't open (a device that didn't resolve).
// A store that can't be read prints nothing.
func printEmbeddingsLine(stdout io.Writer) {
	dbPath, err := storePath()
	if err != nil {
		return
	}
	if _, err := os.Stat(dbPath); err != nil {
		return
	}
	st, err := openStore(dbPath)
	if err != nil {
		return
	}
	defer func() { _ = st.Close() }()
	if line, ok := embeddingsStatus(st); ok {
		_, _ = fmt.Fprintln(stdout, line)
	}
}

func embeddingsStatus(st *store.Store) (string, bool) {
	_, on, err := embed.LoadSetup(st)
	if err != nil {
		return "", false
	}
	if !on {
		return "embeddings: off", true
	}
	r, ok, err := drain.ReadReport(st)
	if err != nil {
		return "", false
	}
	failure := ""
	if !r.LastFailureAt.IsZero() {
		failure = ", last failure " + r.LastFailureAt.Local().Format("2006-01-02 15:04")
		if r.LastFailure != "" && r.LastFailure != r.SetupError {
			failure += ": " + oneLine(r.LastFailure)
		}
	}
	switch {
	case r.SetupError != "":
		return "embeddings: on, not embedding: " + oneLine(r.SetupError) + failure, true
	case !ok || r.Fingerprint == "":
		return "embeddings: on, not drained yet (the daemon embeds in the background)", true
	}
	c, err := st.EmbedCounts(r.Fingerprint)
	if err != nil {
		return "", false
	}
	return fmt.Sprintf("embeddings: on (%s), %d embedded, %d queued, %d skipped%s",
		shortFingerprint(r.Fingerprint), c.Embedded, c.Queued, r.Skipped, failure), true
}

// shortFingerprint cuts the model hash, the fingerprint's first part, to 8
// characters: "2188ac1d|tpl-ecc4bd80|vulkan|768".
func shortFingerprint(fp string) string {
	hash, rest, ok := strings.Cut(fp, "|")
	if ok && len(hash) > 8 {
		return hash[:8] + "|" + rest
	}
	return fp
}

// oneLine folds a multi-line message (a refusal that lists the devices)
// onto one line.
func oneLine(s string) string {
	var parts []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			parts = append(parts, strings.TrimSuffix(l, ":"))
		}
	}
	return strings.Join(parts, "; ")
}
