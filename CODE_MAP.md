# Code Map

Manually regenerated after the `cmd/`+`internal/` reorg (the `update-file-docs.js`/`.soul-docs` pipeline that normally produces this file automatically isn't part of this checkout). Every entry below is a real definition at the exact file/line shown.

## Structs

| Name | File:Line | Summary |
|---|---|---|
| `Desktop` | `internal/desktop/types.go:4-17` | Represents a managed desktop container: its identity, profile, harness (`ollama`/`claude`), backing Docker resources, status, and resource allocation. Serialized to JSON for the API and templates. |
| `Manager` (desktop) | `internal/desktop/manager.go:22-26` | Owns the Docker client and the desktop containers it manages; the mutex guards create/delete against concurrent access. |
| `Manager` (ollama) | `internal/ollama/ollama.go:29-32` | Owns the Docker client and network name used to create/find the single shared Ollama container. |
| `HelpNote` | `internal/helpnotes/store.go:11-18` | A support note submitted from inside a running desktop, tied to a specific desktop and VM. |
| `Store` (helpnotes) | `internal/helpnotes/store.go:21-24` | Mutex-guarded in-memory map of `HelpNote` by ID. |
| `WorkerMemoryEntry` | `internal/memory/store.go:14-21` | A reusable workflow or decision learned while a desktop worker was operating. |
| `Store` (memory) | `internal/memory/store.go:24-27` | Mutex-guarded in-memory map of `[]WorkerMemoryEntry` keyed by desktop ID. |
| `Server` | `internal/httpapi/server.go:17-22` | Composes `desktop.Manager`, `helpnotes.Store`, `memory.Store`, and `webui.Renderer` to serve the control-plane HTTP API and dashboard. |
| `PageData` | `internal/webui/render.go:17-22` | Template data for the index/messages pages: the current desktops, the worker memory, and the request host. |
| `HelpNotesPageData` | `internal/webui/render.go:25-27` | Template data for the `/dashboard` help-notes page. |
| `Renderer` | `internal/webui/render.go:30-32` | Loads and executes the web UI's HTML templates (`index.html` parsed once at startup; `messages.html`/`dashboard.html` re-parsed per request). |

## Funcs

### `cmd/flowera`

| Name | File:Line | Summary |
|---|---|---|
| `main` | `cmd/flowera/main.go:22-71` | Loads `.env`, builds the Docker client, constructs `desktop.Manager`/`ollama.Manager`/`httpapi.Server`, ensures desktop images/network exist, registers HTTP routes, kicks off Ollama readiness in the background, and starts `http.ListenAndServe(:8080)`. |

### `internal/desktop`

| Name | File:Line | Summary |
|---|---|---|
| `NewManager` | `internal/desktop/manager.go:29-31` | Creates a `Manager` backed by the given Docker client. |
| `Create` | `internal/desktop/manager.go:34-152` | Provisions a new desktop container (volume, env, container config) for the given profile and harness and starts it, rolling back on failure. |
| `List` | `internal/desktop/manager.go:155-273` | Returns all desktop containers managed by this instance, read from their Docker labels (`desktop-profile`, `desktop-harness`, defaulting to `ollama` when absent) and inspect data. |
| `Start` | `internal/desktop/manager.go:276-288` | Starts the Docker container backing the given desktop ID. |
| `Stop` | `internal/desktop/manager.go:291-313` | Stops the Docker container backing the given desktop ID, honoring `DESKTOP_STOP_TIMEOUT_SECONDS`. |
| `Restart` | `internal/desktop/manager.go:316-340` | Stops and restarts the Docker container backing the given desktop ID. |
| `Delete` | `internal/desktop/manager.go:343-397` | Removes the Docker container and persistent volume backing the given desktop ID. |
| `SendMessage` | `internal/desktop/manager.go:400-478` | Posts a JSON message to a running desktop's internal `:8081/message` endpoint, verifying the container exists and is running first. |
| `PersistMemoryFile` | `internal/desktop/manager.go:481-527` | Docker-execs a heredoc write of pre-formatted memory content into a desktop container's `/home/headless/memory` directory. |
| `ProxyTarget` | `internal/desktop/proxy.go:23-58` | Resolves the internal VNC address for a desktop ID, returning sentinel errors (`ErrNotFound`/`ErrNotRunning`/`ErrNoNetwork`/`ErrNoAddress`) so `httpapi` can reproduce the original 404/503 messages without this package touching `net/http`. |
| `EnsureImages` | `internal/desktop/docker.go:30-79` | Pulls any required desktop Docker images that are not already present locally. |
| `EnsureNetwork` | `internal/desktop/docker.go:81-114` | Creates the shared `flowera-network` Docker network if it does not already exist. |
| `resolveDesktopImage` | `internal/desktop/docker.go:116-131` | Maps a desktop profile (basic/developer/ai) to its corresponding Docker image. |
| `buildDesktopEnv` | `internal/desktop/docker.go:137-161` | Builds the container environment variables (VNC, desktop identity, control-plane/Ollama URLs, `AGENT_HARNESS`, and `ANTHROPIC_API_KEY` when set) for a desktop by name and harness. |
| `buildContainerConfigs` | `internal/desktop/docker.go:164-200` | Builds the Docker container, host, and network configuration for a desktop from its resolved image, resources, profile, and harness (labels both `desktop-profile` and `desktop-harness`). |
| `sanitizeName` | `internal/desktop/docker.go:190-212` | Normalizes a string into a safe lowercase container/volume name fragment. |
| `ServerHost` | `internal/desktop/docker.go:214-221` | Returns the configured external host (`DESKTOP_HOST`), defaulting to `localhost`. |

### `internal/ollama`

| Name | File:Line | Summary |
|---|---|---|
| `NewManager` | `internal/ollama/ollama.go:36-38` | Creates a `Manager` backed by the given Docker client and network name. |
| `EnsureReady` | `internal/ollama/ollama.go:42-228` | Ensures the Ollama image is pulled, a persistent volume exists, the container is created/started, the API becomes reachable, and the configured model (`Model`) is pulled. |
| `waitForOllama` | `internal/ollama/ollama.go:230-272` | Polls the Ollama API until it responds successfully or a two-minute deadline elapses. |
| `ensureModel` | `internal/ollama/ollama.go:274-` | Checks whether the configured model exists in the container (`ollama show`) and pulls it (`ollama pull`) if not. |

### `internal/helpnotes`

| Name | File:Line | Summary |
|---|---|---|
| `NewStore` | `internal/helpnotes/store.go:27-31` | Creates an empty help-notes `Store`. |
| `Add` | `internal/helpnotes/store.go:34-39` | Records a help note. |
| `List` | `internal/helpnotes/store.go:42-53` | Returns all recorded help notes, in no particular order. |
| `Close` | `internal/helpnotes/store.go:56-61` | Deletes a help note by ID (no-op if absent). |

### `internal/memory`

| Name | File:Line | Summary |
|---|---|---|
| `NewStore` | `internal/memory/store.go:30-34` | Creates an empty memory `Store`. |
| `Record` | `internal/memory/store.go:39-77` | Validates and stores a new worker memory entry; does not touch Docker (see `desktop.PersistMemoryFile`, composed by `httpapi.desktopMemoryHandler`). |
| `List` | `internal/memory/store.go:79-101` | Returns worker memory entries (optionally filtered by desktop ID), most recent first. |
| `FormatForDesktopFile` | `internal/memory/store.go:103-122` | Renders a memory entry as the flat text file persisted into a desktop's memory directory, and returns the filename to use. |

### `internal/httpapi`

| Name | File:Line | Summary |
|---|---|---|
| `NewServer` | `internal/httpapi/server.go:25-33` | Creates a `Server` from its component stores/managers. |
| `RegisterRoutes` | `internal/httpapi/server.go:35-51` | Registers every HTTP route the control plane serves (unchanged from the original single-file route table). |
| `dashboardHandler` | `internal/httpapi/desktops.go:16-46` | Renders the root dashboard page listing all desktops and worker memory. |
| `desktopsAPIHandler` | `internal/httpapi/desktops.go:48-57` | Returns the current list of desktops as JSON. |
| `createDesktopHandler` | `internal/httpapi/desktops.go:60-170` | Validates a desktop-creation request (name, profile, harness, cpus, memory) — rejecting `harness=claude` with `profile=basic` — and creates the desktop. |
| `startDesktopHandler` | `internal/httpapi/desktops.go:173-194` | Starts an existing desktop by ID. |
| `stopDesktopHandler` | `internal/httpapi/desktops.go:197-218` | Stops a running desktop by ID. |
| `restartDesktopHandler` | `internal/httpapi/desktops.go:221-242` | Restarts a desktop by ID. |
| `deleteDesktopHandler` | `internal/httpapi/desktops.go:245-266` | Deletes a desktop and its persistent storage by ID. |
| `desktopProxyHandler` | `internal/httpapi/proxy.go:14-99` | Reverse-proxies HTTP requests to a desktop container's internal VNC address, mapping `desktop.ProxyTarget`'s sentinel errors to the original 404/503 responses. |
| `messagesDashboardHandler` | `internal/httpapi/messages.go:11-32` | Renders the messages dashboard page for sending messages to desktops. |
| `sendMessageHandler` | `internal/httpapi/messages.go:34-` | Validates and forwards a message-send request to a specific desktop. |
| `helpTextHandler` | `internal/httpapi/helpnotes.go:15-60` | Records a help note submitted from inside a desktop. |
| `helpNotesHandler` | `internal/httpapi/helpnotes.go:62-65` | Returns all recorded help notes as JSON. |
| `closeHelpNoteHandler` | `internal/httpapi/helpnotes.go:67-81` | Deletes a help note by ID. |
| `dashboardPostsHandler` | `internal/httpapi/helpnotes.go:83-` | Renders the help-notes dashboard, most recent first. |
| `desktopMemoryHandler` | `internal/httpapi/memory.go:12-` | Stores a new worker memory entry (via `memory.Store.Record` + `desktop.PersistMemoryFile`) or returns all remembered workflows. |
| `jsonResponse` / `jsonError` | `internal/httpapi/json.go:10-` / `:20-` | Write a value as JSON / write a generic 500 JSON error (never leaks `err.Error()` to the client; logs it server-side). |
| `getHost` | `internal/httpapi/host.go:10-` | Derives the externally-reachable host from an incoming request, substituting `DESKTOP_HOST` for `localhost`. |

### `internal/webui`

| Name | File:Line | Summary |
|---|---|---|
| `NewRenderer` | `internal/webui/render.go:35-41` | Parses `index.html` once at startup. |
| `RenderIndex` | `internal/webui/render.go:44-46` | Renders the root dashboard page. |
| `RenderMessages` | `internal/webui/render.go:50-56` | Renders the messages page (re-parses `messages.html` per call). |
| `RenderHelpNotesDashboard` | `internal/webui/render.go:60-66` | Renders the help-notes dashboard (re-parses `dashboard.html` per call). |
