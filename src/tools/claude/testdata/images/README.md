# Image fixtures

Unmodified upstream fixture files, not hand-written provider responses.

- `blue-purple-pink-large.png`, `blue-purple-pink-large.lossless.webp`, `blue-purple-pink-large.normal-filter.lossy.webp` and `go-turns-two-280x360.jpeg` are from `golang.org/x/image v0.48.0/testdata`, upstream commit `884d13f747188b6b8581f58ddb403e7a98874f3c`.
- `video-001.gif` is from the pinned Go 1.27.3 distribution's `src/image/testdata`.

Both upstream projects use the Go Authors' BSD license copied here as `LICENSE`. Tests transform the PNG fixture to exercise a source larger than the captured 2000-pixel output limit. These fixtures test decoding, shrinking and budget enforcement, not sharp byte parity.
