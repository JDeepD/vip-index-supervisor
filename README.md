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
| history / recovery | inspect saved runs and resume their exact scope |
| info     | versions + status + resume point                     |
| status   | current indexing progress, one shot                  |
| watch    | poll status until indexing goes idle                 |
| health   | is the active index populated? do counts align?      |
| counts   | DB vs ES document counts                             |
| versions | browse versions: activate, delete, or build into one |
| unlock   | clear a stale index lock (delete-transient)          |
| stop     | ask a running index to stop                          |
| notifications | configure ntfy phone alerts and send a test     |

The exact command the wizard's answers produce is shown before anything runs,
so the flags stay learnable rather than hidden.

## Saved runs and recovery

Every supervised run now saves its settings, selected versions, checkpoints,
phase progress, attempt outcomes, and final result. Open **history / recovery**
after selecting your target. Choose a run to see the recovery assessment, then
confirm **Resume this saved run** when offered. On a finished run dashboard,
press **r** to open its recovery screen directly.

History lists local records only. Opening a recovery assessment reads remote
status and version lists; it never stops a worker, clears a transient, creates
an index, or activates a version. It shows the saved checkpoint and version,
the current local pin, remote state, recent attempts, the recorded error, and
the next action:

- **Ready to resume:** the local lock is free, remote status currently reports
  idle, and the saved target/version scope passes the checks.
- **Another worker may still be running:** wait and investigate the active
  worker. Quiet output is not evidence that its lock is stale.
- **Needs investigation:** status is unknown, versions/pins changed, history
  is corrupt or superseded, or a destructive operation was interrupted without
  enough evidence to resume safely.

The supervisor reloads the saved record and repeats recovery checks under the
local state lock before starting. Resume preserves the target, post-type
filter, per-page count, strategy, and other indexing settings; it uses that
run's saved version/checkpoint, not a newer shared checkpoint file. Completed
phases are skipped. If indexing itself finished but verification did not,
resume retries completion checks without indexing those objects again.

Saved-run recovery **never repeats `--setup`**. A partially rebuilt phase with
confirmed progress resumes into the same version. A setup phase with no
confirmed progress—including untouched phases of a multi-phase setup
run—requires an explicit decision in the ordinary wizard. An interrupted
version-registration command also requires review, because another registration
might create an unnecessary version. Already-active new-version builds require
inspection rather than automatic replay.

Creation metadata, when available, is compared to detect a deleted/recreated
numeric version. Recovery cannot prove that an index was not manually rebuilt
outside this supervisor; do not reuse its checkpoint if that happened. An idle
report is not a guarantee that no remote transient exists, and a ready
assessment does not repair invalid settings or authentication. Inspect the
recorded error before resuming.

Resuming creates a new history entry linked to the original. Progress and
milestone state are carried forward; the saved time budget and retry allowance
apply afresh to this resume. Notifications use the current session's settings.
Direct WP runs must be resumed from the original working directory.

Records live at `<state-dir>/runs/<run-id>.json`; `runs/latest` identifies the
newest run independently of wall-clock changes. History is not automatically
deleted. Each record retains the latest 200 attempt summaries and the omitted
count; raw attempt logs still expire after 14 days. Use **d** in history to
inspect a custom state directory. Older checkpoint-only runs remain available
through the ordinary wizard, but cannot be reconstructed into exact saved runs.

History files use owner-only Unix permissions; protect them with appropriate
Windows permissions. Notification endpoints/tokens are excluded, but direct WP
commands and error details may still be sensitive. A record left as "running"
after a crash means unfinished bookkeeping, not proof of a live or dead worker.

## Phone notifications (ntfy)

Notifications are optional and off until you configure a topic:

1. Subscribe to a topic in the ntfy iPhone app.
2. After choosing a target, open **notifications**, or use **Index → Options →
   Notifications**. Enter the full topic URL, for example
   `https://ntfy.sh/your-long-unguessable-topic`, using the same server and
   topic as the app. Add an access token if your server/topic requires one.
3. Select **Send test**, check your phone, then **Apply**. Sending a test does
   not start indexing or apply/save the settings.
4. Enable **Remember on this computer** before applying if you want the
   settings loaded in future sessions. Otherwise they affect only this session.

Supervised indexing sends alerts for run start, **25%, 50%, 75%, and 100% per
indexing phase**, and the final result (completed, failed, or interrupted).
For a post-only run these are that run's progress milestones. Optional retry
alerts are limited to one per minute per phase. Failures use high priority.
There are no periodic heartbeats or external monitors. Batch-by-batch progress
and standalone actions such as `status`, `watch`, `unlock`, or manual version
activation do not send alerts.

