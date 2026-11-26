# Dreams may send text to a Harness the Human chose; Backstory still opens no connection

A Dream reads a Project's recent Sessions and Records and writes Drafts. It can run on either of two backends:
- **a local generative model** on this Device's GPU, through the same llama.cpp runtime as embeddings;
- **a cloud Harness** run headless (`claude -p` or `codex exec`).

Dreaming is off by default. The Human picks one backend per Device. Choosing a cloud Harness means the dream window's Record text and Timeline summaries go to that Harness's cloud model, under the account the Harness is logged into.

So the promise changes from "nothing leaves your machine" to **"Backstory opens no connection; a Harness you chose may"**:
- `ops/nonetwork_test.go` is unchanged, because Backstory only starts a process.
- The README says nothing leaves the machine unless cloud dreaming is turned on.
- Turning it on (`backstory dream --on --harness …`) prints exactly what is sent and to whom, and asks for consent. The consent is saved per Device with the wording's version, and a new wording stops cloud dreaming until the Human consents again.

The run itself gets no write path. It returns schema-checked JSON, and the daemon writes the Drafts as `IdentityInference` in one transaction. Backstory's own runs are kept out of later inference by leaving no transcript, keeping Backstory's hooks inert (`BACKSTORY_OFF=1`), and skipping the run by its id.

The decision and its reasons are in [#23](https://github.com/caioniehues/backstory/issues/23). What a dream reads and writes is in [#24](https://github.com/caioniehues/backstory/issues/24), and when it runs and what it may spend is in [#25](https://github.com/caioniehues/backstory/issues/25).

## Considered Options

- **Local model only (rejected).** It keeps the old promise whole, but the quality of a 12–20B local model for consolidation is unproven. It would make dreaming depend on a GPU, and on a large model download.
- **Cloud Harness only (rejected).** Its quality is higher and #22 researched it, but it would make dreaming impossible for a Human who won't send Ledger text to a cloud model.
- **Backstory calls a model API directly (rejected).** That would mean Backstory opening a connection, an exception to the no-network test, and an API key to store. A Harness already holds the account and opens the connection.
- **A cloud Harness without per-Device consent (rejected).** A Device that syncs a setting from a Peer could then start sending text without the Human ever choosing it on that Device.
