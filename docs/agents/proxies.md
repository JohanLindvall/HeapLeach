# Proxy routes

Some services count free downloads per address, so the downloader spreads
their files over many addresses through a pool of proxy routes.
`internal/proxy` adapts `github.com/JohanLindvall/proxytron/lease` to this
program; routed transfers live in `internal/download` (`proxy.go`,
`multiroute.go`, `settings.go`, `challenge.go`). How the HTTP client reports
proxy failures is in [http.md](http.md); Keep2Share's own flow is in
[hosts/keep2share.md](hosts/keep2share.md).

## The pool

The persistent egress pool is enabled by default, backed by bbolt, with
`-proxies=false` or `HEAPLEACH_PROXIES=0` to disable it. The library owns
discovery, scoring, leases, exit probing and claims, and their defaults.
Application scope policy, the health epoch, the cached probe-outage policy and
HTTP identity stay here.

The pool's scopes are `config.ProxyGroups`: `keep2share`, `fileboom`,
`emload`, `mega` and `cloudflare` (WAF recovery). An extractor marks a
per-address service with `Pace.PerRoute` and names it in `Pace.Group`.

The inventory lives beside the queue file wherever that is put
(`defaultProxyDB`), and in the default state directory when no queue is kept,
so a container that mounts only its state directory keeps its routes across
restarts. Bolt stores the inventory and each health outcome immediately, while
active leases stay in memory. Progress samples never write Bolt from
`sampleLocked`; a background task flushes changed rows. The manager owns the
pool, opens it lazily when proxies are enabled, and closes it after the
workers finish.

Explicit routes never fall back to direct and never disable destination
certificate verification. An HTTPS proxy's outer TLS connection accepts
self-signed certificates, and `DialTLSContext` handles that hop alone; TLS
inside CONNECT keeps the original certificate verification, tested with both
transport modes.

## Leasing a route

The dispatcher leases a route before resolving and holds it through the
transfer: one file per address per service, across aliases and proxy
protocols. The request context pins the CAPTCHA, ticket redemption, redirects
and bytes to the route's one isolated client (`httpx.WithRoute`). Changing
routes invalidates an IP-bound ticket and cached target; a retry on the same
healthy route may reuse them. Cooldowns belong to the service and the route,
never to the service's whole queue.

The score estimates useful bytes per second for the remaining file size,
including reliability and observed setup delay. Unmeasured endpoints share one
exploration candidate, drawn at random, first an address and then one of its
ports, so discovery neither walks adjacent addresses in URL order nor spends
itself on one gateway's thousands of ports. Selection and reservation share
the pool lock, including for these unrated routes. Unmeasured routes never
preempt working transfers.

### Routes never tried

**A route never tried for a service answers for itself alone**
(`entry.untested`): what the other ports on its address are doing says nothing
about it. Feeds list gateways of thousands of ports on one address, and those
ports leave through public addresses of their own, rarely the gateway's. Held
to its address, nearly every untested route would sit "busy" or "cooling"
because one sibling port was leased or had met a host's limit. So an untested
route is eligible unless it is dead, leased itself or cooling itself, and its
own lease still counts against its address, so the address's tried ports stay
busy. Once tried, a route shares its address's leases and cooldowns like any
other. Most gateway ports turn out dead, so they are tried and set aside: a
dead proxy costs a file no retry, and an extra stream no backoff
(`routeBroken`).

`Lease.Cooldown` writes the host's timer on its own route alone, and tried
ports pick it up where routes are chosen (`addressCooling`). Writing it on
every port would be a synced bbolt transaction per port under the pool's lock
— thousands for one wait on a gateway, with the dispatcher waiting on that
lock while holding the manager's.

### Exit addresses

