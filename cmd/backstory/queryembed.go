package main

import (
	"log"
	"path/filepath"

	"github.com/RidgetopAi/backstory/internal/drain"
	"github.com/RidgetopAi/backstory/internal/embed"
	"github.com/RidgetopAi/backstory/internal/mcp"
	"github.com/RidgetopAi/backstory/internal/search"
	"github.com/RidgetopAi/backstory/internal/store"
)

// queryServerSocketName is the warm query server's socket, beside the
// daemon's own in its 0700 runtime directory. llama-server wants a unix
// socket path ending in ".sock".
const queryServerSocketName = "embed.sock"

// startQueryServer starts the daemon's warm query server (#43): it follows
// `embed --on`/`--off` by itself, so the daemon only starts it here and
// closes it on the way out. Its socket sits beside daemonSock.
func startQueryServer(st *store.Store, daemonSock string, logger *log.Logger) *embed.QueryServer {
	return embed.StartQueryServer(queryServerConfig(st, daemonSock, logger))
}

// queryServerConfig is the query server's configuration. A crash or a
// failed start backs off by the drain's rules (#42): drain.FirstBackoff,
// doubling, capped at drain.MaxBackoff.
func queryServerConfig(st *store.Store, daemonSock string, logger *log.Logger) embed.QueryServerConfig {
	return embed.QueryServerConfig{
		Settings: st,
		Socket:   filepath.Join(filepath.Dir(daemonSock), queryServerSocketName),
		Backoff:  &embed.Backoff{Base: drain.FirstBackoff, Cap: drain.MaxBackoff},
		Logf:     logger.Printf,
	}
}

// cliSearcher is the Searcher the CLI's own commands (recall, export) use.
// While embeddings are on here, query text is embedded by the daemon over
// its socket (embed_query); with no daemon the search is FTS-only with
// "embeddings unavailable: daemon unreachable". While they are off it is
// FTS-only with "embeddings off", and nothing dials the daemon.
func cliSearcher(st *store.Store) search.Searcher {
	if _, on, err := embed.LoadSetup(st); err != nil || !on {
		return search.New(st)
	}
	sock, err := socketPath()
	if err != nil {
		return search.New(st)
	}
	return search.New(st, search.WithEmbedder(mcp.DaemonEmbedder{SocketPath: sock}))
}
