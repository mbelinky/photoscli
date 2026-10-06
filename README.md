# photoscli

`photoscli` is a JSON CLI for Apple Photos album automation on macOS. It uses the documented Photos scripting interface and PhotoKit, with exact Photos local identifiers. It can list, read, create, populate, scan, and remove duplicate copies from albums. Album removal preserves the photos in the library. There is no library-photo deletion command.

## Build

```sh
go test ./...
./scripts/build.sh
```

The macOS build embeds its own Photos usage description and signs the binary.
It uses `PHOTOSCLI_SIGN_IDENTITY` when set, otherwise a keychain certificate
named `photoscli Code Signing` when one exists, otherwise an ad-hoc signature.
An ad-hoc signature changes with every build, so macOS asks for Photos access
again after each install; a stable certificate keeps the grant. Do not reuse
another application's identity or Photos permission.

## Authorization

The bridge sends Apple events to Photos. The responsible terminal or agent application needs macOS **Privacy & Security > Automation > Photos** permission. The unprobed status is deliberately `unknown`, because macOS cannot report this reliably without contacting Photos:

```sh
./bin/photoscli auth status --json
./bin/photoscli auth status --probe --timeout 45s --json
```

The probe is read-only, but it can launch Photos and can show the normal macOS Automation prompt. It never edits privacy settings. A locked GUI session can leave Photos running without answering Apple events; this is reported as a timeout.

For remote callers, install the background worker on the Mac that owns the
Photos library. The worker owns its Photos permission instead of inheriting
the permission of an SSH session or whichever app launched the CLI:

```sh
./scripts/install-worker.sh
photoscli auth photos --request --timeout 180s
photoscli auth status --probe --timeout 45s
```

Approve the standard macOS prompts while the desktop is unlocked. The first
command requests full Photos access for PhotoKit membership changes; the second
checks Automation access for the scripting commands. `photoscli auth photos`
reads the current PhotoKit status without prompting. Installing the worker does
not request either permission automatically. Ad-hoc rebuilds may require a new
Photos grant.

The worker serves a private Unix socket for the same macOS user and serializes
commands. Once configured, the CLI fails if the worker is unavailable, rather
than running under a different permission context. It never retries a mutation
after a lost connection. No TCP listener or remote shell endpoint is created.

## Albums

All output is JSON. Mutations are dry-runs by default and need `--execute`. Album and photo IDs are exact, opaque IDs from Photos; do not substitute filenames.

```sh
# List album names and IDs.
./bin/photoscli albums list --json

# Resolve one album. A duplicate name is an error; use its ID instead.
./bin/photoscli albums get --name "Road Trip" --json
./bin/photoscli albums get --id 'ALBUM-LOCAL-ID' --json

# Preview, then idempotently create by exact name.
./bin/photoscli albums create --name "Road Trip" --dry-run --json
./bin/photoscli albums create --name "Road Trip" --execute --json

# Preview, then idempotently add exact photo IDs. The file can be a JSON array
# or one ID per line. Every ID is validated before Photos receives the add.
./bin/photoscli albums add --album-id 'ALBUM-LOCAL-ID' --photo-ids-file photo-ids.json --dry-run --json
./bin/photoscli albums add --album-id 'ALBUM-LOCAL-ID' --photo-ids-file photo-ids.json --execute --json

# Preview, then import local files into an album. --files-file must contain a
# JSON array of paths. Every path is resolved and validated as a regular file
# before Photos receives the import request. Photos duplicate checking is on.
./bin/photoscli albums import --album-id 'ALBUM-LOCAL-ID' --files-file files.json --dry-run --json
./bin/photoscli albums import --album-id 'ALBUM-LOCAL-ID' --files-file files.json --execute --json
```

After each executed add or import, the CLI reads the album back and verifies membership for every returned photo ID. Repeating create or add is safe: an existing exact-name album is reused and already-present photos are skipped. Imports use Photos' duplicate checking by default. `--skip-duplicate-check` disables that check so unattended imports do not stop at Photos' duplicate dialog; callers must dedupe their files before using it. If Photos returns no imported photo IDs, the command reports an empty `importedPhotoIds` array rather than claiming files were imported. If a mutation times out, its outcome is reported as unknown; inspect the album before retrying.

