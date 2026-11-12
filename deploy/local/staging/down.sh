#!/usr/bin/env bash
# down.sh [--purge]: remove the staging containers and pod. --purge also deletes
# the staging data dir (config, generated keys, rollback records).
set -euo pipefail
. "$(dirname "$0")/lib.sh"

ct_rm "$PROXY_CT"
ct_rm "$MOCK_CT"
if pod_exists "$POD"; then podman pod rm -f "$POD" >/dev/null; fi
if [ "${1:-}" = "--purge" ]; then
  guard_data_dir
  rm -rf "$DATA_DIR"
  echo "staging down, data dir removed"
else
  echo "staging down (data kept in $DATA_DIR)"
fi
