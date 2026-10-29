# CI and releases

The workflows are in `.github/workflows/`, their scripts in
`.github/scripts/`.

## Main images and checks

Every push to `main` runs `.github/workflows/ci.yml`. Its shared
`checks.yml` workflow builds the UI, runs formatting, vet, inventory and
license checks, tests Go and the UI, and exercises the real OCR model on
Linux. Pull requests and manually dispatched CI runs perform those checks
without publishing. Ordinary CI does not build native release archives.

Successful main pushes call `image.yml`, which builds the runtime image from
source for Linux amd64 and arm64 and publishes `latest` and `sha-<full SHA>`
to `ghcr.io/johanlindvall/heapleach`. It never creates tags or releases.
`.github/scripts/image-version.sh` stamps the binary and OCI version label
as `<latest stable tag>-g<12-character SHA>`, including when the commit is
itself tagged. It selects the highest stable `vX.Y.Z` tag reachable from the
built commit; with no such tag it uses `v0.0.0`. Full Git history is required.

Image publication is serialized, and a queued run checks that its commit is
still main's tip before building, so late runs cannot replace a newer image.
The Dockerfile compiles the UI and Go on the builder's platform and uses a
target C compiler for cross-builds. QEMU handles the runtime package install;
local arm64 image builds also need binfmt. Main images compile from source;
manual release images use the verified published archives instead.

## Manual versions and releases

Neither pushing a commit nor pushing a tag allocates a version or publishes
a release. **Update version** (`version.yml`) is the manual entry point for
new releases; dispatch it on `main` after CI passes for that exact commit.
Its optional `version` input defaults to incrementing the highest stable
`vX.Y.Z` tag's patch, or `v0.0.1` when no stable tags exist. An explicit
`version=vX.Y.Z` overrides the default. `make tag` dispatches the default;
`make tag V=vX.Y.Z` supplies an override. Both require `gh` and a clean
checkout at remote main.

`.github/scripts/release-tag.sh` creates an annotated tag after checking
its format, ancestry and ordering. It refuses to move an existing tag.
Each workflow run records its ID in the tag annotation: reruns recover
their original version from Git even on a fresh runner or after newer tags
exist. A new dispatch has a new run ID and increments the patch even at
the same commit. The version workflow is serialized through publication,
so another update cannot allocate a version while it is building.

After tagging, Update version **always calls Release** (`release.yml`) with
the selected tag. It calls the reusable workflow directly because tags
pushed with `GITHUB_TOKEN` do not start another workflow. Release can also
be dispatched manually with `tag=vX.Y.Z` to rebuild an existing version.
Both entry points use the same publication queue.

Release resolves the tag on main's history and pins every checkout to its
commit. It runs `checks.yml` plus native archive builds and packaged OCR
tests on Linux amd64/arm64, macOS Intel/Apple silicon and Windows amd64.
All must pass before publication. A tag push alone does not start this work.

The archive cache is keyed by toolchain, dependencies and the selected
commit, with a fallback to previous compilation output. Native macOS builds
use `macos-15-intel` and `macos-15`, since cgo and the bundled OCR library
must match the target architecture.

Publication requires all five archives, creates `SHA256SUMS`, and uploads to
a draft release before verifying the asset count and publishing it. Release
notes use GitHub's generated changelog. A retry reuses the draft and replaces
partial uploads.

The release image job downloads the published Linux archives, checks their
checksums and amd64 version, and selects Docker's `released-binary` stage
with `BINARY_STAGE`. It publishes `vX.Y.Z`, `X.Y.Z`, `vX.Y` and `X.Y` image
tags. Docker metadata's automatic `latest` tag is explicitly disabled:
only main's image publisher may move `latest`.

`make test-release` exercises read-only image version selection, default
patch bumps, explicit overrides, retries and invalid requests against
temporary Git remotes. It never changes this repository's tags.

## Dependabot

Dependabot (`.github/dependabot.yml`) proposes grouped Go module updates each
Monday at 05:00 Stockholm time. Once a Dependabot pull request passes CI,
`dependabot-merge.yml` squashes only the tested commit into main. Nothing
from that pull request is checked out or run with the merge token.

A merge made with `GITHUB_TOKEN` starts no push workflow, so the merger
explicitly dispatches CI on main. CI accepts that bot dispatch as the main
push's substitute and publishes its image after checks pass. It never
updates the version or creates a release. Grouping avoids repeated image
rollouts and conflicts between related dependency updates. `onnxruntime_go`
is excluded because it must match the runtime pinned in
`helpers/captcha/install.sh`; those move together by hand.

## Checklist

For release-related changes and manual publication:

1. Read `gh run list` and verify successful CI on the **exact commit** being
   tagged. An empty filtered result is not evidence of success. Review
   `git log <last tag>..main`, including the authors of unfamiliar commits.
   The version workflow enforces the CI requirement, and the release workflow
   repeats the checks on the selected tag's commit.
2. Run `make dist`, extract the Linux amd64 archive, check `-version`, and
   run `sha256sum -c SHA256SUMS` from `dist`. A local rehearsal uses
   `git describe`, so `<last tag>-N-g<sha>-dirty` is expected before committing.
3. Run `make frontend-clean` and remove the generated `dist` staging before
   committing. This restores the tracked embed placeholder.
4. After publishing, verify what the workflow actually uploaded. For a main
   image, pull it and check `-version` and the OCI revision/version labels.
   For a manual release, download its published archive and check the
   checksum and `-version`. Local rehearsal alone does not prove that the
   published artifact contains the change.
