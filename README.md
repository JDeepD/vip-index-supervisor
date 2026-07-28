# vip-index-supervisor

A single-binary TUI that supervises WordPress VIP Enterprise Search indexing.

Indexing a large site takes hours, and a deploy kills the running process.
This tool drives `wp vip-search index` to completion, automatically resuming
from the last indexed object ID after any interruption — deploy SIGTERM, OOM
kill, connection reset, or a stall — so nobody has to babysit the run.

Run it on a persistent host (bastion / tmux), **not** inside the VIP
container: a deploy is what kills the indexing process.

## Build

```
go build -o vip-index-supervisor .
```

Cross-compile for any OS — the binary is fully static, no runtime needed:

```
GOOS=linux  GOARCH=amd64 go build -o vip-index-supervisor-linux .
GOOS=darwin GOARCH=arm64 go build -o vip-index-supervisor-mac .
GOOS=windows GOARCH=amd64 go build -o vip-index-supervisor.exe .
```

## Use

Just run the binary — everything is interactive:

- **arrows / j k** move · **enter** select · **space** toggle multi-selects
- **esc** goes back to the previous screen at any point
- **q** quits from menus; **ctrl+c** during a run stops gracefully
  (a second ctrl+c force-kills)

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
| versions | list index versions and document counts              |
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
| stale lock (`An index is already occurring`)| `delete-transient`, then retry (not a failed attempt) |
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
