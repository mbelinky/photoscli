#!/bin/bash
# Rebuild and reinstall photoscli from this checkout when a request file
# appears. Runs as a LaunchAgent in the desktop session, where the login
# keychain that holds the signing certificate is unlocked, so remote callers
# never need to sign over SSH.
#
# Upgrade path: the new binary replaces the old one by atomic rename, then the
# running worker gets SIGTERM. The worker serves one request at a time and
# finishes the request in hand before it exits; launchd's KeepAlive then
# starts the new binary. The worker is never unloaded, so no in-flight Photos
# change is cut short. Callers that arrive during the restart get
# worker_unavailable before anything is submitted.
set -uo pipefail
umask 077
photoscli_root="$(cd -- "$(dirname -- "$0")/.." && pwd)"
service_dir="$HOME/Library/Application Support/photoscli"
requests="$service_dir/rebuild-requests"
responses="$service_dir/rebuild-responses"
lock="$service_dir/rebuild.lock"
binary="$HOME/.local/bin/photoscli"
label=com.mbelinky.photoscli.worker
target="gui/$(id -u)/$label"
mkdir -p "$requests" "$responses"
mkdir "$lock" 2>/dev/null || exit 0
trap 'rmdir "$lock"' EXIT

worker_pid() {
  launchctl print "$target" 2>/dev/null | awk '$1 == "pid" && $2 == "=" {print $3; exit}'
}

# Replace the binary atomically and restart the worker on it. Succeeds only
# when a new worker process is running and answers.
switch_to() {
  local source="$1" old_pid new_pid
  install -m 755 "$source" "$binary.incoming" || return 1
  mv -f "$binary.incoming" "$binary" || return 1
  cmp -s "$source" "$binary" || return 1
  old_pid="$(worker_pid)"
  launchctl kill SIGTERM "$target" || return 1
  # Allow a long import to finish; never force-kill the worker.
  for _ in $(seq 1 1800); do
    new_pid="$(worker_pid)"
    if [[ -n "$new_pid" && "$new_pid" != "$old_pid" ]]; then break; fi
    sleep 1
  done
  [[ -n "$new_pid" && "$new_pid" != "$old_pid" ]] || return 1
  for _ in $(seq 1 100); do
    if "$binary" auth photos --json 2>/dev/null | grep -q '"ok":true'; then return 0; fi
    sleep 0.2
  done
  return 1
}

for request in "$requests"/*.request; do
  [[ -e "$request" ]] || continue
  id="$(basename "$request" .request)"
  [[ "$id" =~ ^[A-Za-z0-9._-]+$ ]] || { rm -f "$request"; continue; }
  rm -f "$request"
  log="$responses/$id.log"
  backup="$binary.before-$id"
  ok=false
  {
    echo "rebuild $id at $(date -u +%FT%TZ) from $(git -C "$photoscli_root" rev-parse --short HEAD)"
    if ! cp -p "$binary" "$backup" || ! cmp -s "$binary" "$backup"; then
      echo "could not back up the installed binary; nothing was changed"
    elif ! "$photoscli_root/scripts/build.sh" || ! codesign --verify --strict "$photoscli_root/bin/photoscli"; then
      echo "build or signature check failed; the installed binary is unchanged"
    elif switch_to "$photoscli_root/bin/photoscli"; then
      ok=true
      echo "new worker $(worker_pid) is running and answers"
    else
      echo "new build did not come up; restoring the previous binary"
      if switch_to "$backup"; then
        echo "previous binary restored; worker $(worker_pid) answers"
      else
        echo "RESTORE FAILED: reinstall $backup by hand"
      fi
    fi
    $ok && rm -f "$backup"
  } >"$log" 2>&1
  /usr/bin/python3 - "$responses/$id.json" "$ok" "$log" "$binary" <<'PY'
import hashlib, json, os, sys
path, ok, log, binary = sys.argv[1:5]
tail = open(log, encoding="utf-8", errors="replace").read()[-2000:]
digest = hashlib.sha256(open(binary, "rb").read()).hexdigest()
tmp = path + ".tmp"
with open(tmp, "w") as handle:
    json.dump({"ok": ok == "true", "binary_sha256": digest, "log_tail": tail}, handle)
os.replace(tmp, path)
PY
done
