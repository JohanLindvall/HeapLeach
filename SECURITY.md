# Security policy

## Reporting a vulnerability

Please report vulnerabilities privately, through GitHub's private
vulnerability reporting:
**[Report a vulnerability](https://github.com/JohanLindvall/HeapLeach/security/advisories/new)**
(the *Security* tab → *Report a vulnerability*). Do not open a public issue
for anything you believe is exploitable.

The report stays private between you and the maintainer while it is
investigated and fixed, and the advisory is published once a release with
the fix is out. Say in the report if you would like to be credited.

## Supported versions

Only the latest release is supported: fixes ship in the next release, and
the container image's `latest` tag and the release archives follow it.

## What is in scope

HeapLeach downloads whatever untrusted pages and hosts serve it, so the
boundaries that matter most are:

- **Writing outside the download directory** — a remote name, folder or
  redirect that escapes it, overwrites an existing file or follows a link
  out of it.
- **Running code** — anything that reaches the helper script, yt-dlp or
  ffmpeg with arguments or files an attacker chose.
- **TLS** — a connection accepted with a certificate that does not lead to a
  trusted root or does not match the host, including through the
  certificate-chain completion described in the code (`aia.go`).
- **The API from a browser** — a page in another origin able to drive the
  API (cross-origin protection), or the UI rendering remote text as markup.
- **Resource exhaustion** that one malicious page or host can cause.

The web UI and API deliberately have no login: HeapLeach is a single-user
service, and anyone who can reach its port can use it. Running it on an
untrusted network without an authenticating proxy in front is a deployment
choice rather than a vulnerability — see
[Network access and trust](README.md#network-access-and-trust) for how to
run it safely.