**No two downloads share an exit address** (`proxytron/lease/exit.go`). The
address a host counts free downloads by is the one a route's requests leave
from, and that need not be the one the proxy listens on: the live ports of one
gateway can leave by many different exits, and one exit can sit behind several
gateways. So no rule about listening addresses can keep two downloads off one
exit. Before a routed attempt's first request to the host, `claimExit`
delegates to `proxy.ExitProber`: it asks through the lease's existing client,
bounded by `ProxyExitProbeTimeout`, and calls `Lease.ClaimExit` to claim the
answer. There is one lease per exit per service, as for an address. A route
whose exit is taken, or cooling for the service, is handed back at no cost to
the file (`errExitInUse`), and selection passes over a route whose known exit
is held or cooling (`exitFree`). The host's timers and refusals met through a
route cool its exit, so every route known to leave by it waits too. Exits are
kept on the routes (`Exit`, `ExitAt`, trusted for `ProxyExitTTL`), and exit
cooldowns are seeded from them on load.

The echo is also how a route shows it works, before a CAPTCHA is spent on it.
One that cannot reach the echo service fails as it would have failed the
host's first request. One that answers each echo service in turn
(`ProxyExitProbeURL`, then `ProxyExitProbeFallbackURL`) with something other
than an address, a page or a refusal of its own, is not carrying requests as a
proxy should (`proxy.ErrExitProbeFailed`): it is shelved, and the file goes on
at no cost — unless the echo services are down over the normal connection too
(`echoReachable`, asked at most once a `ProxyEchoRecheck`). Then the exit
stays unknown and the download goes ahead, since their outage must not shelve
every route. The echo services' answers are not scored as the host's
(`proxy.ExitProber`): only the route's failure to carry the request counts
against it there, or one rate-limited answer would put the route on the
refusal backoff. The shared prober keeps the lease's client and browser
identity, and `proxytron.Detector` parses bounded plain-text or JSON IP
answers for both routed and normal checks. Cancellation stops both checks
without caching an outage or penalizing the route. Extra streams check and
claim their routes the same way. `Config.ProxyExitProbes` carries the URLs and
is empty in a `Config` built by hand, so tests opt in.

### One browser per route

**Each route of a per-route service is one browser** (`clientAdapter.agent`,
`httpx.WithRouteAgent`). It draws a User-Agent from
`config.BrowserUserAgents`, over thirty real desktop Chrome, Edge, Firefox and
Safari strings, when its client is made, and every request it carries sends
that one, whichever client built the request: the CAPTCHA, the ticket, the
echo and the bytes. Hundreds of addresses all naming one browser to a host
counting free downloads per address is a signature of its own.

## When a routed attempt fails

A routed attempt that fails returns the item to the queue to try another
route, with a retry budget of its own (`ProxyRetries`). A failure that the
next attempt may well not meet is retried on the best route free rather than
failing the file. `transientRouteFailure` covers an expired deadline, a
timeout, a connection refused, reset or cut short, and
`extractor.TransientError`. Nothing is charged to the route for these, since
the route layer has already scored what the network did, and these attempts
spend `ProxyRetries` like any route failure.

A failure that is the route's, not the file's, costs the file nothing
(`routeAtFault`):

- a proxy that could not be reached at all (`deadProxy`: a `proxyconnect` or
  `socks connect` around a failed dial);
- a connection it dropped (a bare or unexpected EOF, a reset);
- an address the host refuses (`RefusedError`);
- a proxy that answered and showed it cannot carry the request at all
  (`proxyUnusable`): a SOCKS proxy wanting credentials, a tunnel refused below
  500, plain HTTP where TLS was expected (an `https://` proxy that is not one,
  or HTTP inside the tunnel), or a destination certificate that does not
  verify. The proxy's own TLS hop accepts any certificate, so a destination
  certificate that does not verify came through the tunnel: an expired one
  presented for a host whose own is current is the proxy's doing;
- a tunnel the proxy refuses at 502 or 504 (it cannot reach the host from
  where it is), a TLS handshake that never completes through the tunnel, or a
  tunnel answered in another protocol (a SOCKS proxy listed as an HTTP one,
  which net/http calls a malformed response);
