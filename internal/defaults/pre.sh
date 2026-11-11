#!/usr/bin/env bash
# Global pre script. Runs after checkout and before the repo script, with the
# repo root as working directory. If it exits non-zero the repo script is
# skipped and the build fails.
#
# Edit this file on the data volume; no image rebuild is needed.
# A repo can have its own version at  scripts/<owner>/<repo>/pre.sh
#
# Available: REPO BRANCH COMMIT_SHA BUILD_ID WORKSPACE ARTIFACTS_DIR
#            BUILD_TMP BUILD_ENV CACHE_DIR BUILD_TRIGGER BUILD_URL
#            plus every build secret this repo is allowed to use.
#
# This script and the repo script are separate processes, so `export` here
# does not reach the repo script. To pass a variable on, append one
# KEY=VALUE line per variable to the file "$BUILD_ENV".
set -euo pipefail

# Shared caches on the data volume, reused across builds.
mkdir -p "$CACHE_DIR/gradle" "$CACHE_DIR/npm" "$CACHE_DIR/bun"
{
  echo "GRADLE_USER_HOME=$CACHE_DIR/gradle"
  echo "npm_config_cache=$CACHE_DIR/npm"
  echo "BUN_INSTALL_CACHE_DIR=$CACHE_DIR/bun"
} >> "$BUILD_ENV"

# The Android SDK location is set in the image; pass it on under both names.
if [ -n "${ANDROID_HOME:-}" ]; then
  echo "ANDROID_SDK_ROOT=$ANDROID_HOME" >> "$BUILD_ENV"
fi

# Signing keystore: store it base64-encoded as the build secret
# ANDROID_KEYSTORE_BASE64. It is decoded into the per-build scratch
# directory, which is deleted when the build ends.
if [ -n "${ANDROID_KEYSTORE_BASE64:-}" ]; then
  keystore="$BUILD_TMP/release.keystore"
  printf '%s' "$ANDROID_KEYSTORE_BASE64" | base64 -d > "$keystore"
  chmod 600 "$keystore"
  echo "ANDROID_KEYSTORE_FILE=$keystore" >> "$BUILD_ENV"
fi
