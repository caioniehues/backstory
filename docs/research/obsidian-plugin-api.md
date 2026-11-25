# What an Obsidian desktop plugin can do: child processes, graph rendering, install layout

Research for issue #27 (child of map #14). Gathered 2026-11-25. It answers the question for a desktop-only Obsidian plugin (`isDesktopOnly: true`) that runs the `backstory` CLI (`… --json`) and draws a Ledger graph view. Constraints come from `docs/fork-roadmap.md` and ADR 0001: the plugin never writes memory into vault files.

Sources are primary unless marked. Facts observed on this Device were read-only inspections; no paths or personal data are recorded here.

## Answer in brief

- **Child processes work.** Desktop Obsidian plugins run with Node.js and Electron APIs. `require("child_process")` (`spawn`, `execFile`) is what established plugins use. The plugin must call the binary by **absolute path**, because a GUI-launched Obsidian does not get the login shell's PATH. On this Device `backstory` is not on PATH at all. Use async `spawn` with `shell: false`. Never use `execSync`: it would freeze the UI thread.
- **Flatpak Obsidian is a trap; the AppImage and native builds are fine.** The Flathub sandbox remaps `XDG_DATA_HOME` and `XDG_RUNTIME_DIR` and has no `flatpak-spawn --host` permission. A child `backstory` would look for a different store and daemon socket. This Device runs the official AppImage, so nothing here is sandboxed.
- **Custom views:** subclass `ItemView`, register it with `registerView`, render into `contentEl` and tear down in `onClose`. Any DOM, canvas or WebGL library works inside it, bundled into `main.js` by esbuild.
- **Graph library recommendation:** **sigma.js + graphology** (WebGL, ForceAtlas2 or force layout in a web worker), or **d3-force on a canvas** if the Ledger stays at a few thousand nodes. Cytoscape.js is the heavier, richer option. Pixi.js is a renderer only, with no graph model or layout, and is the largest. See the table below. Confidence on the exact thresholds is medium: the Ledger size was not measured, because there is no Backstory store on this Device.
- **Folder layout:** `<vault>/<configDir>/plugins/<id>/` holding `manifest.json`, `main.js`, an optional `styles.css`, and a `data.json` that Obsidian writes. `<configDir>` is `.obsidian` by default but the user can rename it. The folder name should equal the manifest `id`. The id `backstory` is valid.
- **Load and reload:** Obsidian loads the enabled community plugins at startup. Enabling, disabling and reloading can be done in Settings, by toggling the plugin off and on, or from the official **Obsidian CLI**: `obsidian plugin:enable id=backstory filter=community`, `obsidian plugin:reload id=backstory`, `obsidian plugins:restrict off`. That CLI is registered on this Device.
- **Restricted mode** blocks all community plugins until the user turns it off. It is a per-vault user choice. The installer should report it and should not flip it silently.
- **Sync:** Obsidian Sync does **not** copy community plugins by default. "Installed community plugin list" and "Active community plugin list" are off unless the user turns them on, and plugin settings (`community-plugin-data`) are a separate category. LiveSync copies `.obsidian/plugins/` only through opt-in **Hidden File Sync** or **Customisation Sync**. In the designed-for vault both LiveSync features are **off**. Obsidian Sync's toggles are not stored in the vault, so they must be checked in the UI.

## 1. Spawning child processes

| Fact | Source |
|---|---|
| Node.js and Electron APIs are available only in desktop Obsidian. A plugin that uses them **must** set `isDesktopOnly: true`. | Developer docs, *Submission requirements for plugins*, "Node.js and Electron APIs are only allowed on desktop" |
| In the manifest, `isDesktopOnly` means "can only be used on the desktop app, for example because it uses NodeJS or Electron APIs". | Developer docs, *Reference/Manifest* |
| The sample plugin's esbuild config marks `electron` and all of Node's `builtinModules` as external and builds CJS, so `require("child_process")` resolves at runtime against Obsidian's Node. | `obsidianmd/obsidian-sample-plugin`, `esbuild.config.mjs` |
| A widely used plugin does this: obsidian-git imports `spawn` from `child_process`. | `Vinzent03/obsidian-git`, `src/utils.ts` |
| Obsidian cannot restrict what plugins do. "Plugins inherit Obsidian's access levels": they can read files, use the network and install programs. | Help, *Plugin security → Plugin capabilities* |
| `Platform.isDesktopApp` / `Platform.isLinux` let a plugin detect where it runs. `FileSystemAdapter.getBasePath()` gives the vault's absolute path on desktop. Gate it with `instanceof FileSystemAdapter`. | `obsidian.d.ts`; developer docs *Platform* and the plugin self-critique checklist |

