# Claude read state and image handling

2026-11-11.

## Session read state

`claude.ReadTracker` is an explicit session-owned dependency. Construct one for each session, retain it across turns, and give the same instance to the Claude and generic adapters when switching between those dialects. Never share it between sessions. No state is added to `fsops`.

The adapter resolves the capability's absolute file path and invokes the capability inside `Read`, `Edit` or `Write`. A callback returns its capability error, not a rendered successful result. Typed `UnreadError` and `ChangedError` become the adapter's upstream refusal text. These types are not yet wired to captured tool schemas or verified output text.

- A successful Read records a SHA-256 fingerprint of the whole file even if the returned text is one line. Before and after fingerprints must match. A failed or unstable read grants no new authority.
- Edit requires the remembered content, inode, modification time and inode change time to match the current file. SHA-256 catches same-size overwrites with a restored mtime. Inode identity catches replacement by an identical file. Change time also catches recreation that reuses an inode and restores mtime.
- Successful Edit and Write refresh the fingerprint after the callback, including the new inode of an atomic replacement. Failed mutation callbacks invalidate the prior fingerprint because they may have changed the file before failing.
- Calls through one tracker are serialized. Trackers do not lock unrelated processes or sessions. A check before writing cannot prevent a process outside the session changing the file during the write. No claim of filesystem transaction isolation is made.
- Hashing streams file contents and retains only metadata and one digest per path, not file content. Fingerprinting a large file costs a full-file read before and after a Read and before and after an Edit.

Upstream source: the captured 2.1.280 `chunk-1xqpf2j8.js` Edit path updates `readFileState` with the successful edit's full new content and timestamp (`actualOldString`, `updatedFile`, `staleRecovered`, followed by `.set(he,{content:fv(Qt),timestamp:or,offset:void 0,limit:void 0...})`). The write path likewise refreshes the content and timestamp. This adapter uses fingerprints instead of retaining those contents. It deliberately rejects external changes even if upstream's stale-recovery heuristics might accept them, as required by the session editing rule.

Tests use actual files and `fsops.Read` / `ApplyEdits`: unread file refusal, external changes outside a returned one-line range, same-size overwrite with restored mtime, identical-content inode replacement, deletion/recreation and deletion, followed by re-read and consecutive successful edits. Independent tracker tests exercise a stale second session and write refreshes. Failed and unstable reads do not grant edit authority.

## Image dependency proposal, not added

The repository pins no image decoding or resizing library. The standard library handles PNG/JPEG/GIF, but not WebP, and `image/draw` provides no resampling scaler.

Proposed version: `golang.org/x/image v0.48.0`. Checked its primary upstream tag on 2026-11-11 at `go.googlesource.com/image`, including `go.mod`, `webp/decode.go` and `draw/scale.go`. Its Go directive is 1.26.0, compatible with this repository's Go 1.27.3. Its module requirements are `golang.org/x/sys v0.50.0` (already pinned here) and `golang.org/x/text v0.44.0` (this repository currently pins v0.42.0). The dependency decision therefore also needs to account for that text module upgrade. Nothing has been added or downloaded into this repository's module graph.

The upstream captured format path is `chunk-n26k3byr.js`, `oIe`, with accepted formats PNG/JPEG/GIF/WebP. Other decoded formats are converted to PNG. An image under the target byte and dimension limits is returned unchanged. PNG first tries palette compression at level 9, then JPEG at qualities 80, 60, 40, 20. Dimension scaling preserves aspect ratio, stays within 2000 by 2000 pixels without enlargement, and tries PNG or JPEG compression again. The final smaller fallback uses JPEG quality 20. The file-byte target is 3932160, with a base64 maximum of 5242880. Upstream uses sharp, not Go's encoders, so identical output bytes are not expected from x/image and stdlib encoders.

Proposed use: x/image WebP decoding and draw resampling, standard-library PNG/JPEG encoders for resized output. Preserve already acceptable GIF/WebP bytes rather than re-encoding them. Animated images and sharp's metadata/orientation behavior need recorded cases before claiming parity. Actual shrinking has not been implemented or checked.
