# syntax=docker/dockerfile:1
#
# One image: the service binary plus every toolchain builds may need.
# There are no per-build images, so a new toolchain means rebuilding this one.
# Build for linux/amd64: the Android SDK build tools are x86-64 only.

FROM golang:1.27-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/githubpushbuilder ./cmd/githubpushbuilder

FROM ubuntu:24.04

# Toolchain versions. Change and rebuild to upgrade.
ARG NODE_VERSION=26.13.1
ARG BUN_VERSION=latest
ARG ANDROID_CMDLINE_TOOLS=13114758
ARG ANDROID_PLATFORM=android-36
ARG ANDROID_BUILD_TOOLS=36.0.0

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
      bash ca-certificates curl git git-lfs jq openssh-client tini unzip xz-utils zip \
      openjdk-17-jdk-headless openjdk-21-jdk-headless \
 && rm -rf /var/lib/apt/lists/* \
 # Arch-independent JDK paths, so repo scripts can pick one: JAVA_HOME="$JAVA17_HOME"
 && mkdir -p /opt/java \
 && ln -s "/usr/lib/jvm/java-17-openjdk-$(dpkg --print-architecture)" /opt/java/17 \
 && ln -s "/usr/lib/jvm/java-21-openjdk-$(dpkg --print-architecture)" /opt/java/21
ENV JAVA17_HOME=/opt/java/17 \
    JAVA21_HOME=/opt/java/21 \
    JAVA_HOME=/opt/java/21

# Node.js, checked against the published checksums.
RUN set -eu; \
    case "$(dpkg --print-architecture)" in amd64) arch=x64 ;; arm64) arch=arm64 ;; *) echo "unsupported architecture" >&2; exit 1 ;; esac; \
    file="node-v${NODE_VERSION}-linux-${arch}.tar.xz"; cd /tmp; \
    curl -fsSLO "https://nodejs.org/dist/v${NODE_VERSION}/${file}"; \
    curl -fsSL "https://nodejs.org/dist/v${NODE_VERSION}/SHASUMS256.txt" | grep " ${file}\$" | sha256sum -c -; \
    tar -xJf "${file}" -C /usr/local --strip-components=1 --no-same-owner; \
    rm "${file}"; node --version

# Bun.
RUN set -eu; \
    case "$(dpkg --print-architecture)" in amd64) arch=x64 ;; arm64) arch=aarch64 ;; esac; \
    if [ "${BUN_VERSION}" = latest ]; then url="https://github.com/oven-sh/bun/releases/latest/download"; \
    else url="https://github.com/oven-sh/bun/releases/download/bun-v${BUN_VERSION}"; fi; \
    curl -fsSL -o /tmp/bun.zip "${url}/bun-linux-${arch}.zip"; \
    unzip -q -j /tmp/bun.zip -d /usr/local/bin; rm /tmp/bun.zip; \
    ln -s bun /usr/local/bin/bunx; bun --version

# Builds run as an unprivileged user.
RUN (userdel -r ubuntu 2>/dev/null || true) \
 && useradd --create-home --uid 1000 --shell /bin/bash builder \
 && mkdir -p /data /opt/android-sdk \
 && chown builder:builder /data /opt/android-sdk

# Android SDK, owned by the build user so Gradle can add missing components.
ENV ANDROID_HOME=/opt/android-sdk
ENV PATH="${PATH}:${ANDROID_HOME}/cmdline-tools/latest/bin:${ANDROID_HOME}/platform-tools"
USER builder
RUN set -eu; \
    mkdir -p "${ANDROID_HOME}/cmdline-tools"; \
    curl -fsSL -o /tmp/clt.zip "https://dl.google.com/android/repository/commandlinetools-linux-${ANDROID_CMDLINE_TOOLS}_latest.zip"; \
    unzip -q /tmp/clt.zip -d "${ANDROID_HOME}/cmdline-tools"; rm /tmp/clt.zip; \
    mv "${ANDROID_HOME}/cmdline-tools/cmdline-tools" "${ANDROID_HOME}/cmdline-tools/latest"; \
    yes | sdkmanager --licenses > /dev/null; \
    sdkmanager "platform-tools" "platforms;${ANDROID_PLATFORM}" "build-tools;${ANDROID_BUILD_TOOLS}"

COPY --from=build /out/githubpushbuilder /usr/local/bin/githubpushbuilder

ENV GPB_DATA_DIR=/data \
    GPB_LISTEN=:8080
VOLUME /data
EXPOSE 8080

# /login is the only dashboard page that answers without a session.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s \
  CMD curl -fsS -o /dev/null http://127.0.0.1:8080/login || exit 1

# tini reaps processes that builds leave behind.
ENTRYPOINT ["/usr/bin/tini", "--", "githubpushbuilder"]
CMD ["serve"]
