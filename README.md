# GithubPushBuilder

[![CI](https://github.com/nsiva7/GithubPushBuilder/actions/workflows/ci.yml/badge.svg)](https://github.com/nsiva7/GithubPushBuilder/actions/workflows/ci.yml)
[![Docker pulls](https://img.shields.io/docker/pulls/nsiva77/githubpushbuilder)](https://hub.docker.com/r/nsiva77/githubpushbuilder)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

A small self-hosted build server in one Docker container. Push to GitHub (or
merge a PR), and it runs the build script that lives in your repo, then keeps
the outputs for download.

It is meant for a team that wants its own build machine — for example for
Android apps — without running a full CI system.

- **One container, one volume.** A single Go binary with SQLite. No Docker
  socket, no per-build images: the toolchains (JDK 17 and 21, Android SDK,
  Node.js, Bun) are in the image.
- **No config file in the repo.** The only contract is one script path and
  one output directory.
- **Dashboard.** Queue and history, live logs, artifact downloads with
  SHA-256, manual run, cancel and retry.
- **Safe by default.** Webhooks are HMAC-verified and limited to the repos and
  branches you register. Everything except the webhook needs the admin login.
- **Notifications.** Slack message and GitHub commit status per build.
- **Survives restarts.** Interrupted builds are queued again, and pushes
  missed while it was down are picked up.

Image: [`nsiva77/githubpushbuilder`](https://hub.docker.com/r/nsiva77/githubpushbuilder)
on Docker Hub (`linux/amd64`). The design is in [docs/SPEC.md](docs/SPEC.md).

## How a build runs

1. GitHub sends a `push` event to `POST /hooks/github`. The signature, the repo
   and the branch are checked, duplicates are dropped, and the build is queued.
2. The commit is checked out into a fresh workspace.
3. If the repo has no `.our_container_dir/our_container_script.sh`, the build
   fails right there with a message saying so.
4. The **pre script** runs (`/data/scripts/pre.sh`).
5. The **repo script** runs from the repo root.
6. The **post script** runs (`/data/scripts/post.sh`) — always, also after a
   failure, cancel or timeout.
7. The service records the files in `$ARTIFACTS_DIR`, sends the Slack message,
   sets the GitHub commit status and deletes the workspace.

If the pre script fails, the repo script is skipped and the build is `failed`.
On cancel or timeout the whole process group of the running script is killed.

## Quick start

With Docker Compose, using [docker-compose.yml](docker-compose.yml) from this
repo:

```bash
docker compose up -d
```

```bash
docker compose logs githubpushbuilder
```

Or with plain Docker:

```bash
docker run -d --name githubpushbuilder --restart unless-stopped -p 127.0.0.1:8080:8080 -v githubpushbuilder-data:/data -e GPB_BASE_URL=https://builds.example.com -e GPB_TRUST_PROXY=1 nsiva77/githubpushbuilder:latest
```

On the first start an admin password is generated and printed in the logs
(or set `GPB_ADMIN_PASSWORD` before the first start). Sign in and change it
under **Settings**.

The container listens on `127.0.0.1:8080`. Put a reverse proxy with TLS in
front of it. With Caddy this is enough:

```
builds.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

Then, in the dashboard:

1. **Settings** — set the public URL, and optionally the Slack webhook URL and
   a GitHub token.
2. **Repos → Add a repo** — enter `owner/name` and the branches that should
   build. The next page shows the webhook secret once.
3. On GitHub: repo **Settings → Webhooks → Add webhook**, with payload URL
   `https://builds.example.com/hooks/github`, content type `application/json`,
   the secret, and "Just the push event".
4. Add `.our_container_dir/our_container_script.sh` to the repo
   ([example](examples/our_container_script.sh)) and push.

## The repo script

There is no config file in the repo. The contract is the script path and
`$ARTIFACTS_DIR`: the script builds, and copies whatever should be
downloadable into that directory.

Variables available to all three scripts:

| Variable | Meaning |
|---|---|
| `REPO` | `owner/name` |
| `BRANCH` | Branch that was pushed |
| `COMMIT_SHA` | Full commit SHA being built |
| `BUILD_ID` | Build number |
| `WORKSPACE` | Absolute path of the checkout |
| `ARTIFACTS_DIR` | Where the repo script copies its outputs |
| `BUILD_STATUS` | Post script only: `success`, `failed`, `cancelled`, `timed_out` |
| `BUILD_TMP` | Scratch directory, deleted when the build ends |
| `BUILD_ENV` | File for passing variables to later scripts (see below) |
| `CACHE_DIR` | `/data/cache`, shared between builds |
| `BUILD_TRIGGER` | `webhook`, `manual`, `retry` or `recovery` |
| `BUILD_URL` | Dashboard page of the build |
| `CI` | `true` |

Build secrets that the repo may use are added as environment variables too.
Toolchain variables come from the image: `JAVA_HOME`, `JAVA17_HOME`,
`JAVA21_HOME`, `ANDROID_HOME`.

## Pre and post scripts

They live on the data volume, so they can be edited without rebuilding the
image. Starter versions are written on the first start and never overwritten.

- Global: `/data/scripts/pre.sh` and `/data/scripts/post.sh`
- Per repo, replacing the global one: `/data/scripts/<owner>/<repo>/pre.sh`
  and `post.sh`

The three scripts are separate processes, so `export` in the pre script does
not reach the repo script. To hand a variable on, append a `KEY=VALUE` line to
the file `$BUILD_ENV`:

```bash
echo "GRADLE_USER_HOME=$CACHE_DIR/gradle" >> "$BUILD_ENV"
```

The starter pre script does this for the Gradle, npm and Bun caches, and
decodes a keystore from the secret `ANDROID_KEYSTORE_BASE64` if it exists.

## Queue rules

- **Parallel builds** (`max_parallel_builds`, default 1). Two builds of the
  same repo and branch never run at the same time.
- **Waiting pushes kept per branch** (`max_waiting_per_branch`, default 1).
  When a push arrives and the limit is reached, the oldest waiting push for
  that repo and branch becomes `superseded` and stays in the history. A
  running build is never stopped by a newer push.
- Manual runs and retries don't count toward that limit and are never
  superseded.
- Both settings are changed in the dashboard and apply from the next build.
- **Restart**: builds that were running are queued again. Each registered
  branch that was built before is compared with its head on the remote, and a
  build is queued if a push was missed while the service was down.

Build states: `queued`, `running`, `success`, `failed`, `cancelled`,
`timed_out`, `superseded`.

## Configuration

Everything that changes often is in the dashboard. The container itself reads:

| Variable | Default | Meaning |
|---|---|---|
| `GPB_DATA_DIR` | `/data` | Database, scripts, logs, artifacts, caches |
| `GPB_LISTEN` | `:8080` | Listen address |
| `GPB_BASE_URL` | | Public URL; the dashboard setting wins when set |
| `GPB_ADMIN_PASSWORD` | | Admin password for the first start only |
| `GPB_TRUST_PROXY` | off | Set to `1` behind a reverse proxy, so login rate limiting sees the real client address |
| `GPB_INSECURE_COOKIE` | off | Set to `1` to sign in over plain http. Local testing only |
| `GPB_REPO_SCRIPT` | `.our_container_dir/our_container_script.sh` | Script path inside a repo |
| `GPB_GITHUB_API_URL` | `https://api.github.com` | For GitHub Enterprise |

Lost the password:

```bash
docker compose exec githubpushbuilder githubpushbuilder set-password
```

### Access to private repos

Either a fine-grained GitHub token (read access to Contents, plus read/write
to Commit statuses if statuses are on), set globally under Settings or per
repo; or a deploy key: mount it into `/home/builder/.ssh` and set the repo's
clone URL to `git@github.com:owner/name.git`.

The token is used only by the service's own `git` and API calls. It is not
passed to build scripts.

## Data volume

```
/data
  db.sqlite
  scripts/pre.sh, post.sh, <owner>/<repo>/...
  workspaces/<build_id>/
  logs/<build_id>.log
  artifacts/<owner>/<repo>/<build_id>/
  cache/            # Gradle and other shared caches, and the git cache
  tmp/<build_id>/   # $BUILD_TMP
```

## Security notes

- Only `/hooks/github` and the login form answer without a session.
- The webhook checks `X-Hub-Signature-256` in constant time against the
  repo's secret, and only builds pushes to registered repos and branches.
  Pull request events are ignored, so code from forks is never built.
- The repo script runs with full access to the container and to the secrets
  its repo may use. Register only repos whose pushers you trust.
- The admin password is stored as a bcrypt hash. Failed logins are limited to
  5 per 15 minutes per address.
- Build secrets, tokens and the Slack URL are stored in `db.sqlite` as plain
  text. Protect the volume and its backups accordingly.
- Secret values are masked in build logs. Values shorter than 4 characters
  are not masked, and a script that transforms a secret (for example
  base64-encodes it) before printing it defeats the masking.

## Toolchains

The image carries JDK 17 and 21, Node.js, Bun, and the Android SDK (platform
and build tools set by build arguments in the [Dockerfile](Dockerfile)).
A new toolchain or version means rebuilding the image. The image is built for
`linux/amd64`, because the Android build tools are x86-64 only.

To build the image yourself, for example with other toolchain versions:

```bash
docker build --platform linux/amd64 --build-arg NODE_VERSION=26.13.1 -t githubpushbuilder .
```

Run Gradle with `--no-daemon`. A Gradle daemon detaches from the build and
can survive a cancel or timeout.

## Development

Requires Go 1.26 or newer and `git`.

```bash
make test
```

```bash
make dev
```

`make dev` serves on `http://127.0.0.1:8080` with its data in `./data`, and
prints a generated admin password on the first run.

| Path | Contents |
|---|---|
| `cmd/githubpushbuilder` | Entry point, `serve` and `set-password` |
| `internal/store` | SQLite: settings, repos, queue, artifacts, secrets, sessions |
| `internal/webhook` | `POST /hooks/github` |
| `internal/engine` | Scheduler, checkout, script runner, logs, retention, recovery |
| `internal/notify` | Slack and GitHub commit status |
| `internal/web` | Dashboard, login, live log stream |
| `internal/defaults` | Starter pre and post scripts |

## License

[MIT](LICENSE)
