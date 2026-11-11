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

### Isolated dependency and footprint check

A scratch module pinned v0.48.0, outside the repository. The primary upstream image commit is `884d13f747188b6b8581f58ddb403e7a98874f3c`. Text v0.44.0 is `6070ac32abcdd1670599eeb3ee4d513c8184a513`, also Go 1.26.0. Resolving a copied root modfile with x/image added selects these upgrades: text v0.42.0 to v0.44.0, sync v0.23.0 to v0.25.0, tools v0.49.0 to v0.52.0 and mod v0.41.0 to v0.42.0. Sys stays v0.50.0. The root files were not changed. This checks module selection, not behavioral compatibility of the upgrades.

The existing graph's text requirements are v0.42.0 for x/net and the OTel exporters, v0.41.0 for grpc, and v0.40.0 for grpc-gateway, proto and genproto. The CLI imports text's secure/bidirule, transform, unicode/bidi and unicode/norm packages. The selected image packages (draw, math/f64, webp, riff, vp8, vp8l) themselves do not compile text packages.

Standalone Linux CGO-disabled binaries built with `-trimpath -ldflags='-s -w'` measured 1884320 bytes for a stdlib decode/JPEG baseline, 2150560 with WebP and CatmullRom scaling, and 2212000 with WebP and ApproxBiLinear scaling. The incremental sizes are 266240 and 327680 bytes respectively, not a measurement of the harness binary.

Using the real x/image 600 by 400 `blue-purple-pink-large` fixtures and encoding half-size output as JPEG quality 80, TotalAlloc deltas were:

| input and operation | bytes allocated |
|---|---:|
| PNG decode, no resize, JPEG baseline | 1235680 |
| PNG, CatmullRom resize to 300 by 200, JPEG | 5321720 |
| PNG, ApproxBiLinear resize to 300 by 200, JPEG | 1383200 |
| lossless WebP, ApproxBiLinear resize, JPEG | 1439824 |
| lossy WebP, ApproxBiLinear resize, JPEG | 683800 |

These are one-shot allocation deltas, not peak or retained heap. ApproxBiLinear avoids CatmullRom's larger temporary workspace in this check. The experiment exercises real decoding and shrinking but does not implement the full dimension and byte-budget policy, nor validate animation, orientation or upstream output parity.
