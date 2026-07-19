# Smart Home

Surveillance camera and video doorbell storage. Devices group into
**spaces** (home, workshop, the cabin); a standalone agent, `pcp-camd`,
runs on a box near the cameras, pulls their streams, and pushes ~4-second
recordings to PCP over plain HTTPS — through cloudferry or direct. No
inbound ports at the house, ever.

## Supported hardware

Anything that speaks **RTSP with H.264** works — the near-universal
protocol for surveillance cameras and a large share of doorbells.
Cloud-locked devices with no local stream (Ring, Nest) do **not** work;
PCP does not integrate vendor clouds. H.265-only cameras work with the
per-camera "Transcode" option (CPU cost lands on the agent box). Browser
playback requires H.264.

## Storage math

Size disks before pairing, not after: a 1080p camera at ~2 Mbps writes
**~21 GB/day** of continuous footage. Retention defaults to 7 days per
space (per-camera overrides; the admin caps the maximum). Footage counts
against the space owner's storage quota; when the owner is over quota,
recording **stops loudly** — the agent spools and warns, nothing
silently degrades.

## Setup

1. An admin enables Smart Home on **Admin → Services**, then grants
   users on **Admin → Smart Home** (creation is allowlisted by default;
   anyone can still be invited into an existing space).
2. Create a space, click **Pair an agent**, and run the shown command on
   the agent box within 10 minutes:

   ```
   pcp-camd pair https://pcp.example.org XXXX-XXXX-XXXX
   pcp-camd run
   ```

   `pcp-camd` needs `ffmpeg` on PATH for real cameras. State (including
   the agent credential and the upload spool) lives in `-state DIR`
   (default `./pcp-camd-state`).
3. Add devices from the space page. The one question — security camera
   or doorbell? — tunes the defaults; everything else has a working
   default. Stream URLs look like `rtsp://user:pass@192.168.1.20:554/ch0`;
   give the low-res substream too when the camera has one (cheaper
   motion detection and thumbnails).

## Recording

- **Continuous** (default, recommended): everything within retention.
- **Events only** (experimental): a rolling pre-buffer uploads 8 s
  before to 20 s after each event. Event-only systems live or die on
  detection quality; the label manages expectations.
- **Audio is opt-in per camera** — the agent strips the audio track
  unless "Record audio" is checked.
- Outages lose nothing: the agent spools to disk (2 GB/camera,
  oldest-dropped) and backfills in order on reconnect.

## Events

Motion (agent-side scene detection, or camera-reported via the agent's
LAN callback `http://<agent>:8480/event?camera=<name>&kind=ring|motion`),
doorbell rings, and agent offline/online transitions land on the
timeline, the space activity feed, and — for rings — every member's
notifications (viewers included; each member tunes their own
preferences). Operators mark events reviewed.

## Watching

A camera page opens **live** (~2–4 s delay while watched — cameras drop
to 1-second segments automatically). The timeline below shows coverage
honestly (gaps stay visible), event markers, hover thumbnails, and
zooms 24 h → 5 min. Keys: space play/pause · ←/→ seek · j/k events ·
l live · c select a range. The activity page's search accepts typed
filters — `camera:frontdoor kind:ring after:2026-07-01 acked:no` — plus
free text.

## Clips & deleting footage

Select a range and save it as a **clip**: pinned past retention until
deleted, exportable as one MP4, shareable by revocable tokened link
(optional expiry; the link never reaches outside the shared range), and
copyable into your Drive when Drive is enabled. Deletion tools are
first-class: a selected range, a whole day, or a camera's entire
history — permanent, audited, quota-refunded, and never taking
clip-pinned footage without naming the clips and asking explicitly.

## Roles

`owner` (members, agents, retention, the space itself) · `operator`
(cameras, clips, footage deletion, event review, day-to-day) · `viewer`
(live, timeline, events, doorbell rings). Sharing is space membership —
invite any PCP user.

## API

Scopes `smarthome:read` / `smarthome:write` cover the phone-app surface
under `/api/v1/smarthome/` (spaces, timeline index, Range-served
segments and thumbnails, events with filters, ack, clips, live-boost).
Agents authenticate with their own pairing-issued tokens — never user
API keys — and only on the ingest endpoints.
