# Architecture

[Documentation](README.md) · [Project home](../README.md)

Two diagrams, for the two questions worth answering: what happens to a link,
and what is allowed to depend on what.

### What happens to a link

The middle of this is the part worth knowing. An extractor downloads nothing —
it returns one `File` per download, and the **shape of that `File` is what
decides everything downstream**. A plain `URL` reaches the segmented engine. A
`Resolve` closure instead of a URL means the host signs its links and they
expire, so one is minted per attempt rather than while the item sat in a queue.
`Segments` means the media arrives as an ordered list of parts with no length
to range over. `External` means reaching it needs more than HTTP. `Cipher`
means the bytes arrive encrypted and are decrypted on the way in.

Adding a host is choosing among those shapes; nothing below the extractor has
to be told which host it is talking to.

```mermaid
%%{init: {"flowchart": {"wrappingWidth": 420}}}%%
flowchart TD
    paste(["a link — pasted into the UI, or given on the command line"])

    subgraph resolve["extractor — what is behind it"]
        claim{"an extractor recognises the page?"}
        host["that extractor"]
        direct["Direct — the catch-all, after sniffing for a player, a manifest or a directory index"]
        res["Result: one File per download<br>URL · Resolve · Segments · External · Cipher"]
        claim -- yes --> host --> res
        claim -- no --> direct --> res
    end

    queue[("the queue — an item starts once a worker is free, the queue is unpaused, and the host's own pace allows")]

    subgraph move["download — how the bytes arrive"]
        pick{"what does the File carry?"}
        multi["segmented — the remaining span bisected, one connection per range"]
        seq["sequential — a single stream"]
        playlist["playlist — parts fetched several at a time, appended in order"]
        ytdlp["yt-dlp, through the helper script"]
        bucket["one shared token bucket — the rate ceiling, and where a pause parks"]
        part[("a .part file keyed on the job's own URL, with a sidecar recording each range · ciphertext decrypted on the way in")]
        pick -- "URL, length known,<br>server honours ranges" --> multi --> bucket
        pick -- "URL, no length" --> seq --> bucket
        pick -- Segments --> playlist --> bucket
        pick -- External --> ytdlp
        bucket --> part
    end

    fin["renamed into place · any .ts rewrapped to .mp4"]

    paste --> claim
    res --> queue --> pick
    part --> fin
    ytdlp --> fin
```

Two details in there are load-bearing rather than incidental. The `.part` file
is named from a hash of **the job's own source URL** rather than from anything
per-run or per-item, which is what lets an interrupted transfer be recognised
by the next run instead of restarting from zero — a signed media link would
change every time and resume nothing. And the token bucket is genuinely one
object shared by every connection, which is why pausing costs nothing and why
the code that opens extra connections has to ask whether the ceiling, rather
than the host, is what is holding a transfer back.

### What may depend on what

Arrows are imports — the layering rather than every edge, since a package may
reach anything below it. What matters is that nothing points back up. `webui`
sits outside the stack altogether: it carries the compiled frontend and no
logic, so only the entry point touches it.

```mermaid
flowchart TD
    main["cmd/heapleach<br>flags, signals, and which of the two modes to run"]
    main --> server
    main --> cli
    main --> webui

    server["server<br>JSON API · SSE stream · serves the embedded UI"]
    cli["cli<br>the animated terminal display"]
    webui["webui<br>the compiled frontend (go:embed)"]

    server --> download
    cli --> download
    download["download<br>worker pool · resumable transfers · progress"]
    download --> extractor
    download --> proxy
    proxy["proxy<br>egress leases · throughput scoring · Bolt inventory"]
    proxy --> httpx
    extractor["extractor<br>one file per host, and a fallback that sniffs"]
    extractor --> httpx
    httpx["httpx<br>browser-shaped client · redirects · retry and backoff"]
    httpx --> base

    download --> tools
    extractor --> tools
    tools["tools<br>locates yt-dlp, ffmpeg and deno"]
    base["config · util<br>every tunable, and the dependency-free helpers"]
```

Both halves of the program drive the same `download.Manager`: the server
subscribes to it and streams whole state snapshots to the browser, and the
headless run polls it on a ticker of its own. Nothing about a transfer knows
which of the two is watching.
