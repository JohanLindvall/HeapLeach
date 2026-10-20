# Getting help

Start with the [quick start](README.md#quick-start), the
[documentation index](docs/README.md) or the common fixes below.

| You need to… | Go here |
| --- | --- |
| Report a bug | [Bug report](https://github.com/JohanLindvall/HeapLeach/issues/new?template=bug_report.yml) |
| Suggest an improvement | [Feature request](https://github.com/JohanLindvall/HeapLeach/issues/new?template=feature_request.yml) |
| Request support for a host or platform | [Site request](https://github.com/JohanLindvall/HeapLeach/issues/new?template=site_request.yml) |
| Ask a usage question | [Search existing issues](https://github.com/JohanLindvall/HeapLeach/issues), then [open a question](https://github.com/JohanLindvall/HeapLeach/issues/new?template=question.yml) |
| Report a security vulnerability | Follow the [security policy](SECURITY.md) and report privately |

## Downloads are held or the queue is not moving

After a restart, unfinished jobs are held until you click **Resume** or retry
a job. This prevents a restart from unexpectedly using your connection.
For an unattended service, use `-resume` or `HEAPLEACH_RESUME=1`.

The default free-space floor is **10 GiB**. Below it, new transfers wait.
Free space at the destination or choose an appropriate `-min-free` value.
Also check whether the queue is paused or a job's file limit is zero.
Host cooldowns and proxy waits appear beside the affected file.

## The browser did not open, or port 8080 is busy

A launch with no arguments opens a free local port. If the browser does not
open, use the address printed in the terminal. `HEAPLEACH_OPEN=0` deliberately
suppresses the browser.

An explicit directory or option switches to the normal service defaults.
To choose a free port and open the browser explicitly:

```bash
./heapleach -addr 127.0.0.1:0 -open ~/Downloads
```

For Docker, change the host side of the mapping to
`-p 127.0.0.1:9000:8080`, then open **http://localhost:9000**.

## A media helper or CAPTCHA model is missing

Keep the release's whole `ocr/` directory beside the executable. Source
builds can install it with `make ocr-assets`.

Some sites need yt-dlp, ffmpeg or deno. The Docker image includes them;
release archives bundle OCR but do not bundle all media helpers.
See [helper installation and platform limits](docs/helpers.md).
The external download script requires a POSIX shell, so it is not a native
Windows helper launcher.

## Docker cannot write downloads, or the queue disappears

Create the host download directory before starting the container and use
the matching user ID on Linux. The [Docker example](README.md#docker)
sets both the download mount and a writable state location.

Queue history and proxy measurements need persistent storage as well as the
downloaded files. Follow [Docker persistence](docs/installation.md#docker-and-persistent-state).
Use a separate proxy database for each running instance.

## A supported site stopped working

Try the [latest release](https://github.com/JohanLindvall/HeapLeach/releases/latest).
Check the job's error and the **Logs** panel. Deleted files, login
requirements, regional restrictions, quotas and premium-only content can
prevent a download even when the site's extractor is supported.

If the page is publicly available and the problem persists, submit a
[bug report](https://github.com/JohanLindvall/HeapLeach/issues/new?template=bug_report.yml).
Give the host name and URL shape with placeholders, such as
`https://example.test/file/<id>`. Do not post a real content link.

## Collect a useful diagnostic report

Include the version from the UI or `heapleach -version`, your OS and CPU
architecture, how you installed it, and steps that reproduce the problem.
Describe what you expected and what happened.

Open **Logs** in the UI to filter and export recent messages. Debug logging
is enabled by default; if you disabled it, use `-debug` for a reproduction.
The in-app log history covers only the current run and has a size limit.

Before posting, replace content URLs, hosted filenames, local usernames,
passwords, tokens, cookies and proxy credentials with placeholders.
Do not upload the queue state file: it can contain source passwords.
Keep the relevant error messages and status codes.

## The UI shows a placeholder after a source build

The Go binary embeds the frontend at build time. Run `make build` or
`make native` to compile both halves together, then start the new executable.
See the [development setup](CONTRIBUTING.md#development-setup).