## Duplicate comparison and album cleanup

```sh
# Export rendered previews and inspect exact matches and similar-image pairs.
photoscli albums duplicates --album-id 'ALBUM-LOCAL-ID' --timeout 20m

# Reuse previously exported JPEG/PNG files or the images from a previous scan.
photoscli albums duplicates --album-id 'ALBUM-LOCAL-ID' --images-file images.json

# After checking the pictures, preview removal of the unwanted copies.
photoscli albums remove --album-id 'ALBUM-LOCAL-ID' --photo-ids-file duplicates.json
photoscli albums remove --album-id 'ALBUM-LOCAL-ID' --photo-ids-file duplicates.json --execute
```

The scanner separates byte-identical and pixel-identical groups from perceptual
matches. Similar-looking pictures remain review candidates; they never trigger
automatic removal. Keeper suggestions favor original pixel dimensions, then
favorite status. Separate shots of the same subject should be kept.

The default scan exports rendered JPEGs into a new private directory under the
user's cache folder. `--output-dir DIR` chooses another parent directory. The
result includes preview paths and an `images.json` manifest for visual review.
Original Photos files are not modified. Duplicate scanning compares still
images. Items with known movie filename extensions are reported in
`skippedMedia` and `mediaCounts.videosSkipped` instead of being scanned. The
Photos scripting interface does not expose a media-type property, so the CLI
identifies these videos by filename and records every skip explicitly. The
optional manifest is a JSON array:

```json
[
  {"id":"PHOTO-LOCAL-ID","path":"/absolute/preview.jpg","filename":"IMG_0001.HEIC","width":4032,"height":3024,"favorite":false},
  {"id":"VIDEO-LOCAL-ID","filename":"IMG_0002.MOV","width":1920,"height":1080,"favorite":false,"skipped":true,"skipReason":"known_video_filename_extension"}
]
```

Manifest coverage is checked against the current album, including explicit
entries for skipped videos. A skipped entry must have a recognized movie
filename, the skip reason shown above, and no `path`. Missing or unreadable
still images produce an incomplete-scan error. JPEG EXIF rotation is normalized;
native HEIC containers in a supplied manifest need rendered previews first.
Ordinary HEIC library photos work through the default rendered-JPEG export.
Relative image paths in JSON manifests and import file lists resolve from the
manifest's directory, so the same files work through the background service.

`albums remove` requires exact IDs already present in the selected editable
user album. It validates the whole request before changing anything, then
verifies that the removed IDs left that album, every other member remains, and
all removed photos still exist in the library. Undo a removal by adding those
same IDs back. A timeout or failed readback requires inspection before retrying.

## Hidden photos and ledger

Use `photos hide` only after reviewing candidates. It changes the Photos Hidden
state, not the library contents, and is dry-run by default. Every exact ID is
validated before a change; repeating a hide or unhide reports IDs already in
the requested state. The command reads all requested IDs back after execution.

```sh
photoscli photos hide --photo-ids-file candidates.json --reason blurry --batch september --dry-run
photoscli photos hide --photo-ids-file candidates.json --reason blurry --batch september --execute
photoscli photos unhide --photo-id 'PHOTO-LOCAL-ID' --reason reviewed --execute
photoscli photos hidden list --ids-only > hidden-by-photoscli.json
```

Hide and unhide require PhotoKit to see hidden photos. When Photos locks the
Hidden album (Photos Settings, "Use Touch ID or password"), apps cannot see
hidden photos, so both commands stop with `hidden_album_locked` before
changing anything. A library with no hidden photos reports the same error.

Executed requests append one private JSON line per ID to
`~/Library/Application Support/photoscli/curation-ledger.jsonl`. `photos hidden
list` replays verified hide and unhide entries, so it lists only photos hidden
by this CLI. A photo that was already hidden before a hide request is recorded
with `changed: false` and never appears in this list. Use `--reason` for an exact reason match or `--since 30d` to limit
the current verified hides. A timeout after submission is recorded as
unverified and must be checked in Photos before retrying.
