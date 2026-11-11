# GithubPushBuilder — Design Spec

Status: implemented (v1).
Date: 2026-11-11

## 1. Purpose

A self-hosted build service that runs in a single Docker container. It receives
GitHub push webhooks (which includes merged PRs), runs a build script that lives
in the pushed repo, and serves a web dashboard to check status, download
builds, and trigger or cancel builds.

## 2. Locked decisions

| Area | Decision |
|---|---|
| Language | Go, single binary |
| Packaging | One Docker container; SQLite, logs, artifacts and scripts on a `/data` volume |
| Toolchains | Baked into the container image (JDK, Android SDK, Node/Bun, etc.). No per-build images, no Docker socket |
| Trigger | GitHub `push` webhook, HMAC-verified, allowlisted repos and branches only |
| Network | Public fixed IP or public subdomain, reverse proxy with TLS in front |
| Build steps | Checkout -> container pre script -> repo script -> container post script |
| Repo config | None. The only contract is the repo script path and `$ARTIFACTS_DIR` |
| Login | Single admin password; only the webhook path is open without login |
| Parallel builds | Setting `max_parallel_builds`, default 1 |
| Waiting pushes kept | Setting `max_waiting_per_branch`, default 1 |
| Dashboard v1 | Status + live logs, artifact downloads, manual run, cancel, retry, Slack notifications |

Names still to be chosen (placeholders used below):

- Repo script path: `.our_container_dir/our_container_script.sh`
- Service/binary name: `GithubPushBuilder`

## 3. Build flow

1. **Webhook** — `POST /hooks/github` receives a `push` event. A merged PR
   always produces a push to the base branch, so `push` alone covers merges
   without double triggers.
2. **Validation** — verify `X-Hub-Signature-256` (HMAC SHA-256 with the
   per-repo webhook secret), check the repo and branch allowlist, drop duplicate
   `X-GitHub-Delivery` IDs, enqueue, and return `202` immediately (GitHub
   times out after 10 seconds).
3. **Queue** — persistent in SQLite, FIFO. Workers take builds up to
   `max_parallel_builds`.
4. **Checkout** — clone or fetch the repo into a clean per-build workspace at
   the pushed commit SHA.
5. **Check** — if `.our_container_dir/our_container_script.sh` is missing, fail
   the build immediately with a clear message.
6. **Pre script (defined in container)** — shared setup: export secrets and
   keystore, set `ANDROID_HOME`, point Gradle at the cache directory.
7. **Repo script** — run `.our_container_dir/our_container_script.sh` with the
   repo root as working directory.
8. **Post script (defined in container)** — collect artifacts, send Slack
   notification, set GitHub commit status, clean the workspace.

### Environment variables passed to all three scripts

| Variable | Meaning |
|---|---|
| `REPO` | `owner/name` |
| `BRANCH` | Branch that was pushed |
| `COMMIT_SHA` | Full commit SHA being built |
| `BUILD_ID` | Unique build number |
| `WORKSPACE` | Absolute path of the checkout |
| `ARTIFACTS_DIR` | Directory the repo script copies outputs into |
| `BUILD_STATUS` | Post script only: `success`, `failed`, `cancelled`, `timed_out` |

### Failure rules

- Pre script fails -> repo script is skipped, build is `failed`.
- Post script **always** runs, including on failure, cancel and timeout.
- Timeout: global default 30 minutes, overridable per repo in the dashboard.
  On timeout or cancel, kill the whole process group.

### Pre/post script location

- One global pair in `/data/scripts/pre.sh` and `/data/scripts/post.sh`.
- Optional per-repo override in `/data/scripts/<owner>/<repo>/pre.sh|post.sh`.
- Stored on the volume so they can be edited without rebuilding the image.

## 4. Queue behaviour

Build states: `queued`, `running`, `success`, `failed`, `cancelled`,
`timed_out`, `superseded`.

- **`max_waiting_per_branch` (default 1)** — applies per repo + branch. When a
  new push arrives and the limit is already reached, the oldest waiting build
  is marked `superseded` and stays visible in history. A running build is never
  cancelled by a newer push.
- **Manual runs** — do not count toward the waiting limit and are never
  superseded.
- **`max_parallel_builds` (default 1)** — each build gets its own workspace
  directory. Two builds of the same repo + branch never run at the same time
  (avoids Gradle cache and artifact clashes).
- Both settings are editable in the dashboard and apply from the next build,
  no restart.
- **Restart recovery** — on startup, re-queue builds that were `running`, and
  compare each allowlisted branch head (GitHub API) with the last built commit
  to catch webhooks missed while the service was down.

## 5. Dashboard (v1)

- **Status** — queue, running builds, history with result, duration, commit,
  branch and trigger (webhook or manual).
- **Live logs** — streamed with server-sent events; full log file kept per
  build.
- **Builds** — per-build artifact list and download, SHA-256 shown, retention
  of the last N builds per repo + branch (configurable).
- **Controls** — manual run for any registered repo + branch, cancel, retry.
- **Admin** — register repos and branches, webhook secrets, build secrets,
  Slack webhook URL, timeouts, the two queue settings.

## 6. Security

- Only `/hooks/github` is reachable without login; everything else requires
  the admin session.
- Admin password stored hashed (bcrypt or argon2), session cookie
  (`HttpOnly`, `Secure`, `SameSite`), login attempts rate-limited.
- Webhook secrets compared in constant time.
- Only pushes to allowlisted repos and branches trigger builds; never build
  fork PRs. The repo script has full access to the container and to the
  secrets the pre script exports.
- Build secrets stored on the server, injected only for repos allowed to use
  them, masked in logs.
- Repo access via a fine-grained GitHub token or deploy keys (read-only), plus
  commit-status write permission if statuses are enabled.

## 7. Notifications

- Slack incoming webhook on success and failure, with repo, branch, commit,
  duration and a link to the build page.
- GitHub commit status (`pending`, `success`, `failure`) on the built commit.

## 8. Suggested layout

```
/data
  db.sqlite
  scripts/pre.sh, post.sh, <owner>/<repo>/...
  workspaces/<build_id>/
  logs/<build_id>.log
  artifacts/<owner>/<repo>/<build_id>/
  cache/            # Gradle and other shared caches
```

## 9. Known trade-offs

- All repos share the toolchain versions installed in the image; a new
  toolchain means rebuilding the image. Installing several JDKs and letting
  the repo script choose eases this.
- Android builds are memory-heavy; raising `max_parallel_builds` above 1 needs
  enough RAM for multiple Gradle daemons.

## 10. Implementation order

1. Webhook endpoint, validation, SQLite queue
2. Script runner (checkout, pre, repo script, post), log files, timeout, cancel
3. Dashboard: login, status, live logs
4. Artifacts and retention, manual run and retry
5. Slack and GitHub commit status
6. Dockerfile with toolchains, restart recovery

## 11. Later candidates

Tag-triggered builds, scheduled builds, deploy steps, Play Store upload,
per-repo toolchain selection.
