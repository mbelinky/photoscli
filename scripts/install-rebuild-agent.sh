#!/bin/bash
# Install the LaunchAgent that rebuilds photoscli on request. Remote callers
# drop NAME.request into ~/Library/Application Support/photoscli/rebuild-requests
# and read rebuild-responses/NAME.json.
set -euo pipefail
umask 077
photoscli_root="$(cd -- "$(dirname -- "$0")/.." && pwd)"
service_dir="$HOME/Library/Application Support/photoscli"
label=com.mbelinky.photoscli.rebuild
plist="$HOME/Library/LaunchAgents/$label.plist"
domain="gui/$(id -u)"
mkdir -p "$service_dir/rebuild-requests" "$service_dir/rebuild-responses" "$HOME/Library/LaunchAgents"
chmod +x "$photoscli_root/scripts/rebuild-agent.sh"
# Hold the rebuild lock while the agent is replaced, so no rebuild can start
# or be cut short in between; a request that arrives meanwhile waits and is
# picked up when the new agent loads.
if ! mkdir "$service_dir/rebuild.lock" 2>/dev/null; then
  echo "A rebuild is running; try again when $service_dir/rebuild.lock is gone." >&2
  exit 1
fi
trap 'rmdir "$service_dir/rebuild.lock" 2>/dev/null; rm -f "${temp:-}"' EXIT
launchctl bootout "$domain/$label" 2>/dev/null || true
temp="$(mktemp "$service_dir/rebuild-agent.XXXXXX")"
plutil -create xml1 "$temp"
plutil -insert Label -string "$label" "$temp"
plutil -insert ProgramArguments -array "$temp"
plutil -insert ProgramArguments.0 -string "$photoscli_root/scripts/rebuild-agent.sh" "$temp"
plutil -insert WatchPaths -array "$temp"
plutil -insert WatchPaths.0 -string "$service_dir/rebuild-requests" "$temp"
plutil -insert RunAtLoad -bool YES "$temp"
plutil -insert ProcessType -string Interactive "$temp"
plutil -insert EnvironmentVariables -dictionary "$temp"
plutil -insert EnvironmentVariables.HOME -string "$HOME" "$temp"
plutil -insert EnvironmentVariables.PATH -string "/opt/homebrew/bin:/usr/local/go/bin:/usr/bin:/bin:/usr/sbin:/sbin" "$temp"
plutil -insert StandardOutPath -string "$service_dir/rebuild-agent.log" "$temp"
plutil -insert StandardErrorPath -string "$service_dir/rebuild-agent.log" "$temp"
plutil -lint "$temp"
install -m 600 "$temp" "$plist"
# Release the lock and stop owning it, so the exit trap cannot remove a lock
# the new agent may already hold.
rmdir "$service_dir/rebuild.lock"
trap 'rm -f "${temp:-}"' EXIT
launchctl bootstrap "$domain" "$plist"
echo "Rebuild agent installed: touch '$service_dir/rebuild-requests/NAME.request'"