- a challenge from the host.

Public lists are mostly such proxies, so charging them to the file would fail
it on dead routes before it met one that worked. The pool shelves each such
route. The cost, accepted on purpose, is that a host refusing every proxy has
files trying routes for as long as the pool offers any, then waiting in the
queue, rather than failing, and each refusal also costs a CAPTCHA. A host down
for every route likewise leaves its files waiting for one. Other timeouts
after proxy setup, HTTP refusals, stalls and a SOCKS "host unreachable" still
count against the file.

**An address refused outright waits an hour** (`ProxyAddressRefusedWait`), its
exit with it. A host can refuse nearly every address at once, the direct
connection's included; on a short backoff each address would ask again within
minutes, after another CAPTCHA each time.

A file whose part file already holds bytes goes back to the front of the queue
after a route failure, not the back ([download.md](download.md)).

## Starting on the normal connection

A per-address service whose quota the normal connection usually has room for
marks its pace `DirectFirst`; mega does
([hosts/file-hosts.md](hosts/file-hosts.md)). Its files start on the normal
connection with no pace at all: `newItem` keeps the pace aside in `routePace`,
so they take the queue's files at once and streams like any host's, and no
route is leased for them. When the host makes that connection wait, a
`WaitError` from `Resolve` or from `File.Refused`, `moveOntoRoutes` gives the
file its pace and sends it straight back to the queue, to be leased a route of
the group like any per-address file. Without proxies the file stays on the
normal connection and waits for the time named; switching proxies on moves a
file waiting that way at once. Until it moves, a file is no demand on the pool
and is charged to no group, so it holds back none of the group's routed files
(`hostFullLocked`).

Nothing is written to the pool's direct route when a file moves. mega's
resolver keeps the wait it was told by route, the normal connection's under
`httpx.DirectRoute`, and answers a lease of the direct route with it, which
cools that route for the group in the ordinary way without asking the host
again.

## Route health epochs

**Route health can be forgotten wholesale** (`config.ProxyHealthEpoch`). The
database records the epoch its verdicts were reached under, and a database
opened under another has every route's health dropped while the inventory is
kept: shelving, cooldowns, answers, speeds, exits and the priors
(`forgetHealth`, in proxytron). Raise the epoch when a change means the
recorded verdicts were reached wrongly. It has a cost: the dead routes are
forgotten with the rest and are found again by files, five seconds a route, so
for hours afterwards most attempts meet dead proxies and files read "Checking
the download route — connection attempt 55". Raise it only when the verdicts
of a whole kind are wrong. The comment on the constant records what each epoch
forgot.

## Inside the pool

These behaviours are proxytron's (`lease/`), recorded because the queue leans
on them; change them there.

- **Selection never walks the inventory.** A feed holds tens of thousands of
  routes, so ranking runs off an index (`lease/index.go`) of the routes by
  address, by ID and by measured speed, rebuilt when routes are added or
  removed. A route's first speed enters the index where speeds are written
  (`observe`, `Progress`); a speed set anywhere else, as tests do, is missed
  unless it is set before the index is built.
- An address's leases are a count in the index. An address found cooling is
  remembered until the cooldown it found (`addressCooling`); cooldowns are
  only ever lengthened, so that memory can err only on the safe side. An
  address found free is never remembered, since a cooldown can start at any
  moment.
- Exploration draws an address and then one of its ports, so each address is
  drawn in proportion to the share of its ports that could take the transfer.
  After repeated misses it draws by route, and after more it lists the
  qualifying routes and draws among them, for when what qualifies is a sliver
  of a few gateways' ports. Upgrades rate measured routes only.
- A route shelved as dead or broken gives up its HTTP client when its last
  lease ends, since route hunting tries thousands like it an hour and their
  clients would otherwise grow the heap without end. A lease keeps the client
  it was taken with, so this never pulls one from a request in flight.

## Throughput and upgrades

