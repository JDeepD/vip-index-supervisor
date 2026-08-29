# vip-index-supervisor

A single-binary TUI that supervises WordPress VIP Enterprise Search indexing.

Indexing a large site takes hours, and a deploy kills the running process.
This tool drives `wp vip-search index` to completion, automatically resuming
from the last indexed object ID after any interruption — deploy SIGTERM, OOM
kill, connection reset, or a stall — so nobody has to babysit the run.

Run it on a persistent host (bastion / tmux), **not** inside the VIP
container: a deploy is what kills the indexing process.

## Install on a remote instance

The binaries are published as GitHub release assets — no runtime, no
dependencies. The install script detects OS/arch, verifies the SHA-256
checksum against the release, and puts the binary on PATH
(`/usr/local/bin` if writable, else `~/.local/bin`):

```
curl -fsSL https://vip-index-supervisor.jdeep.in/install.sh | bash
```

It is built to be pipe-safe: nothing executes until the whole script has
arrived, a corrupted download fails the checksum and installs nothing, and
`VERSION=v1.0.0` / `INSTALL_DIR=...` environment variables override the
defaults. Prefer to read it first? It is [install.sh](install.sh) in the
repository root.

### Manual download

On a typical remote instance (Linux x86_64):

```
curl -fsSL -o vip-index-supervisor https://github.com/JDeepD/vip-index-supervisor/releases/latest/download/vip-index-supervisor-linux-amd64
chmod +x vip-index-supervisor
./vip-index-supervisor
```

Or with wget:

```
wget -O vip-index-supervisor https://github.com/JDeepD/vip-index-supervisor/releases/latest/download/vip-index-supervisor-linux-amd64
chmod +x vip-index-supervisor
```

For other machines, swap the asset name:

| platform          | asset                                    |
|-------------------|------------------------------------------|
| Linux x86_64      | `vip-index-supervisor-linux-amd64`       |
| Linux ARM64       | `vip-index-supervisor-linux-arm64`       |
| macOS Apple Si    | `vip-index-supervisor-darwin-arm64`      |
| macOS Intel       | `vip-index-supervisor-darwin-amd64`      |
| Windows x86_64    | `vip-index-supervisor-windows-amd64.exe` |

Pin a version by replacing `latest/download` with `download/<tag>`, e.g.
`download/v1.0.0`. If the repository is private, plain curl/wget cannot reach
release assets — use the GitHub CLI instead:

```
gh release download --repo JDeepD/vip-index-supervisor --pattern 'vip-index-supervisor-linux-amd64'
```

## Build and release

```
go build -o vip-index-supervisor .
```

Cross-compile for every OS — the binary is fully static:

```
GOOS=linux   GOARCH=amd64 go build -o dist/vip-index-supervisor-linux-amd64 .
GOOS=linux   GOARCH=arm64 go build -o dist/vip-index-supervisor-linux-arm64 .
GOOS=darwin  GOARCH=arm64 go build -o dist/vip-index-supervisor-darwin-arm64 .
GOOS=darwin  GOARCH=amd64 go build -o dist/vip-index-supervisor-darwin-amd64 .
GOOS=windows GOARCH=amd64 go build -o dist/vip-index-supervisor-windows-amd64.exe .
```

The asset names above are what the install commands expect. Publish them
(include `dist/checksums.txt` so downloads can be verified):

```
gh release create v1.0.0 dist/* --title v1.0.0
```

Or let CI do all of it — pushing a version tag triggers
`.github/workflows/release.yml`, which builds every platform from the tagged
commit, stamps the version, and publishes the release with checksums:

```
git tag v1.0.2
git push origin v1.0.2
```

`./release.sh v1.0.2` remains the manual/local path; `--dry-run` builds
without publishing.

## Use

Just run the binary — everything is interactive:

- **arrows / j k** move · **enter** select (on forms: next field) · **space**
  toggle
- **esc** goes back to the previous screen at any point
- **q** quits from menus; **ctrl+c** during a run stops gracefully
  (a second ctrl+c force-kills); the run log scrolls with **↑/↓ / pgup/pgdn**
- `vip-index-supervisor --version` prints the release the binary was built from

The options step has an **Advanced ▸** screen for the rare overrides: a forced
resume object ID, a custom state directory, the stall timeout, and ignoring
the state-dir lock. Attempt logs older than 14 days are pruned automatically
at the start of each run.

The first screen picks how WordPress is reached:

- **WordPress VIP environment** — runs `vip @app.env --yes -- wp vip-search …`
  (requires VIP-CLI on PATH and authenticated; get the env from `vip app list`)
- **Direct wp-cli** — runs `wp vip-search …` against any site where `wp`
  already works (a local install, a plain server); extra args like
  `wp --path=/srv/site` are fine

Then an action:

| action   | purpose                                              |
|----------|------------------------------------------------------|
| index    | run/resume supervised indexing with live progress    |
| info     | versions + status + resume point                     |
| status   | current indexing progress, one shot                  |
| watch    | poll status until indexing goes idle                 |
| health   | is the active index populated? do counts align?      |
| counts   | DB vs ES document counts                             |
| versions | browse versions: activate, delete, or build into one |
| unlock   | clear a stale index lock (delete-transient)          |
| stop     | ask a running index to stop                          |