Percentages use the reported object counts for the saved phase workload, never
object IDs. Retries and saved-run resumes retain progress and do not repeat
already queued milestones. Unknown totals produce no percentage alerts.
The 100% alert waits for a clean indexing result and any required verification
and activation; merely printing a full progress count is not enough. For the
last phase, the run-completed notification is the 100% alert, avoiding a
duplicate completion message. A newly started run without history measures its
remaining workload, not work done by an unrelated older run.

Delivery runs in the background and is best-effort: HTTP failures do not change
the indexing result, and are reported in the supervisor log. Requests time out
after 4 seconds; shutdown allows up to 6 seconds for the final alert. The final
result takes priority over queued intermediate alerts, so short runs or a slow
server may omit intermediate messages. Failed requests are not retried, and an
abrupt host shutdown or `SIGKILL` cannot send a final notification. Acceptance
by ntfy is not confirmation that the phone received the push.
Milestone state is saved before queueing, so a crash at that boundary can omit
an alert; delivery is not exactly-once.

Alerts contain the target/environment and phase/version/checkpoint details,
but never raw CLI output, command arguments, or the access token. Topics on a
public server are not inherently private: use an unguessable topic or access
controls. See [ntfy publishing and authentication](https://docs.ntfy.sh/publish/).
The tool requires HTTPS, except for HTTP loopback addresses used in local tests,
and does not follow redirects. For a self-hosted ntfy server, configure
[iOS instant notifications](https://docs.ntfy.sh/config/#ios-instant-notifications)
on that server as well.

Remembered settings are stored as `vip-index-supervisor/notifications.json`
inside the OS user configuration directory (`$XDG_CONFIG_HOME`, or
`~/.config` on Linux; `~/Library/Application Support` on macOS; `%AppData%` on
Windows). The token is **not encrypted**. The file has owner-only permissions
on Unix; protect it with appropriate Windows permissions. These settings apply
to future targets on the same computer. To disable saved notifications, clear
the topic URL and apply with **Remember on this computer** enabled; applying
without it leaves previously saved settings unchanged.

## Build strategies

**New version (recommended).** Registers a new index version, builds into it,
verifies it, and activates it only once the phase completes. The current index
keeps serving search for the entire rebuild. Activation is gated: the new
version must never be empty and must hold at least 90% of the documents of the
index it replaces — a rebuild that finishes but produces nothing leaves the
old index serving and tells you to investigate.
Unknown document counts, a missing previous active version, and filtered
post-type builds also block automatic activation. A failed version registration
never silently reuses another inactive version; choose **Build into** explicitly
if that is what you intend.

**Resume in place.** Continues indexing into the version serving search now,
resolving that version to a number and using only its scoped local checkpoint.
`get-last-indexed-post-id` is informational: it does not identify the version
or post-type filter that produced it, so it is not used for automatic resume.

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
ID spaces are per-indexable). `--setup` is retained if an attempt was refused
by a lock before doing any work, but is never repeated after progress. An
ambiguous setup failure before progress requires operator review. With a new version, the
chosen version number is pinned to the state directory, so a restart resumes
into the same half-built version instead of registering a new one each time.
Missing or already-active pins require review instead of silently targeting
the live index. Newly created versions discard any old checkpoint for that slot.

Checkpoints are scoped by (indexable, post types, version) — a checkpoint from
one scope is never trusted by another — and are written atomically (temp file,
fsync, rename), because the process is expected to be killed mid-write.

State lives under `~/.vip-reindex/<target>/`:

```
checkpoint.<indexable>[.types-<hash>].v<n>     lowest object ID reached
version.<indexable>[.types-<hash>]             version being built, while in flight
supervisor.lock                                held for the life of a run
logs/supervisor.log                            every event, timestamped
logs/events.jsonl                              the same events, structured (jq-able)
logs/attempt-*.log                             raw output of each attempt
runs/<run-id>.json                            saved settings, progress, attempts, outcome
runs/latest                                  newest run ID (independent of wall clock)
```

Only one supervisor may hold a state directory at a time; the lock is an OS
file lock, released by the kernel if the holder dies, so it cannot go stale.

Compatibility note: legacy unversioned checkpoints (`checkpoint.post`),
slug-only post-type scopes, and `upper_limit_object_id` are not automatically
imported. Direct WP targets now have separate state directories based on their
command and working directory. Old files are retained. Without a scoped
checkpoint, Resume starts from the top **without `--setup`**; to avoid replay,
use an explicit resume ID only after verifying its target/version/filter.

## Failure handling

| situation                                   | response                                            |
|---------------------------------------------|-----------------------------------------------------|
| lock refusal before progress               | wait 10s after the first error and 30s after the second, then retry without deleting remote state |
| third consecutive lock refusal             | diagnose the sync; known-idle status permits one cleanup, while active or unknown state is left untouched. No movement in 15s is not proof of a dead worker |
| process died, progress was made             | resume from the checkpoint, reset backoff           |
| process died, no progress                   | exponential backoff, abort after 5 fruitless tries  |
| no output for 10 minutes                    | probe remote progress before stopping the local process tree; do not erase remote sync metadata |
| bad flag, unknown post type, expired auth   | abort immediately — retrying cannot help            |
| wall-clock budget reached                   | stop at a checkpoint so a re-run resumes            |
| new version failed verification             | old index stays active; pin kept for manual review  |
| second ctrl+c                               | skip the grace period and force-kill the tree       |

Progress resets the lock-error sequence. A local VIP-CLI exit is not proof
that its remote worker has stopped. If diagnosis leaves a lock in place,
confirm that no indexer is running before using **unlock**, then **resume the
same version**; a lock alone is not a reason to rebuild with `--setup`.

Unrelated PHP/ACF warnings and stack traces are retained in attempt logs but
do not count as progress, completion, locks, or authorization failures. JSON
results are schema-checked even with diagnostic text around them. A success
message cannot override a nonzero exit or an explicit command error.

Local command cleanup is separate from remote sync-state cleanup. On macOS
and Linux each command gets a private process group; lingering group members
are killed even when the main command exits successfully or closes its pipes.
A graceful termination still escalates if the main command exits before its
helpers. Every directly started command is waited on exactly once, including
on cancellation, so the supervisor does not leave its own exited children
unreaped. Output continues draining during shutdown.

This is not machine-wide process containment: descendants that deliberately
detach into another group, remote VIP workers, and a supervisor killed with
SIGKILL are outside this guarantee. Orphaned descendants are reaped by the
OS's adopting process (containers need a functioning init/reaper). Windows
uses best-effort `taskkill /T` for cancellation, not POSIX process groups.
The supervisor never scans for or kills unrelated machine processes.

## Tests

```sh
go test -race ./...
go vet ./...
go test ./internal/vipsearch -fuzz=FuzzParsers -fuzztime=10s
```

Tests use local fake CLI helper processes and loopback HTTP servers, never
real VIP sites or real ntfy endpoints. They cover
noisy and oversized output, string/numeric JSON fields, failed mutations,
scoped checkpoints, retry sequences, version safeguards, cancellation, and
output draining during graceful shutdown, TERM-ignoring group members,
leader-first exit, forced-stop races, and direct-child reaping. Local CLI
integration tests also check that helper PIDs disappear, including zombies.
Notification tests cover HTTP/auth,
timeouts, redirects, secret handling, retry rate limits, queue saturation,
final-alert ordering, settings persistence, UI configuration, and run outcomes.
Recovery tests cover exact-scope resume, completed-phase skipping, active or
unknown workers, changed versions/pins, corrupt or superseded history, runtime
revalidation, interrupted registration, and clock rollback. Milestone tests
cover count-based thresholds, retries/resumes, unknown totals, overflow, and
failed verification at apparent 100%.
CI runs tests before releases.

Non-retryable failures are recognised only on lines that *begin* with
`Error:`, `Fatal error:`, or `PHP Fatal error:`, so a post titled "Unauthorized
biography of a forbidden city", echoed back by verbose error output, cannot
abort the run.

## Layout

```
main.go                    entry point (TUI only; refuses to run without a terminal)
internal/vipsearch/        command construction + output parsing
  target.go                vip-CLI vs direct wp, running commands
  parse.go                 progress/fatal/lock parsing, JSON extraction
  client.go                typed read commands (status, versions, counts, …)
  status.go                schema-checked status/health JSON parsing
internal/childproc/        process-tree cancellation and termination per OS
internal/supervise/        the engine
  supervisor.go            phase/attempt loop, stall watchdog, backoff
  checkpoint.go            scoped, atomic checkpoint files
  versions.go              new-version create / verify / activate
  lock.go                  single-instance state-dir lock
  notifications.go         supervised lifecycle alerts
  history.go               private atomic run records and resume configuration
  recovery.go              read-only recovery assessment and runtime revalidation
internal/notify/           ntfy HTTP publisher, bounded queue, saved settings
internal/tui/              Bubble Tea front-end
  app.go                   screen stack (esc pops)
  screen_wizard.go         target → action → index wizard → confirm
  screen_run.go            live supervision dashboard
  screen_output.go         read-only commands, watch, unlock
  screen_notifications.go  optional ntfy configuration and test message
  screen_history.go        saved run browser, recovery details, resume confirmation
```