**Practical rules for the Backstory plugin** (these follow from the facts above and from Node's documented behaviour):

1. **Absolute binary path.** A desktop launcher starts Obsidian with the session environment, not an interactive shell, so version-manager shims and `~/.local/bin` additions may be missing. Have `backstory install obsidian` write the absolute path of the running `backstory` binary (Go's `os.Executable()`) into the plugin's default settings, with a settings field to override it. Then the plugin never searches PATH.
2. **`spawn(path, args, {shell: false})`**, collecting stdout. `execFile` also works, but Node caps its buffered output at `maxBuffer` (1 MiB by default), and a `graph --json` dump could exceed that. Never use `execSync` or `spawnSync`: the plugin runs on Obsidian's renderer thread, so a blocking call freezes the window.
3. **One CLI-run module**, with not-installed, non-zero-exit and busy handling in one place. This is the lesson of audit item #7 for the Quickshell panel.
4. **Environment.** Pass `process.env` through, so `XDG_DATA_HOME` and `XDG_RUNTIME_DIR` match the user's session. That holds for native and AppImage installs.
5. **Flatpak.** The Flathub manifest (`flathub/md.obsidian.Obsidian`) gives `--filesystem=home` but not `--talk-name=org.freedesktop.Flatpak`, so the plugin cannot reach the host with `flatpak-spawn --host`. Inside the sandbox Flatpak points `XDG_DATA_HOME` and `XDG_RUNTIME_DIR` at per-app locations. A child `backstory` would therefore open or create a different store and miss the daemon socket. Options: declare Flatpak unsupported (recommended), or have the plugin pass the store path explicitly. The second only works if the CLI accepts one, and the daemon socket would still be out of reach. The Obsidian CLI binary itself knows a separate Flatpak socket path, which confirms that Obsidian treats Flatpak as a distinct runtime. (Flatpak's XDG remapping is standard Flatpak behaviour, not taken from Obsidian's docs. Confidence: high.)

Observed on this Device: Obsidian is the official AppImage (installer 1.15.x), which has no sandbox. `backstory` is not on PATH and there is no Backstory store, so Backstory is not installed here yet.

## 2. Custom views and graph libraries

### ItemView

- Subclass `ItemView`, implement `getViewType()`, `getDisplayText()`, `onOpen()` (render into `this.contentEl`) and `onClose()` (unmount, stop layouts and workers). Register it in `onload` with `this.registerView(TYPE, leaf => new View(leaf))`. That API exists since 0.9.7. (Developer docs, *Plugins/User interface/Views*; *Reference/…/Plugin/registerView*.)
- React or Svelte can be mounted on `contentEl` (developer docs, *Use React / Use Svelte in your plugin*). A graph canvas is mounted the same way.
- Register intervals and DOM events through the plugin's `register*` helpers so that unload and reload clean up. This matters because `plugin:reload` is how upgrades get picked up.

### Library comparison

npm versions and unpacked sizes are from `npm view` on 2026-11-25. Unpacked size includes sources, types and maps. It is an upper bound, not the bundled size.

| Library | What it is | Rendering | Layout | Scale (per its docs) | npm, unpacked |
|---|---|---|---|---|---|
| **sigma.js + graphology** | Graph renderer on a graph model | WebGL | graphology-layout-forceatlas2 or graphology-layout-force, both with web-worker supervisors | Built for "thousands of nodes and edges". Its performance guide only starts tuning (`antialiasEdges: false`, lower curve segments) at "hundreds of thousands of edges". | sigma 4.0.6, 3.4 MB; graphology 0.26.0, 2.7 MB; FA2 0.10.1, 79 kB |
| **d3-force** | Layout only (velocity Verlet). You draw it yourself, on SVG or canvas. | You choose | Many-body force with a Barnes–Hut quadtree (`theta` 0.9 by default) | No stated limit. SVG DOM becomes the bottleneck long before the simulation does. | 3.0.1, 55 kB |
| **Cytoscape.js** | Full graph-theory library with layouts, styles and selection | Canvas, plus an optional WebGL mode (`webgl: true`) that falls back to canvas when zoomed in close | Many built-in and extension layouts | Its docs call the old large-graph hints (`hideEdgesOnViewport`, `textureOnViewport`) "largely moot" after performance work. The WebGL mode exists for large networks. | 3.35.0, 5.8 MB |
| **pixi.js** | 2D WebGL engine with no graph model and no layout | WebGL / WebGPU | None (pair it with d3-force) | Not graph-specific | 8.23.0, 80 MB |

Sources: sigmajs.org *How-to → Improve rendering performances* and *drag-drop* (ForceSupervisor worker); `d3/d3-force` manyBody API reference; `cytoscape/cytoscape.js` `documentation/md/core/init.md`, `documentation/md/performance.md`, `documentation/webgl.md`.

**Recommendation.** The Ledger is small by graph standards. Records are written on purpose, so they number in the hundreds to low thousands, and edges are a small multiple of that. Embedding-neighbour edges are what can grow: k neighbours per Record means k·N extra edges.
- **Default: sigma.js + graphology.** WebGL keeps panning smooth with several thousand nodes plus faint k-NN edges. The layout runs in a worker, off Obsidian's UI thread. Graphology also gives a convenient in-memory model for filters (hide Tombstoned Records, show only Current ones, edge kinds).
- **Lighter alternative: d3-force with a canvas renderer.** It is the smallest dependency and the simplest code, and it is enough if the view shows hundreds to low thousands of nodes. Avoid SVG beyond about a thousand elements. That figure is general web practice, not from d3's docs; confidence medium.
- Cytoscape fits only if the view needs compound nodes or graph algorithms. Pixi adds size without adding graph features.
- **Measure before committing.** Count Records and edges on a Device that has a store. This Device has none, so no count is given here. Have `graph --json` support a node cap or filter, so the view never has to draw the whole Ledger.

## 3. Folder layout, loading, reloading, restricted mode

### Layout

```
<vault>/<configDir>/plugins/backstory/
  manifest.json   required; read by Obsidian
  main.js         required; the bundled CJS plugin
  styles.css      optional; loaded automatically with the plugin
  data.json       written by the plugin's saveData(); absent until first save
```

- **manifest.json** needs `id`, `name`, `version` (semver x.y.z), `minAppVersion`, `description`, `author` and `isDesktopOnly`. `authorUrl` and `fundingUrl` are optional. The id may contain only lowercase letters and hyphens, can't end with `plugin` and can't contain `obsidian`. "For local development, the id should match the plugin's folder name; otherwise some methods, such as `onExternalSettingsChange`, won't be called." (Developer docs, *Reference/Manifest*.)
- **data.json:** `loadData()` and `saveData()` read and write `data.json` in the plugin folder. **`onExternalSettingsChange()`** (since 1.5.7) fires when `data.json` changes on disk from outside, such as a sync service or another program. That lets the installer update settings, for example the binary path, while Obsidian runs. (Developer docs, *Plugin/loadData*, *saveData*, *onExternalSettingsChange*.)
- **configDir is not always `.obsidian`.** Settings → Files and Links → *Override config folder* can rename it per vault, and Obsidian Headless takes `--config-dir` (Help, *Configuration folder*; *Sync/headless*). In the API it is `app.vault.configDir`. `backstory install obsidian --vault` should default to `.obsidian`, accept `--config-dir`, and `--check` should warn when `.obsidian` is missing.

### Install by hand, load, reload

- Obsidian finds plugins in `<configDir>/plugins/`. The official tutorial installs by putting a folder there and then enabling it under **Settings → Community plugins → Installed plugins** (Developer docs, *Build a plugin*). The enabled ids are listed in `<configDir>/community-plugins.json` (observed in the designed-for vault).
- **Reload after an upgrade:** toggle the plugin off and on in Installed plugins (Developer docs, *Development workflow*), restart Obsidian, or use **`obsidian plugin:reload id=backstory`** (Help, *Obsidian CLI → Developer commands*). The community Hot-Reload plugin is a dev-time option only.
- **Obsidian CLI** (installer 1.12.7+). Enable it under Settings → General → Command line interface. It talks to the running app over a unix socket, and if the app is not running, the first command launches it. Relevant commands: `plugins` (with `format=json`), `plugin:enable`, `plugin:disable`, `plugin:reload`, `plugins:restrict [on|off]`. (Help, *Obsidian CLI*.) It is registered on this Device.
- **Recommendation for `backstory install obsidian`.** Copy the three files, keeping files the user edited, as the roadmap requires. Then, if the Obsidian CLI is present, run `obsidian plugin:enable id=backstory filter=community` on install and `obsidian plugin:reload id=backstory` on upgrade, and `obsidian plugin:disable` before `--remove`. If it is not present, print the manual steps. Avoid editing `community-plugins.json` while Obsidian runs, because the app owns that file and may write it back. That last point is an inference, not documented; confidence medium.
- **Caution:** the CLI starts Obsidian if it is not running. `--check` should therefore not call it, or should first check that Obsidian is already running.

### Restricted mode

- Restricted mode (formerly Safe mode) turns all community plugins off. To install or use one, the user must turn it off under Settings → Community plugins (Help, *Plugin security → Restricted mode*; *Community plugins*). The CLI exposes `plugins:restrict` to check or toggle it.
- It is a deliberate per-vault security switch. `backstory install obsidian` should **report** it and leave it alone. In the designed-for vault it is already off, since LiveSync and Style Settings are enabled community plugins.

## 4. Do Obsidian Sync or LiveSync copy `.obsidian/plugins/`?

**Obsidian Sync.** The default vault-configuration sync covers main settings, appearance, themes and snippets, hotkeys, the active core plugin list and core plugin settings. "To sync community plugins, manually enable **Active community plugin list** and **Installed community plugin list**." (Help, *Sync → Settings → Selective syncing*.) Headless Sync splits these into `community-plugin` and `community-plugin-data` categories, so plugin code and plugin settings (`data.json`) are toggled separately (Help, *Sync/headless*, `ob sync-config --configs`). Synced plugin changes take effect on other devices only after a restart (Help, *Troubleshoot Sync*). These toggles are not stored as files in the vault, so they could not be inspected here. Check them in Settings → Sync.

**Self-hosted LiveSync.** It has two opt-in features that can carry plugin files (`vrtmrz/obsidian-livesync`, `docs/settings.md` and `docs/tips/hidden-file-sync.md`):
- **Hidden File Sync** (`syncInternalFiles`) syncs files under `.obsidian`. Its default ignore list excludes only `node_modules`, `.git` and LiveSync's own folder, so once it is on, `plugins/backstory/` would sync unless it is excluded.
- **Customisation Sync** (`usePluginSync`) syncs plugin files, snippets and config per item.

The docs call both "supported, opt-in". In the designed-for vault's LiveSync settings, `syncInternalFiles` and `usePluginSync` are both **false**.

**Implications for install**
- Neither syncs the plugin by default, and today neither is on for LiveSync. If either gets turned on, the plugin's code reaches other devices. That is harmless: it is code, not memory (ADR 0001), and `isDesktopOnly` stops it loading on mobile.
- **Keep `data.json` free of memory and secrets.** If plugin data syncs, it reaches other devices. The settings should hold only preferences and the binary path. A synced absolute path from another device would be wrong, so the plugin should fall back to a clear "backstory not found, set the path" message, and `--check` should verify the path.
- `--check` could report whether LiveSync's Hidden File Sync or Customisation Sync is on. The installer can read those keys from LiveSync's `data.json`, but that file also holds the user's sync credentials, so it should read only those two keys. Obsidian Sync's toggles cannot be read, so it can only print a reminder.

## Confidence and gaps

- **High:** child processes in desktop plugins, ItemView API, manifest fields, data.json, the CLI commands, restricted mode, Obsidian Sync defaults, LiveSync opt-in features.
- **Medium:** graph-library size thresholds (no Ledger was measured on this Device), and whether writing `community-plugins.json` while Obsidian runs is unsafe (not documented).
- **Not verified:** the Obsidian Sync toggle state for the designed-for vault (it lives in the app, not the vault). Also not verified: how an AppImage's own environment variables affect children. The AppImage runtime may add variables, so pass the environment through and test once.

## Sources

- Obsidian developer docs (`obsidianmd/obsidian-developer-docs`): Reference/Manifest; Reference/TypeScript API (PluginManifest, Plugin.registerView, loadData, saveData, onExternalSettingsChange, Platform); Plugins/User interface/Views; Getting started/Build a plugin, Development workflow, Mobile development; Community directory/Submission requirements for plugins.
- `obsidianmd/obsidian-api` `obsidian.d.ts` (FileSystemAdapter.getBasePath, Vault.configDir).
- `obsidianmd/obsidian-sample-plugin` `esbuild.config.mjs`.
- Obsidian Help: plugin-security, community-plugins, cli, configuration-folder, data-storage, sync/settings, sync/headless, sync/troubleshoot.
- `vrtmrz/obsidian-livesync`: docs/settings.md, docs/tips/hidden-file-sync.md.
- `flathub/md.obsidian.Obsidian` manifest (finish-args).
- `Vinzent03/obsidian-git` `src/utils.ts` (child_process spawn in a shipped plugin).
- sigmajs.org how-to pages (performance, drag-drop); `d3/d3-force` manyBody reference; `cytoscape/cytoscape.js` documentation (init, performance, webgl).
- npm registry (`npm view <pkg> version dist.unpackedSize`).
