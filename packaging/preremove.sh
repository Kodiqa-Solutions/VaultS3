#!/bin/sh
set -e
# Stop the service on removal, but never touch /var/lib/vaults3: taking a
# package off a machine must not delete the objects stored on it.
if command -v systemctl >/dev/null 2>&1; then
    systemctl stop vaults3 >/dev/null 2>&1 || true
    systemctl disable vaults3 >/dev/null 2>&1 || true
fi
