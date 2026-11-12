#!/usr/bin/env bash
# Example build script for an Android app. Copy it into your repo as
#   .push_builder/script.sh
# It runs from the repo root. Its only job is to build and to copy what
# should be downloadable into "$ARTIFACTS_DIR".
set -euo pipefail

# Pick one of the JDKs installed in the image (JAVA17_HOME or JAVA21_HOME).
export JAVA_HOME="$JAVA17_HOME"

# --no-daemon keeps Gradle inside this build, so a cancel or timeout stops it.
# ANDROID_KEYSTORE_FILE comes from the pre script; the passwords are build
# secrets added in the dashboard.
./gradlew --no-daemon assembleRelease \
  -Pandroid.injected.signing.store.file="$ANDROID_KEYSTORE_FILE" \
  -Pandroid.injected.signing.store.password="$KEYSTORE_PASSWORD" \
  -Pandroid.injected.signing.key.alias="$KEY_ALIAS" \
  -Pandroid.injected.signing.key.password="$KEY_PASSWORD"

cp app/build/outputs/apk/release/*.apk "$ARTIFACTS_DIR/"
