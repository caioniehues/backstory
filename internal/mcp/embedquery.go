package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/RidgetopAi/backstory/internal/search"
	"github.com/RidgetopAi/backstory/internal/store"
)

// DaemonMethodEmbedQuery embeds search query text on the daemon's warm
// query server (#20 Q21, #43). Like DaemonMethodBlock it is exported for the
// CLI side (DaemonEmbedder) and is not one of the five frozen tools. It is
// read-only and never mints a session.
const DaemonMethodEmbedQuery = "embed_query"

// EmbedQueryDeadline is how long the daemon waits for its query embedder
// before answering that the deadline was missed. It is below the Searcher's
// search.QueryEmbedDeadline so that the daemon's answer, not the client's
// own timeout, reaches the CLI, with room for the socket round trip.
const EmbedQueryDeadline = search.QueryEmbedDeadline * 4 / 5

// Daemon error codes the CLI side maps back onto the Searcher's reasons.
const (
	embedCodeOff      = "embeddings-off"
	embedCodeDeadline = "deadline-exceeded"
)

// ErrDaemonUnreachable is DaemonEmbedder's error when no daemon answers on
// the socket: the Searcher's reason is "embeddings unavailable: daemon
// unreachable".
var ErrDaemonUnreachable = errors.New("daemon unreachable")

// EmbedQueryParams is embed_query's argument. With FingerprintOnly the
// daemon embeds nothing and answers the current fingerprint (a Record
// query's need).
type EmbedQueryParams struct {
	Text            string `json:"text,omitempty"`
	FingerprintOnly bool   `json:"fingerprint_only,omitempty"`
}

// EmbedQueryResult is embed_query's answer: the vector (absent for
// FingerprintOnly) and the fingerprint it was made under.
type EmbedQueryResult struct {
	Fingerprint string    `json:"fingerprint"`
	Vector      []float32 `json:"vector,omitempty"`
}

// ServeOption configures ServeDaemonConn beyond its fixed arguments.
type ServeOption func(*serveOptions)

type serveOptions struct {
	queryEmbedder  search.Embedder
	recordEmbedder search.Embedder
}

// WithQueryEmbedder gives the daemon's embed_query method its query
// embedder (the warm query server). Without it embed_query answers
// "embeddings off".
func WithQueryEmbedder(e search.Embedder) ServeOption {
	return func(o *serveOptions) { o.queryEmbedder = e }
}

// WithRecordEmbedder gives note's related-Decisions check its embedder for
// the new Decision's text: the batch path (embed.RecordEmbedder), never the
// warm query server, which holds query text only (#37 user story 45).
// Without it the check is text-only.
func WithRecordEmbedder(e search.Embedder) ServeOption {
	return func(o *serveOptions) { o.recordEmbedder = e }
}

// daemonSearchers are a connection's two Searchers: recall's, whose
// embedder is the warm query server, and note's related-Decisions check's,
// whose embedder is the batch path.
type daemonSearchers struct {
	recall, related search.Searcher
}

// relatedSearcher is the Searcher note's related-Decisions check uses: over
// the record embedder under RelatedEmbedDeadline, or text-only without one.
// It never uses the connection's own Searcher, whose embedder is the query
// server.
func (o serveOptions) relatedSearcher(st *store.Store) search.Searcher {
	if o.recordEmbedder == nil {
		return search.New(st)
	}
	return search.New(st, search.WithEmbedder(o.recordEmbedder), search.WithDeadline(RelatedEmbedDeadline))
}

// isEmbedQuery reports whether a connection's first request line is
// embed_query, which is answered with no session.
func isEmbedQuery(line []byte) bool {
	var req DaemonRequest
	return json.Unmarshal(line, &req) == nil && req.Method == DaemonMethodEmbedQuery
}

