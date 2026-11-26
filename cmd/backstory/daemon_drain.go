package main

import (
	"context"
	"log"
	"os"

	"github.com/RidgetopAi/backstory/internal/drain"
	"github.com/RidgetopAi/backstory/internal/store"
)

// startDrain starts the daemon's embedding drain (#42) in the background:
// a pass now, after each Record this daemon writes (st's write hook), and
// on drain.PollInterval for CLI and Sync writes. It runs whether or not
// Capture is paused; `embed --off` is what stops it. While embeddings are
// off each pass reads the setting and touches nothing else. The returned
// stop unhooks it, ends it and waits, so runDaemon defers it before the
// store closes.
func startDrain(ctx context.Context, st *store.Store, logger *log.Logger) (stop func()) {
	d := drain.New(drain.Config{Store: st, Holder: os.Getpid(), Open: drain.OpenBatch(st), Logger: logger})
	st.OnRecordWrite(d.Wake)
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.Run(ctx)
	}()
	return func() {
		st.OnRecordWrite(nil)
		cancel()
		<-done
	}
}
