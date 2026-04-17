#!/bin/sh
set -eu

# Volume mounts start out root-owned; fix so the non-root user can write.
mkdir -p /state
chown -R nobody:nogroup /state 2>/dev/null || true

exec gosu nobody:nogroup "$@"