Five-second progress windows train throughput before a long file finishes,
with the resumed prefix, pauses and speed caps excluded. A slow, range-capable
transfer can move to a proven faster route after sustained evidence that the
remaining time saved covers setup costs. `Lease.Upgrade` reserves the
replacement atomically. Only the attempt's child context is cancelled:
`transferRouted` waits for the old response and part file to close, then
changes leases and resumes. A user cancellation wins, and a settings change
that invalidates the reservation resumes on the old lease.

Feed attempts are 24 hours apart unless a service's runnable demand exceeds
the distinct addresses with sufficient scores, and even early top-ups wait
five minutes. Discovery demand shares the global worker budget across the
services; `SetDemand` publishes their allocations atomically so an obsolete
demand cannot trigger an early feed reload.

## Several routes for one file

**A file can read through several routes at once** (`multiroute.go`). A free
link allows one connection, from the address that obtained it, and carries its
own speed cap, so each extra stream leases a route of its own and runs a free
download of its own there (the resolver keeps a ticket and link per route; see
[hosts/keep2share.md](hosts/keep2share.md)).

Extra streams take idle routes only:

- None starts while a file of the same service is due to start and not running
  (`routeWanted`, a queue scan kept for `RouteWantedRecheck`), and one still
  being obtained is given up. Whether the dispatcher found a route says
  nothing here: untried routes count as free, so it nearly always finds one,
  mostly dead, and the files cycling through them would race extra streams for
  the few that work.
- A file its job holds back is not due, for this or for the pool's demand:
  otherwise one paused job would keep every other file of the service from
  extra streams, and ask the feeds for routes.
- Nor is any file due while the queue can start none, being at its limit on
  files at once, paused or short of room. Such a file waits for a slot, not a
  route, and the transfer whose end frees the slot gives its route back with
  it.
- A running extra keeps its route: its address has spent its free download,
  and a waiting file given it would only be told to come back in an hour. So
  extra streams engage as a queue drains, for its last files.
- The queue sets up only `RouteExtraAcquisitions` (64) extra streams at a time
  (`extraAcquiring`). That is no bound on how many run, which is files at once
  times streams per file. Each set-up is a hunt and a CAPTCHA, and the files'
  own routes need the reader first.

Two rules keep the streams honest:

- **One connection per route.** Opening the next range on a route while the
  host may still count the last one is a second connection, so every request
  asks for at most `RouteRangeSize` and is read to its end, and the next goes
  out on the same connection. The first request in `transferOnce` is bounded
  the same way, and `splitUnrequested` never cuts into a range already asked
  for: a split that did would make the connection abandon its response and
  open another, which the test rig counts.
- **Each route's speed is its own.** The main route's speed estimate is fed
  only its own bytes: `sampleLocked` subtracts `extraBytes`, each extra trains
  its own lease through `Lease.Progress`, and no upgrade runs while extras do,
  since aborting the attempt would end them too.

An extra that gets nowhere has cost a CAPTCHA, so attempts back off towards
`RouteExtraBackoffMax`. An extra refused for its route's used-up quota
(`File.Refused`) holds that route back from the service until the quota is
back (`pumpSource`). One whose route goes silent for the stall timeout is
dropped alone, because the transfer's own watchdog waits for every connection
to go quiet. A range left unread, by a stopped extra or a restored sidecar, is
claimed by the next connection to finish its own; the main route reads one at
a time.

## Changing settings while running

Proxy settings can change while workers run. `settingsMu` serializes
preparation with shutdown, and a failing database open leaves the whole update
unapplied. Disabling keeps the pool and its active leases alive; normal
admission for each service waits for that service's transfers to drain.
Enabling waits for its unleased direct transfer before the pool may lease that
address. Both modes use `httpx.DirectRoute` for ticket and cooldown identity.
Queued direct cooldowns are cleared on enable so new routes can run, while the
resolver keeps the address's actual timer. Source edits preserve active
leases, and late results from removed feeds are discarded.

