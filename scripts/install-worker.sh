#!/bin/bash
set -euo pipefail
umask 077
if [[ "$(uname -s)" != Darwin ]]; then
  echo 'The Photos worker must be installed on the Mac that owns the Photos library.' >&2
  exit 1
fi
photoscli_root="$(cd -- "$(dirname -- "$0")/.." && pwd)"
photoscli_service_dir="$HOME/Library/Application Support/photoscli"
photoscli_binary="$HOME/.local/bin/photoscli"
photoscli_label=com.mbelinky.photoscli.worker
photoscli_domain="gui/$(id -u)"
photoscli_plist="$HOME/Library/LaunchAgents/$photoscli_label.plist"
if launchctl print "$photoscli_domain/$photoscli_label" >/dev/null 2>&1; then
  echo "The worker is already running. Finish outstanding commands and unload $photoscli_label before upgrading." >&2
  exit 1
fi
if [[ -L "$photoscli_service_dir" || -L "$photoscli_plist" || -L "$photoscli_binary" ]]; then
  echo 'Refusing to replace a symbolic link at a Photos worker installation path.' >&2
  exit 1
fi
"$photoscli_root/scripts/build.sh"
mkdir -p "$HOME/.local/bin" "$HOME/Library/LaunchAgents" "$photoscli_service_dir"
chmod 700 "$photoscli_service_dir"
install -m 755 "$photoscli_root/bin/photoscli" "$photoscli_binary"
photoscli_temp_plist="$(mktemp "$photoscli_service_dir/launchagent.XXXXXX")"
trap 'rm -f "$photoscli_temp_plist"' EXIT
plutil -create xml1 "$photoscli_temp_plist"
plutil -insert Label -string "$photoscli_label" "$photoscli_temp_plist"
plutil -insert ProgramArguments -array "$photoscli_temp_plist"
plutil -insert ProgramArguments.0 -string "$photoscli_binary" "$photoscli_temp_plist"
plutil -insert ProgramArguments.1 -string worker "$photoscli_temp_plist"
plutil -insert RunAtLoad -bool YES "$photoscli_temp_plist"
plutil -insert KeepAlive -bool YES "$photoscli_temp_plist"
plutil -insert ThrottleInterval -integer 10 "$photoscli_temp_plist"
plutil -insert ProcessType -string Interactive "$photoscli_temp_plist"
plutil -insert EnvironmentVariables -dictionary "$photoscli_temp_plist"
plutil -insert EnvironmentVariables.HOME -string "$HOME" "$photoscli_temp_plist"
plutil -insert EnvironmentVariables.PATH -string /usr/bin:/bin:/usr/sbin:/sbin "$photoscli_temp_plist"
plutil -insert StandardOutPath -string "$photoscli_service_dir/worker.log" "$photoscli_temp_plist"
plutil -insert StandardErrorPath -string "$photoscli_service_dir/worker-error.log" "$photoscli_temp_plist"
plutil -lint "$photoscli_temp_plist"
install -m 600 "$photoscli_temp_plist" "$photoscli_plist"
launchctl bootstrap "$photoscli_domain" "$photoscli_plist"
photoscli_ready=false
for ((photoscli_attempt=0; photoscli_attempt<50; photoscli_attempt++)); do
  if [[ -S "$photoscli_service_dir/worker.sock" ]]; then
    photoscli_ready=true
    break
  fi
  sleep 0.1
done
if [[ "$photoscli_ready" != true ]]; then
  echo "Worker did not create its socket. Inspect $photoscli_service_dir/worker-error.log; no CLI routing marker was created." >&2
  exit 1
fi
printf '1\n' > "$photoscli_service_dir/worker.enabled"
chmod 600 "$photoscli_service_dir/worker.enabled"
"$photoscli_binary" auth photos
echo 'Worker installed. Use photoscli auth photos --request while the desktop is unlocked to authorize Photos access.'