// handleEmbedQuery answers embed_query within EmbedQueryDeadline, returning
// at the deadline even if emb ignores its context.
func handleEmbedQuery(emb search.Embedder, raw json.RawMessage) DaemonResponse {
	var p EmbedQueryParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return errResponse("invalid-params", "invalid embed_query params: "+err.Error())
		}
	}
	if !p.FingerprintOnly && strings.TrimSpace(p.Text) == "" {
		return errResponse("invalid-params", `missing required field "text"`)
	}
	if emb == nil {
		return errResponse(embedCodeOff, search.ReasonEmbeddingsOff)
	}
	ctx, cancel := context.WithTimeout(context.Background(), EmbedQueryDeadline)
	defer cancel()
	type answer struct {
		res EmbedQueryResult
		err error
	}
	done := make(chan answer, 1)
	go func() {
		var a answer
		if p.FingerprintOnly {
			a.res.Fingerprint, a.err = emb.Fingerprint(ctx)
		} else {
			a.res.Fingerprint, a.res.Vector, a.err = emb.EmbedQuery(ctx, p.Text)
		}
		done <- a
	}()
	var a answer
	select {
	case a = <-done:
	case <-ctx.Done():
		a.err = ctx.Err()
	}
	switch {
	case errors.Is(a.err, search.ErrEmbeddingsOff):
		return errResponse(embedCodeOff, search.ReasonEmbeddingsOff)
	case errors.Is(a.err, context.DeadlineExceeded):
		return errResponse(embedCodeDeadline, fmt.Sprintf("query embedding missed the daemon's %s deadline", EmbedQueryDeadline))
	case a.err != nil:
		return errResponse("unavailable", a.err.Error())
	}
	b, err := json.Marshal(a.res)
	if err != nil {
		return errResponse("internal", err.Error())
	}
	return DaemonResponse{Result: b}
}

// DaemonEmbedder is the CLI's and export's search.Embedder: it asks the
// daemon at SocketPath (DaemonMethodEmbedQuery), so the CLI never starts a
// GPU runtime itself. No daemon reads as ErrDaemonUnreachable, embeddings
// off in the daemon as search.ErrEmbeddingsOff, and the daemon's missed
// deadline as context.DeadlineExceeded.
type DaemonEmbedder struct {
	SocketPath string
}

var _ search.Embedder = DaemonEmbedder{}

// EmbedQuery embeds text on the daemon's query server.
func (d DaemonEmbedder) EmbedQuery(ctx context.Context, text string) (string, []float32, error) {
	res, err := d.call(ctx, EmbedQueryParams{Text: text})
	if err != nil {
		return "", nil, err
	}
	return res.Fingerprint, res.Vector, nil
}

// Fingerprint is the daemon's current fingerprint.
func (d DaemonEmbedder) Fingerprint(ctx context.Context) (string, error) {
	res, err := d.call(ctx, EmbedQueryParams{FingerprintOnly: true})
	return res.Fingerprint, err
}

// daemonDialTimeout bounds the dial when ctx has no deadline.
const daemonDialTimeout = 5 * time.Second

func (d DaemonEmbedder) call(ctx context.Context, p EmbedQueryParams) (EmbedQueryResult, error) {
	timeout := daemonDialTimeout
	if dl, ok := ctx.Deadline(); ok {
		timeout = time.Until(dl)
	}
	if timeout <= 0 {
		return EmbedQueryResult{}, ctx.Err()
	}
	conn, err := net.DialTimeout("unix", d.SocketPath, timeout)
	if err != nil {
		if ctx.Err() != nil {
			return EmbedQueryResult{}, ctx.Err()
		}
		return EmbedQueryResult{}, ErrDaemonUnreachable
	}
	defer func() { _ = conn.Close() }()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	params, err := json.Marshal(p)
	if err != nil {
		return EmbedQueryResult{}, err
	}
	line, err := json.Marshal(DaemonRequest{Method: DaemonMethodEmbedQuery, Params: params})
	if err != nil {
		return EmbedQueryResult{}, err
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return EmbedQueryResult{}, d.ioErr(ctx, err)
	}
	reply, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return EmbedQueryResult{}, d.ioErr(ctx, err)
	}
	var resp DaemonResponse
	if err := json.Unmarshal(reply, &resp); err != nil {
		return EmbedQueryResult{}, fmt.Errorf("decode the daemon's answer: %w", err)
	}
	if resp.Error != nil {
		switch resp.Error.Code {
		case embedCodeOff:
			return EmbedQueryResult{}, search.ErrEmbeddingsOff
		case embedCodeDeadline:
			return EmbedQueryResult{}, fmt.Errorf("daemon: %s: %w", resp.Error.Message, context.DeadlineExceeded)
		}
		return EmbedQueryResult{}, errors.New(resp.Error.Message)
	}
	var res EmbedQueryResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		return EmbedQueryResult{}, fmt.Errorf("decode the daemon's answer: %w", err)
	}
	return res, nil
}

// ioErr is ctx's error when it ended, else the daemon going away mid-call.
func (DaemonEmbedder) ioErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("daemon: %w", err)
}
