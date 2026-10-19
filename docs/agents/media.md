# Playlists, tracks and external downloads

Files that are not one rangeable resource: HLS playlists (`hls.go` in
`internal/extractor`; `playlist.go`, `hlscrypt.go`, `mux.go`,
`packedaudio.go`, `subtitles.go`, `languages.go` in `internal/download`), the
remux of finished transport streams (`remux.go`, `mediaprobe.go`), and pages
that need yt-dlp (`external.go`, `internal/tools`).

## Reading a playlist

Every playlist goes through `resolveMediaPlaylist` (`hls.go`), and every
caller carries the key, the separate audio and the subtitles it returns onto
its `File`. So no path joins an encrypted or demuxed playlist as it stands:
either would make a file that looks finished and is ciphertext or silent.

- **Which variant.** A master playlist's `CODECS` describes the whole
  presentation, not the variant's own segments, so a variant that names a
  separate `AUDIO` group advertises audio it does not carry.
  `hlsVariant.muxed` therefore checks the group's renditions for a `URI` of
  their own rather than trusting `CODECS`; without that a demuxed host would
  yield a silent file that looks like a finished download. Such a variant
  comes with its audio renditions instead, which `mux.go` joins to it. At the
  same height `bestVariant` takes H.264 over HEVC and AV1, whatever their
  bitrates (an HEVC rendition can outbid H.264 at the same height): every
  player decodes H.264, and HEVC in an MP4 tagged `hev1`, as packagers mostly
  tag it, plays in nothing of Apple's.
- **Keys.** `mediaPlaylistKey` accepts one key over every segment — the same
  key line repeated before each segment counts as one — and refuses the rest
  before a byte is fetched: SAMPLE-AES, a key that changes or starts partway,
  and a non-`identity` KEYFORMAT (DRM). Over fragmented MP4 the initialisation
  segment is first in the list. Named before the key, it is in the clear
  (`SegmentKey.Clear`): not decrypted, and not counted in the sequence an
  absent IV is numbered from. Named after it, it is encrypted too, which needs
  the key's own IV, since it has no sequence number; without one it is
  refused. The key reaches the transfer as `File.SegmentKey`, and a resolver
  can replace it with the segments (`Target.Segments`, `Target.SegmentKey`)
  for a playlist signed for less than the queue may wait.
- **Separate audio.** A CMAF master, and anything carrying an audio
  description, names its audio as `EXT-X-MEDIA` renditions with playlists of
  their own, and its variants' segments carry video alone.
  `resolveMediaPlaylist` follows the chosen variant's audio group to every
  rendition's playlist (`hlsMedia.Audio`, then `File.Audio` and
  `Target.Audio`), in the group's order with its default first. A rendition
  named without a URI is carried in the video's own segments and is listed
  with no segments, which is how a main language muxed into the variant sits
  beside an alternate kept apart.
- **Subtitles.** A variant naming a `SUBTITLES` group comes with its WebVTT
  renditions (`resolveSubtitles`, `File.Subtitles`), keeping the playlist's
  default and forced flags. A rendition whose playlist cannot be read, or is
  WebVTT wrapped in fragmented MP4 or encrypted, is not listed.
