#!/usr/bin/env bash
# images/portable.sh -- what the image scripts need on a host without GNU tools.
# Sourced, not run. On macOS the scripts run with these; on Linux with the real ones.

# file_size prints the size of a file in bytes.
file_size() { wc -c <"$1" | tr -d '[:space:]'; }

if ! command -v sha256sum >/dev/null 2>&1; then
  sha256sum() { shasum -a 256 "$@"; }
fi

# date_of prints a Unix time as UTC, in the format given as $2.
date_of() { date -u -d "@$1" "$2" 2>/dev/null || date -u -r "$1" "$2"; }
