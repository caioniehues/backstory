# The Obsidian integration never writes memory into vault files

The Obsidian integration is a desktop-only plugin that runs the `backstory` CLI and renders what it returns, the way the Quickshell panel does. It does not write Records out as markdown notes. The vault this was designed for (`~/ObsidianVault`) syncs through Obsidian Sync and LiveSync. Memory written there would leave the machine, and Purge and Delete could not reach those copies. That would break both "nothing leaves your machine" and "forgetting reaches everywhere". The cost is that Obsidian's own graph view, Bases, backlinks and search, which work only on files, don't see Backstory memory. The plugin draws its own Ledger graph instead.

## Considered Options

- **Export Records as wikilinked notes (rejected as the default).** It gives Obsidian's own graph and Bases for free, but it puts memory text into files that sync. It may come back later as an opt-in adapter at the `export` seam, written to a vault that does not sync, and the forgetting module would have to cover it.
