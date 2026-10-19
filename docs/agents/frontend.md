# Frontend

`frontend/`: React 19 + Vite, TypeScript in `strict` mode with
`noUnusedLocals` and `verbatimModuleSyntax`. `tsc -b` runs as part of
`npm run build`, so a type error fails `make build`. Node is not installed on
this machine; `make frontend` and `make test-frontend` run in Docker. The
build lands in the Go embed directory (see the single-binary coupling in
[AGENTS.md](../../AGENTS.md)).

## State

State comes entirely from the SSE snapshot via `useLiveState`, which falls
back to polling `/api/state` when the stream is unavailable, and merges the
stream's row patches into what it holds (`snapshotMerge.ts`; the protocol is
in [server.md](server.md)). Components render straight from the payload — the
server precomputes job aggregates so the UI does no derivation. Everything the
snapshot carries is rendered somewhere: `note` explains a deliberate wait, and
`segmentsDone`/`segmentsTotal` are the only progress a playlist has, since it
has no byte total until its last part lands. A field the UI stops reading
should come out of the payload rather than being left to accumulate.

The one vocabulary the UI does own is in `status.ts`: which statuses count as
active, terminal or retryable, and the sidebar's filters and the search built
on them. The sidebar, the cards and the "Clear finished" count all ask it
rather than keeping lists of literals of their own, and it is where the
pure-logic tests for that live.

The sidebar's second section, **Files**, is built on the same module:
`phaseOf` sorts every file across all jobs into Downloading, Queued, Failed
(cancelled included — both are what a retry is for) or Done, and choosing one
replaces the job cards with `FileList`, a windowed list of the job cards' own
rows, each naming its job. Its per-row actions carry only the item id, looked
up to a job there, so one stable callback serves every row.

`gamification.ts`/`useProgress.ts` add a progress layer persisted in
`localStorage`; it is derived from bytes actually written.

## The version badge

The header's version badge (`VersionBadge`, `version.ts`) shows the server's
build from the snapshot (`Snapshot.Version`) rather than asking once: contdep
redeploys under open tabs, the stream reconnects to the new server, and the
page goes on running the interface it was loaded with. The first version a
snapshot reports is taken as the page's own, and a later, different one turns
the badge into a reload button.

## Settings and limits

The header's Settings button opens live concurrency, streams, speed and
download proxy controls. Proxy settings last for the session; parallel
downloads, streams and proxy protocols are kept with the queue
([download.md](download.md)). The protocol choices (`ProtocolSelect`) show a
choice as made until a snapshot agrees, or for a few seconds if none does, as
the proxy switch does, and each sends only its own group.

Each job card's gauge button opens that job's own limits (`JobLimits`): number
fields that commit on Enter or blur and are held to the queue's values, a
speed menu offering only speeds beneath the queue's, and the server's value
shown otherwise. Tags on the card say which limits hold while it is closed.

`ProxyList` selects Keep2Share, FileBoom, EMLOAD, MEGA or WAF recovery with
the `site` query and polls `/api/proxies` only while mounted and the tab is
visible. The inventory can contain tens of thousands of routes and must not be
added to SSE snapshots. Filtering, sorting and pagination happen server-side,
in `proxytron/lease`'s `Pool.Page`, which builds rows only up to the end of
the requested page and works out each route's ID and shown address once;
building every route's row for each poll would cost hundreds of milliseconds
and tens of megabytes a request on a large pool. Scores use the selector's
deterministic mean estimate, not a fresh random draw when viewed. Source
drafts fetch `/api/settings` separately and survive failed saves; source
credentials never belong in the list or queue snapshots.

## The Logs panel

The Logs panel polls `/api/logs` deltas only while it is open, visible and
unpaused, aborts pending reads on close or pause, and discards expired rows
even when the byte limit shortens the window. Export uses the current filters.
Messages render as text, including markup in remote error messages.

## Styles

`styles.css` is a single design system: one accent gradient, one surface ramp,
one shadow scale, dark and light via `prefers-color-scheme`. Prefer composing
from the tokens at the top over adding new colours.

The page scrolls on the root element and `.shell` is centred inside a 1440px
cap, so the scrollbar's width decides where everything sits. `html` therefore
carries `scrollbar-gutter: stable`: without it, selecting a filter that
resolves to nothing shortens the page below one viewport, the scrollbar
disappears and the entire layout slides sideways.
