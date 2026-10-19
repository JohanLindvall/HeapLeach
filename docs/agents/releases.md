# CI and releases

The workflows are in `.github/workflows/`, their scripts in
`.github/scripts/`.

## Automatic releases

Every push to `main` that passes CI calls `.github/workflows/release.yml`,
which creates an annotated tag for that exact commit by incrementing the
highest stable tag's patch version. Publication is serialized, retries reuse
the current tag, and late CI runs for commits already in a newer release skip
publishing. Pull-request runs perform checks only. The caller invokes the
publisher directly, because tags pushed with `GITHUB_TOKEN` do not trigger
another workflow. `.github/scripts/release-tag.sh` selects the version, and
`make test-release` tests it against a temporary Git remote.

Native archive builds and OCR model tests run on all five platforms alongside
the main checks, and all of them gate publication. The caller passes its
tested SHA, and the publisher verifies its checkout against it before reusing
those results. A standalone tag push runs the Go suite itself. Release
compilation has a cache of its own, keyed by toolchain, dependencies and
commit, which restores the previous commit's output and saves the new one;
CI's immutable `go.sum` cache would leave every cross-platform build cold.

## Dependabot

Dependabot (`.github/dependabot.yml`) proposes the Go modules' updates every
Monday at 05:00 Stockholm time, a quiet hour, grouped into one pull request,
and the pull request lands by itself. Once its CI passes,
`dependabot-merge.yml` squashes it into main. A merge made with the workflow's
own token starts no workflow, so the workflow then starts CI on main itself
(`workflow_dispatch`, which CI's release job accepts as it does a push), and
that run releases it. Only Dependabot's own pull requests are merged, only at
the commit CI passed, and nothing from them is checked out or run with that
token. Each merge is a release and a restart, which is why the updates come as
one a week rather than one per module. `onnxruntime_go` is left out: it must
match the native runtime that `helpers/captcha/install.sh` pins, so the two
move together by hand.

## Manual tags and published artifacts

`make tag V=v1.2.3` writes and pushes an explicit annotated tag, and the
release workflow also runs on `v*` tags. Native runners build the five
archives, then the publisher uploads them alongside `SHA256SUMS`. The image
job waits for publication, then pushes the runtime image for amd64 and arm64
to `ghcr.io/johanlindvall/heapleach` as `vX.Y.Z` and `X.Y.Z` (the git tag's
spelling and Docker's), `vX.Y` and `X.Y`, and `latest`. The image job
downloads the published Linux archives, verifies their checksums and amd64
version, and selects Docker's `released-binary` stage with `BINARY_STAGE`, so
both release formats carry the same executable and UI.

Local image builds default to the `backend` stage: the UI and Go stages run on
the builder's platform (`--platform=$BUILDPLATFORM`) and Go uses a target C
compiler for cross-builds. QEMU emulates the runtime package install, so an
arm64 image needs binfmt locally; CI's QEMU setup step supplies it.

Release notes are GitHub's auto-generated changelog link; the `tag` target
authors no body. The tag target refuses a dirty tree, which is what makes step
3 below necessary rather than tidy.

## Checklist

For release-related changes and manual tags, do each of these; each catches a
failure the others miss.

1. `gh run list` — CI green on the **exact commit** being tagged, not merely
   somewhere on the branch. Check who authored anything unfamiliar in
   `git log <last tag>..main` rather than assuming it is yours. Read the plain
   list rather than filtering: `gh run list --commit <sha>` prints nothing
   when it matches nothing, which looks exactly like a commit CI never ran. An
   empty result is not evidence of a green build. Automatic releases enforce
   this through the CI job dependency.
2. `make dist`, then extract the linux/amd64 archive, run `-version` and
   `sha256sum -c`. This is a rehearsal on the host, so its binaries are
   stamped `<last tag>-N-g<sha>-dirty` — that is `git describe` before the tag
   exists, not a fault.
3. `make frontend-clean && rm -rf dist` — restores the embed placeholder and
   clears the staging, so `make tag`'s clean-tree check passes honestly
   instead of being bypassed.
4. Once the workflow finishes, download the **published** artifact and check
   its checksum and `-version`. Verifying the local rehearsal proves nothing
   about what CI actually uploaded. Where a release exists for one
   identifiable change, grep the binary for it: the embedded UI is not
   compressed, so `grep -c scrollbar-gutter heapleach` settles whether a fix
   shipped instead of trusting that the build picked it up.
