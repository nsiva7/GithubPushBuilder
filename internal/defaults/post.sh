#!/usr/bin/env bash
# Global post script. Always runs: after success, failure, cancel and timeout.
# BUILD_STATUS is one of: success, failed, cancelled, timed_out.
#
# Edit this file on the data volume; no image rebuild is needed.
# A repo can have its own version at  scripts/<owner>/<repo>/post.sh
#
# The service itself already does the following after this script, so there
# is no need to repeat them here:
#   - records every file in $ARTIFACTS_DIR (size + SHA-256) for download
#   - sends the Slack message and sets the GitHub commit status
#   - deletes the workspace and $BUILD_TMP
#
# Use this script for anything extra: copy more files into $ARTIFACTS_DIR,
# upload a build somewhere, call another service.
set -uo pipefail

echo "Build #$BUILD_ID of $REPO ($BRANCH) ended with status: $BUILD_STATUS"

if [ -d "$ARTIFACTS_DIR" ] && [ -n "$(ls -A "$ARTIFACTS_DIR" 2>/dev/null)" ]; then
  echo "Files in ARTIFACTS_DIR:"
  (cd "$ARTIFACTS_DIR" && find . -type f -printf '  %P (%s bytes)\n')
fi
exit 0