**Each proxy group can be held to one protocol** (`config.ProxyType`,
`ProxyTypes`, `proxy.SetTypes`): any, SOCKS (socks4 to socks5h) or HTTP (http
and https), for every group or for one alone. The library's own
`lease.WithProxyType` is fixed when the pool opens and covers every scope, so
this is `Pool.SetScopeProxyType`, a restriction per scope on top of it that
can change while the pool runs. A specific protocol leaves the direct
connection out, and a group with no route of its protocol gets no lease rather
than another protocol's: its files wait in the queue, as for any group without
a free route. A change holds from the next lease; one already granted is left
to finish, and an upgrade reserved before it is checked again
(`Lease.Configured`). The pool's `Page` shows each scope only what it may use,
so the list is the answer to what a group can draw on. `applyProxyTypesLocked`
is called wherever the answer can change: when the pool opens, when the
settings change, and when `Restore` brings back saved ones, since `main` opens
the pool before reading the queue. A change to `proxyTypes` names only the
groups it changes ("" follows the all-groups protocol again), so two quick
changes to two groups from the browser cannot undo each other. Both the
all-groups protocol and the groups' own are kept with the queue
([download.md](download.md)).

## Logging

Once a minute a goroutine of its own (`summarizeRoutes`) logs `route attempts`
per service (`routeSummaryLocked`): how routed attempts ended (dead, shared,
refused, waited, transient, stalled, failed, finished), and how many files are
moving bytes or still trying. It runs apart from the dispatcher, whose tick is
quietest exactly when files are failing routes as fast as they start. A queue
that is all dead routes or all refusals says so in one line.

Each attempt also says how it ended (`logRouteAttempt`,
`route attempt ended`): the route's address without credentials, its exit, the
outcome, the cause, and what the file does next (another route, a wait, done
or fail). Dead routes and taken exits are logged at debug level, since they
can come twenty a second and say nothing about the host; everything the host
answered is logged at info. `deferWaitLocked` logs a wait only for real waits,
with their reason, so a file sent on to another route is not reported as the
host asking it to wait.

## Recovery for other hosts

### WAF challenges

A Cloudflare challenge ([http.md](http.md)) starts recovery. The manager
installs `extractor.WithChallengeRecovery` for source extraction, and the
registry reaches nested sources with the same hook. A recovery reruns the
whole extraction on one lease. `File.Refresh` re-reads the smallest known
source on the worker's route, matching a file by name and directory rather
than by its position in a listing, before minting fresh media URLs. External
downloaders own their network requests and cannot use this recovery.

Any native host can enter the `cloudflare` proxy group after a challenge.
Per-route services keep their group and address limits, adding `Pace.WAF` to
route selection. `entry.WAF` stores challenge outcomes separately: a WAF
failure changes no ordinary service counters, speed, transport health or
cooldown. Recovery combines that penalty with ordinary reliability and
unthrottled speed measurements; known free-host caps are not shared as an
estimate of unrestricted capacity. A challenge through a route costs the file
or source nothing (`routeAtFault`): finding an address the host does not
challenge is what recovery is for, and each challenged route is held back by
its own WAF record. Other failures use the ordinary proxy budget; source lease
acquisition is cancelable and bounded by the request timeout. WAF recovery
keeps the client's User-Agent, since some hosts sign requests with it (gofile
does).

### Temporary failures

A source or file that opts into `ProxyFallback` (doodstream does) starts on
the normal connection and, after a temporary network or HTTP failure there, or
a persistent `RELOAD`, tries the shared recovery pool. The manager classifies
errors and owns reservations, penalties and random unmeasured draws; a missing
video or a parser error cannot start a proxy search. Source recovery exempts
unreachable, dropped and refused proxies from the retry budget as file workers
do, while other temporary failures stay bounded. A challenge still records its
separate WAF penalty. During source discovery the connection attempt is added
to the source's note.