The exact command the wizard's answers produce is shown before anything runs,
so the flags stay learnable rather than hidden.

## Build strategies

**New version (recommended).** Registers a new index version, builds into it,
verifies it, and activates it only once the phase completes. The current index
keeps serving search for the entire rebuild. Activation is gated: the new
version must never be empty and must hold at least 90% of the documents of the
index it replaces — a rebuild that finishes but produces nothing leaves the
old index serving and tells you to investigate.

**Resume in place.** Continues indexing into the version serving search now,
picking the resume point from the local checkpoint and the platform's own
`get-last-indexed-post-id` (whichever is safer, i.e. lower).

**Rebuild in place (`--setup`).** Drops and recreates the live index; search
returns nothing until the rebuild finishes. Against a production-looking
environment the TUI requires typing the environment name to confirm, because
every command carries `--skip-confirm` and nothing downstream will stop you.

## Managing index versions

The `versions` action is interactive: pick an indexable, arrow over its
versions, and press enter on one to act on it.

- **Activate** (inactive versions only) — makes that version serve search.
  The confirmation shows its document count against the currently active
  index and warns before anything runs: an empty version, a version holding
  under 90% of the active one's documents, or an older version (a rollback)
  are all flagged. Against a production-looking environment you must type the
  environment name to confirm.
- **Delete** (inactive versions only) — permanently removes the version and
  its documents, behind the same confirmation and production guard. The
  active version can never be deleted from here.
- **Build into** — runs supervised indexing *into* that existing version
  (`--version=N`), with all the usual resume/checkpoint behaviour, but never
  creates or activates anything: when the build completes, the tool tells you
  to activate it from the versions screen once you are satisfied. This is how
  you finish a half-built version whose earlier run refused activation.

After an activate or delete, the list refreshes so the new state is visible
immediately.

## How resuming works

Indexing walks object IDs from high to low, printing `Last Object ID: N`. The
supervisor checkpoints the lowest `N` seen and resumes with
`--upper-limit-object-id=N`. Re-indexing the boundary object is an idempotent
upsert — overlap is harmless, skipping is not.

Each indexable is a separate supervised phase with its own checkpoint (object
ID spaces are per-indexable). `--setup` is applied only on the first attempt,
so a restart can never wipe a partially built index. With a new version, the
chosen version number is pinned to the state directory, so a restart resumes
into the same half-built version instead of registering a new one each time.

Checkpoints are scoped by (indexable, post types, version) — a checkpoint from
one scope is never trusted by another — and are written atomically (temp file,
fsync, rename), because the process is expected to be killed mid-write.

State lives under `~/.vip-reindex/<target>/`:

```
checkpoint.<indexable>[.<post-types>][.v<n>]   lowest object ID reached
version.<indexable>[.<post-types>]             version being built, while in flight
supervisor.lock                                held for the life of a run
logs/supervisor.log                            every event, timestamped
logs/events.jsonl                              the same events, structured (jq-able)
logs/attempt-*.log                             raw output of each attempt
```

Only one supervisor may hold a state directory at a time; the lock is an OS
file lock, released by the kernel if the holder dies, so it cannot go stale.

## Failure handling

| situation                                   | response                                            |
|---------------------------------------------|-----------------------------------------------------|
| stale lock (`An index is already occurring`)| `delete-transient`, then retry with exponential backoff (not a failed attempt) |
| lock that will not clear                    | after 5 tries, probe the blocking sync: a **frozen** one (no movement in 15s) is cleared with `stop-indexing`; a **live** one aborts the run with a diagnosis, untouched |
| process died, progress was made             | resume from the checkpoint, reset backoff           |
| process died, no progress                   | exponential backoff, abort after 5 fruitless tries  |
| no output for 10 minutes                    | kill the process tree and retry                     |
| bad flag, unknown post type, expired auth   | abort immediately — retrying cannot help            |
| wall-clock budget reached                   | stop at a checkpoint so a re-run resumes            |
| new version failed verification             | old index stays active; pin kept for manual review  |
| second ctrl+c                               | skip the grace period and force-kill the tree       |

Non-retryable failures are recognised only on lines that *begin* with
`Error:`/`Warning:`, so a post titled "Unauthorized biography of a forbidden
city", echoed back by verbose error output, cannot abort the run.

## Layout

```
main.go                    entry point (TUI only; refuses to run without a terminal)
internal/vipsearch/        command construction + output parsing
  target.go                vip-CLI vs direct wp, running commands
  parse.go                 progress/fatal/lock parsing, JSON extraction
  client.go                typed read commands (status, versions, counts, …)
internal/supervise/        the engine
  supervisor.go            phase/attempt loop, stall watchdog, backoff
  checkpoint.go            scoped, atomic checkpoint files
  versions.go              new-version create / verify / activate
  lock.go                  single-instance state-dir lock
  process_{unix,windows}.go  process-tree termination per OS
internal/tui/              Bubble Tea front-end
  app.go                   screen stack (esc pops)
  screen_wizard.go         target → action → index wizard → confirm
  screen_run.go            live supervision dashboard
  screen_output.go         read-only commands, watch, unlock
```
