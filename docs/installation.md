# Installation and updates

[Documentation](README.md) · [Project home](../README.md)

## Release archives

Download the archive matching your OS and CPU from the
[latest release](https://github.com/JohanLindvall/HeapLeach/releases/latest).
The [quick start](../README.md#quick-start) maps platforms to archive names
and shows extraction commands.

Extract the entire archive, including `ocr/`. Start `heapleach` on Linux or
macOS, or `heapleach.exe` on Windows. The web UI is embedded, so Go and Node.js
are not needed to run a release.

The Linux binaries use glibc and a native OCR runtime; use the supplied
Docker image on systems such as Alpine that use a different C library.
Docker builds from this checkout produce Linux executables, even when Docker
runs on macOS or Windows. For a host-native binary, use a release archive or
`make native` with the [development prerequisites](../CONTRIBUTING.md#development-setup).

Media helpers such as yt-dlp and ffmpeg are separate from the release
archives. The Docker image includes them. See [helpers](helpers.md) for
installation and Windows limitations.

## Verify a download

Download `SHA256SUMS` from the **same release** as the archive. Keep both in
one directory and compare the checksum before extracting.

On Linux, this checks the downloaded archives listed in the checksum file:

```bash
sha256sum --check --ignore-missing SHA256SUMS
```

On macOS, calculate the checksum and compare it with the matching filename
in `SHA256SUMS`:

```bash
shasum -a 256 heapleach_*_darwin_*.tar.gz
cat SHA256SUMS
```

In Windows PowerShell:

```powershell
Get-FileHash .\heapleach_*_windows_amd64.zip -Algorithm SHA256
Get-Content .\SHA256SUMS
```

Use a folder containing only the version you are checking. Check the full
hash, not just its beginning or end.

## Docker and persistent state

The [README Docker command](../README.md#docker) works on Linux and macOS.
It maps the API to loopback, runs as your user and keeps queue state under
`Downloads/.heapleach-state/`. The proxy database defaults to the same state
directory. Keep that directory private: the queue can contain source
passwords.

For Windows with Docker Desktop using Linux containers, use PowerShell:

```powershell
$downloads = Join-Path $env:USERPROFILE 'Downloads'
New-Item -ItemType Directory -Force -Path $downloads | Out-Null
docker run --rm -it --name heapleach `
  -p 127.0.0.1:8080:8080 `
  -e HEAPLEACH_STATE=/downloads/.heapleach-state/queue.json.zst `
  --mount "type=bind,source=$downloads,target=/downloads" `
  ghcr.io/johanlindvall/heapleach:latest
```

Open **http://localhost:8080**. To run in the background, replace `--rm -it`
with `-d`. Use `docker logs -f heapleach` to inspect logs and
`docker stop heapleach` to stop it cleanly. Remove a stopped container with
`docker rm heapleach` before creating a replacement with the same name;
the bind-mounted downloads and state stay on disk.

For separate state storage, mount a writable directory at `/state` and set
`HEAPLEACH_STATE=/state/queue.json.zst`. Set `HEAPLEACH_PROXY_DB` only if you
want the proxy inventory somewhere else. Each running instance needs its
own proxy database.

Published images support Linux amd64 and arm64. `latest` follows releases;
use a version tag such as `vX.Y.Z`, replacing it with an existing release
version, to pin an installation. Remote access requires the
[network precautions](usage.md#network-access-and-trust).

## Update

For an archive installation:

1. Stop HeapLeach cleanly so it saves its queue.
2. Download and verify the new archive.
3. Extract into a new application folder, keeping the new executable and its
   matching `ocr/` together. Keep your download and state directories.
4. Start the new executable. Unfinished jobs are held until you resume them.

For Docker, pull the image and recreate the container with the same mounts
and settings:

```bash
docker pull ghcr.io/johanlindvall/heapleach:latest
docker stop heapleach
```

Remove the stopped container if it was not started with `--rm`, then repeat
your `docker run` command. `HEAPLEACH_RESUME=1` opts an unattended service
into automatically resuming unfinished jobs.

## Build from source

See [CONTRIBUTING.md](../CONTRIBUTING.md#development-setup) for Docker,
native and hot-reload workflows. `make help` lists the available targets.

`make run-image DOWNLOADS=/mnt/media PORT=9000` builds the image from your
checkout and maps it to port 9000. This target publishes on all interfaces;
use it on a trusted network.

Media helpers are fetched during image builds and cached. To update those
helpers in a locally built image, rebuild the `runtime` target with
`docker build --no-cache --target runtime -t heapleach .`. OCR assets remain
pinned to the versions in the repository.