- **Byte ranges of one file are parts like any other.** A playlist that slices
  one file per rendition (`EXT-X-BYTERANGE`, and a map's `BYTERANGE`), as
  ARTE's do, has each part carried as the file's URL with its range in the
  fragment (`#range=FIRST-LAST`, `extractor.SegmentRange`), which no request
  ever sends. `File.Segments` stays a list of strings for the many extractors
  that never meet a range. A range that states no offset starts where the
  previous part of the same file ended, and a map's at the start.
- **Audio descriptions** are named so (" (audio description)") only where the
  name would otherwise read like another rendition's: SRF names its
  description as it names the main track, but for a trailing space, while
  ARTE's name ("Deutsch (Audiodeskription)") already says it.

## Fetching a playlist

**Playlist transfers** (`playlist.go`) handle files that arrive as an ordered
list of parts rather than one rangeable resource. They bypass the segmented
engine entirely: there is no length to divide and no offsets to seek to, so
parts are fetched several at a time and appended strictly in order, with an
out-of-order buffer bounded by releasing a worker slot only once a part has
been *written*.

Two things here are easy to get wrong:

- A part is read through `m.throttled` straight into a buffer, **not** through
  `httpx.Client.Bytes`. That helper caps what it reads at
  `config.MaxResponseBytes`, which is sized for pages: a media part larger
  than it would be silently truncated and joined anyway. Reading the body
  directly also means a pause parks mid-part and the rate cap holds *within* a
  part rather than only between parts.
- The stall watchdog samples raw fetched bytes, not `Item.downloaded`. The
  item counter only moves when a whole part lands *in order*, so a large part
  arriving slowly looks identical to a dead connection to anything watching
  it. `watchForStall` therefore takes a progress function rather than an item.
  It starts before fetching an encryption key and counts those bytes too: the
  key uses the streaming client, so a silent body otherwise waits forever.
  Through a proxy, that stall follows the same failure penalty, cooldown and
  route retry as a stalled media body.

`fetchSegmentOnce` asks a byte-range part for exactly its bytes, checks
`Content-Range` and the length, and refuses a host that answers with the whole
file (`errRangeIgnored`, not retried): read whole, a playlist of a thousand
parts would fetch the file a thousand times.

**AES-128 playlists are decrypted natively** (`hlscrypt.go`). HLS's
`METHOD=AES-128` encrypts each segment on its own (AES-CBC, PKCS#7), so each
segment — already fetched whole into a buffer — is decrypted in place before
it joins the file, and the part file and its checkpoint hold plain bytes,
which keeps resume honest. The key is fetched once per attempt; the IV is the
playlist's own or the segment's media sequence number. A wrong key shows only
in the padding, so a padding error fails the attempt rather than writing
noise.

**Separate tracks are fetched together**: segment *i* of every track before
*i+1* of any, from one pool of connections, each into a part file of its own
beside the video's (`audioPart`) with a checkpoint of its own. Their links
were signed together, so none expires while another downloads ahead of it, and
the item's segment count moves with the time the transfer takes. A checkpoint
is held to the track that wrote it (`playlistState.Track`): a playlist's
tracks commonly have the same number of segments, and a part resumed from
another language's checkpoint would carry both. `onDisk` sums every track's
checkpoint for the retry budget.

## Joining the tracks

The join is ffmpeg, by stream copy into an MP4 (`joinArgs` in `mux.go`), and
three things in it are easy to get wrong:

- ffmpeg starts every input at zero on its own unless told otherwise, which
  drops the offset between renditions: half a second in the test fixture, lip
  sync in a real file. `-copyts` keeps their timestamps, which share the
  playlist's one timeline, and `-avoid_negative_ts make_zero` moves the output
  as a whole to start at zero. `TestJoiningKeepsTheOffsetBetweenTracks` holds
  it.
- An MP4 keeps a track's language only as three letters, and ffmpeg drops a
  two-letter `sv` without a word: `mp4Language` maps ISO 639-1 to 639-2/T,
  taking the primary subtag of a longer tag (`sv-x-tydligaretal` is `swe`).
  The label goes in `handler_name`, which is what an MP4 keeps and VLC and mpv
  show; `title` is dropped. And an MP4 enables every audio track flagged
  default, each in an alternate group of its own, for a player to take as
  tracks to play together. So exactly one is: the playlist's default.
- The audio is exactly the renditions the playlist names, each from where it
  says. Audio in the video's segments that the playlist does not name is left
  out: Akamai (SRF, ZDF) mixes the default track into the video *and*
  publishes it apart, and keeping both lists it twice. A rendition named as
  carried in the video is mapped from it once ffprobe has counted what is
  there, and one that is not there is left out rather than failing the join.

**Packed audio is placed where its tag says** (`packedaudio.go`). HLS can
carry audio with no container, ADTS AAC segment after segment, each opening
with an ID3 tag whose PRIV frame
(`com.apple.streaming.transportStreamTimestamp`) holds its 33-bit 90 kHz
timestamp. ffmpeg's ADTS reader ignores the tag and starts the audio at zero,
while the TS video beside it keeps its own timestamps (France TV's start at
ten seconds), so joined as it is, the sound would run ten seconds early. The
join reads the tag at the head of each audio part and gives ffmpeg the start
(`-itsoffset`); an fMP4 or TS audio part has no such tag and is left alone.

**A video presented before it is decoded is restamped** (`presentedEarly`).
France TV's packager stamps its B-frames with presentation times earlier than
their decode times, the first frame decoded at the time it is shown. A TS
reader copes; copied into an MP4 the frames come out with repeated times,
which a player shows by dropping some. The probe reads the video part's first
packets, and where any is presented early, the join has ffmpeg work the decode
times out from the presentation times (`-fflags +igndts`) on that input alone.
A sound stream's decode times are left as they were.

ffmpeg is a dependency here, not a convenience: without it the video alone is
silent, so `transfer` refuses such an item before fetching a byte
(`errNoFFmpegToJoin`), and a retry looks for it again. The joined file is
written beside the parts (`joinedPart`), checked by `identicalOnDisk`,
reserved and renamed like any other, and only then are the parts and their
checkpoints removed: a join that fails leaves them, and the retry joins again
without fetching anything.

## Subtitles

**Subtitles come along as text tracks** (`subtitles.go`). A subtitle is a
bonus to the video, never a condition of it, so each step that could fail one
leaves it out instead: a rendition that cannot be read is not listed (above);
a track that cannot be fetched in `SubtitleFetchTimeout` is logged and left
out; a join that fails with subtitles is made again without them. Nothing is
fetched without ffmpeg, which they could not be joined without.

They are fetched at the start of each attempt, while their links are freshest,
and before the stall watchdog starts, since a host slow to serve them has not
stalled the video. `mergeWebVTT` joins a track's files into one: a plain
header, every style and region block once, and then the cues. The other blocks
go, identifiers too, which would repeat across files. A segmented track's
files each say where their cues sit with an `X-TIMESTAMP-MAP`, which can
differ from file to file, so the cues are moved onto the first mapped file's
timeline, counting on across the 90 kHz counter's 33-bit wrap. A cue repeated
across a boundary, same times and text, is kept once. The merged file is
written whole, beside the part (`subtitlePart`), and a file an attempt could
not replace is removed, so the join never takes one that may belong to another
track.

The join makes each an MP4 text track (`mov_text`). ffmpeg reads no
`X-TIMESTAMP-MAP` of its own accord, so where the cues sit is given with
`-itsoffset`: a mapped file's offset puts them on the media's timeline, and an
unmapped file's cues count from the start of the picture, which ffprobe reads
off the video's part. The two are not the same: ffmpeg's own CMAF starts its
picture 23 ms in, after the audio's priming. A playlist that names no audio,
joined only for its subtitles, carries all its audio in the video, and all of
it comes along. ffmpeg's MP4 muxer enables the first subtitle track when none
is default, which is how every tool's MP4 subtitles behave.

## Remuxing

`remux.go` is a convenience on top, never a dependency: where a rendition
carries audio and video together, the extractor picks it. It converts any
finished `.ts`, always by stream copy, and keeps the `.ts` when a lossless
copy is impossible. `mediaprobe.go` chooses what to carry over: the best
video, and the best audio and subtitle track per language. One trap worth
knowing: a copied MP2 stream is reported as `mp3` once inside MP4 because the
MPEG audio layers share an identifier there — the bitstream is byte-identical,
so that is not a re-encode.

## External downloads

**External transfers** (`external.go`, `internal/tools`) cover pages where
reaching the media needs more than HTTP.

**Finding helpers.** `tools.Find` resolves a helper binary next to the running
executable before falling back to PATH, which is what makes
`make dependencies` work without a system install. It caches misses as firmly
as hits, so `RetryJob` and `RetryItem` call `tools.Recheck` first: being told
a helper is missing is what sends someone off to install it, and a service
that went on insisting it was absent afterwards would make a working install
look broken. Hits are kept — a path that resolved once does not stop existing.
A *newly added* job still meets the cached miss; only retrying looks again.

**Running them.** `tools.CommandContext` shares Unix process-group
cancellation and a bounded inherited-pipe wait across metadata, download and
remux helpers. Cancelling must kill the whole process group, since the script
spawns yt-dlp, which spawns ffmpeg. Download progress must be read
concurrently with `Wait`, so that its inherited-pipe deadline actually starts.
`tools.Probe` additionally bounds metadata time and stdout;
`util.BoundedBuffer` keeps diagnostic readers draining without unbounded
allocation.

**Probing.** That bound is why the yt-dlp probes (`youtube.go`) print only the
fields they read with `--print`, one JSON object per line, rather than dumping
with `-J`: a full dump of one ordinary YouTube video can be ten megabytes,
almost all of it machine-translated automatic captions, which would fail the
job before it downloads anything. The probes also pass `--simulate` outright:
`--print` implies it only while every print is at the default stage, and the
`playlist:` print turns it off, so a probe without it would download the whole
video into its working directory.

**The download script.** The download runs through an embedded shell script
(`internal/tools/scripts/yt-download.sh`), overridable by a copy next to the
binary; it reports `PROGRESS`/`FILE` lines that are folded back into normal
item state. yt-dlp's pieces go to `PARTS`, a hidden
`.heapleach/<hash of the page>` beside the destination, which the script
passes on as `--paths temp:`. Each stream and the half-written merge carry a
media extension, so if they sat in the download folder, a media library
watching it would take each of a dubbed video's streams for a video of its
own. The directory is on the same filesystem, so the finished file arrives by
rename. `clearExternalParts` removes it after a success (the shared parent
only once empty) and leaves it after a failure, since `--no-part` resumes from
it.

**The format selector keeps every audio language**, not just the best one. A
dubbed release ranks its original track first, so `ba` alone hands back that
one and silently drops the dubs beside it. Each language is therefore named —
`bv*+ba[language=en-US]+ba[language=ar]+…` — which costs one extraction before
the download to learn what they are, falls back to the plain pair whenever
that probe says nothing useful, and skips the naming entirely for the
single-language case that is most videos. Two flags are load-bearing and
neither is obvious: without `--audio-multistreams` yt-dlp downloads the extra
languages and then keeps one, and without `--embed-metadata` the merge labels
every track with the first one's tag, so every language claims to be the first
and a player cannot offer the choice. That second pass rewrites the finished
file, so it is asked for only when there is more than one language to label.
`mergeall[vcodec=none]`, the documented way to take "all audio", is not what
is wanted: it takes all *formats*, which for a video in four languages can be
seven encodings of each.

**The progress line** is the fiddliest part, and both of its traps produce a
display that looks plausible and is wrong. yt-dlp reports **one file at a
time**, so a merged download reports the video stream and then the audio, each
counting from zero with a total of its own; passed through as they arrive, a
large download would finish reading as small as its audio. The format id ends
the progress template so the streams can be told apart and added up, with a
collapsed byte counter standing in when an overridden script predates that
field. And a fragmented download has no length to state, so the total comes
back as a **float** average (`129456458.22222222`), which `ParseInt` refuses;
it is parsed as a float after that, since dropping the error would drop every
honest total with it and leave an early estimate that happened to be whole
stuck for the whole download.

Two more things matter. `--print` implies `--quiet` in yt-dlp, so `--progress`
is required or no progress is emitted at all. And `DENO` is passed by path
rather than left to be found, because yt-dlp looks for a JavaScript runtime on
PATH and `./bin` is exactly where PATH does not reach. Extraction works
without one, but yt-dlp calls that deprecated and warns that formats may be
missing; for an ordinary video it makes no difference to the format list, so
this is insurance against the deprecation rather than a fix for anything
visible.
