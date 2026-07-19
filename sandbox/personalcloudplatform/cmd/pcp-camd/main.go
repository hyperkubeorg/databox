// Command pcp-camd is the Smart Home camera agent (PROJECT-DRAFT-005
// §4): it runs on a box near the cameras, pulls their RTSP streams,
// cuts ~4-second MP4 segments with ffmpeg, and PUSHES them to PCP over
// plain HTTPS — through cloudferry or direct, never an inbound port.
// Segments land in an on-disk spool first and upload oldest-first, so a
// dead link loses nothing: the spool backfills on reconnect (§4.4).
//
//	pcp-camd pair <pcp-url> <code> [-name workshop] [-state DIR]
//	pcp-camd run [-state DIR]
//
// Camera configuration lives on the PCP server (added in the Smart Home
// UI) and arrives over the hanging command poll, which doubles as the
// heartbeat. Stream URL schemes:
//
//	rtsp://…   a real camera (requires ffmpeg on PATH)
//	file:PATH  loop a local video file as if it were a camera (ffmpeg)
//	loop:PATH  dev/test source: the file's bytes become each segment,
//	           no ffmpeg needed — this is what the smoke tests use
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// segmentSeconds is the nominal segment length (§4.3); live-boost
// (phase 4) shortens it while watchers are present.
const segmentSeconds = 4

// spoolCapBytes bounds one camera's spool (§4.4): oldest-dropped.
const spoolCapBytes = 2 << 30

// state is the paired credential file (state.json in the state dir).
type state struct {
	URL     string `json:"url"`
	Token   string `json:"token"`
	AgentID string `json:"agent_id"`
	SpaceID string `json:"space_id"`
}

// camera mirrors the server's command-channel payload.
type camera struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Doorbell  bool   `json:"doorbell"`
	Stream    string `json:"stream"`
	Substream string `json:"substream"`
	Mode      string `json:"mode"`
	Motion    string `json:"motion"`
	Audio     bool   `json:"audio"`
	Transcode bool   `json:"transcode"`
	// BoostUntilMs is the live-boost lease (§7.1): 1-second segments
	// while it is in the future, reverting locally at expiry.
	BoostUntilMs int64 `json:"boost_until_ms"`
}

// sansBoost is the camera identity without the transient boost lease —
// a lease renewal must extend the boost, never restart the capture.
func (c camera) sansBoost() camera { c.BoostUntilMs = 0; return c }

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: pcp-camd pair <pcp-url> <code> [-name NAME] [-state DIR]\n       pcp-camd run [-state DIR]")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "pair":
		pairCmd(log, os.Args[2:])
	case "run":
		runCmd(log, os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q (pair | run)\n", os.Args[1])
		os.Exit(2)
	}
}

// defaultState resolves the state directory flag default.
func defaultState() string {
	if v := os.Getenv("PCP_CAMD_STATE"); v != "" {
		return v
	}
	return "./pcp-camd-state"
}

// --- pair -------------------------------------------------------------------

func pairCmd(log *slog.Logger, args []string) {
	fs := flag.NewFlagSet("pair", flag.ExitOnError)
	name := fs.String("name", "", "agent display name (default: hostname)")
	stateDir := fs.String("state", defaultState(), "state directory")
	rest := []string{}
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			break
		}
		rest = append(rest, a)
	}
	_ = fs.Parse(args[len(rest):])
	if len(rest) != 2 {
		fmt.Fprintln(os.Stderr, "usage: pcp-camd pair <pcp-url> <code> [-name NAME] [-state DIR]")
		os.Exit(2)
	}
	url, code := strings.TrimRight(rest[0], "/"), rest[1]
	if *name == "" {
		*name, _ = os.Hostname()
	}
	body, _ := json.Marshal(map[string]string{"code": code, "name": *name})
	resp, err := http.Post(url+"/api/v1/smarthome/ingest/pair", "application/json", bytes.NewReader(body))
	if err != nil {
		log.Error("pairing failed", "err", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	var out struct {
		OK      bool   `json:"ok"`
		Error   string `json:"error"`
		Token   string `json:"token"`
		AgentID string `json:"agent_id"`
		SpaceID string `json:"space_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || !out.OK {
		log.Error("pairing refused", "status", resp.Status, "err", out.Error)
		os.Exit(1)
	}
	st := state{URL: url, Token: out.Token, AgentID: out.AgentID, SpaceID: out.SpaceID}
	if err := os.MkdirAll(*stateDir, 0o700); err != nil {
		log.Error("state dir", "err", err)
		os.Exit(1)
	}
	raw, _ := json.MarshalIndent(st, "", "  ")
	if err := os.WriteFile(filepath.Join(*stateDir, "state.json"), raw, 0o600); err != nil {
		log.Error("state write", "err", err)
		os.Exit(1)
	}
	fmt.Printf("paired ✓  agent %s → %s\nstate saved in %s — now run: pcp-camd run -state %s\n",
		out.AgentID, url, *stateDir, *stateDir)
}

// --- run --------------------------------------------------------------------

// agentd is the running daemon: credentials, HTTP client, and the
// per-camera runner set.
type agentd struct {
	log    *slog.Logger
	st     state
	dir    string
	client *http.Client

	mu      sync.Mutex
	runners map[string]*runner
}

func runCmd(log *slog.Logger, args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	stateDir := fs.String("state", defaultState(), "state directory")
	_ = fs.Parse(args)
	raw, err := os.ReadFile(filepath.Join(*stateDir, "state.json"))
	if err != nil {
		log.Error("not paired yet — run: pcp-camd pair <pcp-url> <code>", "err", err)
		os.Exit(1)
	}
	var st state
	if err := json.Unmarshal(raw, &st); err != nil {
		log.Error("bad state.json", "err", err)
		os.Exit(1)
	}
	a := &agentd{
		log: log, st: st, dir: *stateDir,
		client:  &http.Client{Timeout: 60 * time.Second},
		runners: map[string]*runner{},
	}
	log.Info("pcp-camd running", "pcp", st.URL, "agent", st.AgentID)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go a.commandLoop()
	go a.webhookListener()
	<-stop
	log.Info("stopping")
	a.mu.Lock()
	for _, r := range a.runners {
		r.stop()
	}
	a.mu.Unlock()
}

// commandLoop is the hanging config poll (§4.4): the heartbeat, and the
// reconcile trigger when the camera set changes.
func (a *agentd) commandLoop() {
	var rev int64 = -1
	for {
		resp, err := a.get(fmt.Sprintf("/api/v1/smarthome/ingest/commands?rev=%d", rev))
		if err != nil {
			a.log.Warn("command poll failed — retrying", "err", err)
			time.Sleep(5 * time.Second)
			continue
		}
		var out struct {
			OK      bool     `json:"ok"`
			Error   string   `json:"error"`
			Rev     int64    `json:"rev"`
			Cameras []camera `json:"cameras"`
		}
		err = json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		if err != nil || !out.OK {
			if resp.StatusCode == http.StatusUnauthorized {
				a.log.Error("agent token revoked — re-pair from the space settings")
				time.Sleep(30 * time.Second)
				continue
			}
			a.log.Warn("command poll refused", "status", resp.Status, "err", out.Error)
			time.Sleep(5 * time.Second)
			continue
		}
		if out.Rev != rev {
			a.log.Info("camera config", "rev", out.Rev, "cameras", len(out.Cameras))
			a.reconcile(out.Cameras)
			rev = out.Rev
		}
	}
}

// webhookListener is the LAN callback endpoint (§4.3) cameras and
// doorbells with an "HTTP notify" feature point at:
//
//	http://<agent>:8480/event?camera=<name>&kind=ring|motion
//
// The camera is matched by configured name (case-insensitive). LAN-only
// trust by design — bind it with PCP_CAMD_LISTEN, or set it empty to
// turn the listener off.
func (a *agentd) webhookListener() {
	addr := os.Getenv("PCP_CAMD_LISTEN")
	if addr == "" {
		addr = ":8480"
	}
	if addr == "off" {
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/event", func(w http.ResponseWriter, r *http.Request) {
		name := strings.ToLower(r.URL.Query().Get("camera"))
		kind := r.URL.Query().Get("kind")
		if kind != "ring" && kind != "motion" {
			http.Error(w, "kind must be ring or motion", http.StatusBadRequest)
			return
		}
		a.mu.Lock()
		var target *runner
		for _, rn := range a.runners {
			if strings.ToLower(rn.cam.Name) == name {
				target = rn
			}
		}
		a.mu.Unlock()
		if target == nil {
			http.Error(w, "no camera by that name", http.StatusNotFound)
			return
		}
		target.postEvent(kind, "camera-reported")
		fmt.Fprintln(w, "ok")
	})
	if err := http.ListenAndServe(addr, mux); err != nil {
		a.log.Warn("webhook listener failed", "addr", addr, "err", err)
	}
}

// reconcile starts/stops per-camera runners to match the config.
func (a *agentd) reconcile(cams []camera) {
	a.mu.Lock()
	defer a.mu.Unlock()
	want := map[string]camera{}
	for _, c := range cams {
		want[c.ID] = c
	}
	for id, r := range a.runners {
		c, keep := want[id]
		if keep && r.configEqual(c) {
			r.setBoost(c.BoostUntilMs)
			delete(want, id)
			continue
		}
		r.stop()
		delete(a.runners, id)
	}
	for id, c := range want {
		if needsFFmpeg(c.Stream) && !ffmpegPresent() {
			a.log.Error("camera needs ffmpeg on PATH — skipping", "camera", c.Name)
			continue
		}
		r := newRunner(a, c)
		a.runners[id] = r
		go r.run()
	}
}

func ffmpegPresent() bool { _, err := exec.LookPath("ffmpeg"); return err == nil }

func needsFFmpeg(stream string) bool { return !strings.HasPrefix(stream, "loop:") }

// --- HTTP helpers -----------------------------------------------------------

func (a *agentd) get(path string) (*http.Response, error) {
	req, _ := http.NewRequest(http.MethodGet, a.st.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+a.st.Token)
	return a.client.Do(req)
}

func (a *agentd) post(path string, body io.Reader, contentType string) (*http.Response, error) {
	req, _ := http.NewRequest(http.MethodPost, a.st.URL+path, body)
	req.Header.Set("Authorization", "Bearer "+a.st.Token)
	req.Header.Set("Content-Type", contentType)
	return a.client.Do(req)
}

// --- per-camera runner ------------------------------------------------------

// runner captures one camera into its spool and uploads oldest-first.
type runner struct {
	a    *agentd
	cam  camera
	dir  string // spool/<camID>
	done chan struct{}
	once sync.Once
	cmd  *exec.Cmd
	cmdM sync.Mutex

	boostM  sync.Mutex
	boostMs int64
	// windows are the open events-mode upload spans [fromMs, toMs)
	// (guarded by boostM).
	windows [][2]int64
}

func newRunner(a *agentd, c camera) *runner {
	r := &runner{a: a, cam: c, dir: filepath.Join(a.dir, "spool", c.ID), done: make(chan struct{})}
	r.boostMs = c.BoostUntilMs
	return r
}

// configEqual ignores the boost lease — reconcile handles that without
// a capture restart.
func (r *runner) configEqual(c camera) bool { return r.cam.sansBoost() == c.sansBoost() }

// setBoost extends/clears the lease; the capture watcher notices a
// segment-length transition and restarts ffmpeg only then.
func (r *runner) setBoost(untilMs int64) {
	r.boostM.Lock()
	r.boostMs = untilMs
	r.boostM.Unlock()
}

// segSeconds is the current segment length: 1s while boosted (§7.1).
func (r *runner) segSeconds() int {
	r.boostM.Lock()
	defer r.boostM.Unlock()
	if time.Now().UnixMilli() < r.boostMs {
		return 1
	}
	return segmentSeconds
}

func (r *runner) stop() {
	r.once.Do(func() { close(r.done) })
	r.cmdM.Lock()
	if r.cmd != nil && r.cmd.Process != nil {
		_ = r.cmd.Process.Kill()
	}
	r.cmdM.Unlock()
}

func (r *runner) run() {
	if err := os.MkdirAll(r.dir, 0o700); err != nil {
		r.a.log.Error("spool dir", "camera", r.cam.Name, "err", err)
		return
	}
	go r.uploadLoop()
	if r.cam.Motion == "agent" && needsFFmpeg(r.cam.Stream) && ffmpegPresent() {
		go r.motionLoop()
	}
	for {
		select {
		case <-r.done:
			return
		default:
		}
		var err error
		if strings.HasPrefix(r.cam.Stream, "loop:") {
			err = r.captureLoopSource()
		} else {
			err = r.captureFFmpeg()
		}
		if err != nil {
			r.a.log.Warn("capture ended — restarting", "camera", r.cam.Name, "err", err)
		}
		select {
		case <-r.done:
			return
		case <-time.After(3 * time.Second):
		}
	}
}

// captureLoopSource is the no-ffmpeg dev/test source: every segment
// period the target file's bytes become one "segment" stamped with the
// period's start. It exercises pairing, ingest, spool, backfill, and
// live-boost end-to-end with zero hardware.
func (r *runner) captureLoopSource() error {
	path := strings.TrimPrefix(r.cam.Stream, "loop:")
	src, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	for {
		seg := r.segSeconds()
		start := time.Now()
		select {
		case <-r.done:
			return nil
		case <-time.After(time.Duration(seg) * time.Second):
		}
		name := fmt.Sprintf("seg-%d-%d.mp4", start.UnixMilli(), seg*1000)
		if err := os.WriteFile(filepath.Join(r.dir, name+".tmp"), src, 0o600); err != nil {
			return err
		}
		if err := os.Rename(filepath.Join(r.dir, name+".tmp"), filepath.Join(r.dir, name)); err != nil {
			return err
		}
		r.enforceSpoolCap()
	}
}

// captureFFmpeg runs one ffmpeg for a real (rtsp://) or looped (file:)
// source, segmenting into the spool with epoch-stamped names. Video is
// stream-copied (or transcoded to H.264 when configured); audio is
// STRIPPED unless opted in (§4.3). A live-boost transition kills the
// process (the run loop restarts it at the new segment length).
func (r *runner) captureFFmpeg() error {
	segLen := r.segSeconds()
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin"}
	switch {
	case strings.HasPrefix(r.cam.Stream, "file:"):
		args = append(args, "-re", "-stream_loop", "-1", "-i", strings.TrimPrefix(r.cam.Stream, "file:"))
	default:
		args = append(args, "-rtsp_transport", "tcp", "-i", r.cam.Stream)
	}
	if r.cam.Transcode {
		args = append(args, "-c:v", "libx264", "-preset", "veryfast", "-tune", "zerolatency")
	} else {
		args = append(args, "-c:v", "copy")
	}
	if r.cam.Audio {
		args = append(args, "-c:a", "aac")
	} else {
		args = append(args, "-an")
	}
	args = append(args,
		"-f", "segment",
		"-segment_time", strconv.Itoa(segLen),
		"-reset_timestamps", "1",
		"-segment_format", "mp4",
		"-segment_format_options", "movflags=+frag_keyframe+empty_moov+default_base_moof",
		"-strftime", "1",
		filepath.Join(r.dir, fmt.Sprintf("ff%d-%%s.mp4", segLen)),
	)
	// Poster thumbnails (§7.2 hover previews): one JPEG per segment
	// period, from the substream when the camera has one (cheap decode)
	// — a separate ffmpeg below — else a second output here.
	if r.cam.Substream == "" {
		args = append(args,
			"-map", "0:v:0", "-vf", "fps=1/"+strconv.Itoa(segmentSeconds),
			"-q:v", "6", "-strftime", "1",
			filepath.Join(r.dir, "thumb-%s.jpg"),
		)
	} else {
		go r.captureThumbsFromSubstream()
	}
	cmd := exec.Command("ffmpeg", args...)
	cmd.Stderr = os.Stderr
	r.cmdM.Lock()
	r.cmd = cmd
	r.cmdM.Unlock()
	if err := cmd.Start(); err != nil {
		return err
	}
	// Kill on a boost transition so the run loop restarts at the new
	// segment length.
	go func() {
		for {
			select {
			case <-r.done:
				return
			case <-time.After(time.Second):
				if r.segSeconds() != segLen {
					_ = cmd.Process.Kill()
					return
				}
			}
		}
	}()
	err := cmd.Wait()
	return err
}

// captureThumbsFromSubstream runs a low-res ffmpeg for posters only —
// decoding the substream costs a fraction of decoding the main stream.
// Best-effort: it dies quietly with the runner.
func (r *runner) captureThumbsFromSubstream() {
	for {
		select {
		case <-r.done:
			return
		default:
		}
		cmd := exec.Command("ffmpeg",
			"-hide_banner", "-loglevel", "error", "-nostdin",
			"-rtsp_transport", "tcp", "-i", r.cam.Substream,
			"-vf", "fps=1/"+strconv.Itoa(segmentSeconds), "-q:v", "6",
			"-strftime", "1", filepath.Join(r.dir, "thumb-%s.jpg"))
		_ = cmd.Run()
		select {
		case <-r.done:
			return
		case <-time.After(5 * time.Second):
		}
	}
}

// --- events ------------------------------------------------------------------

// eventDebounce spaces motion events (§4.3): one motion event per quiet
// period, not one per frame.
const eventDebounce = 30 * time.Second

// preRollMs/postRollMs bound the events-mode upload window (§4.3).
const (
	preRollMs  = 8_000
	postRollMs = 20_000
)

// postEvent reports one event to PCP and, in events mode, opens the
// upload window around it.
func (r *runner) postEvent(kind, detail string) {
	at := time.Now().UnixMilli()
	if r.cam.Mode == "events" {
		r.boostM.Lock()
		r.windows = append(r.windows, [2]int64{at - preRollMs, at + postRollMs})
		if len(r.windows) > 100 {
			r.windows = r.windows[len(r.windows)-100:]
		}
		r.boostM.Unlock()
	}
	body, _ := json.Marshal(map[string]any{"cam": r.cam.ID, "kind": kind, "at_ms": at, "detail": detail})
	resp, err := r.a.post("/api/v1/smarthome/ingest/event", bytes.NewReader(body), "application/json")
	if err != nil {
		r.a.log.Warn("event post failed", "camera", r.cam.Name, "kind", kind, "err", err)
		return
	}
	resp.Body.Close()
	r.a.log.Info("event", "camera", r.cam.Name, "kind", kind)
}

// inWindow reports whether a segment intersects an open events-mode
// upload window.
func (r *runner) inWindow(startMs, durMs int64) bool {
	r.boostM.Lock()
	defer r.boostM.Unlock()
	for _, w := range r.windows {
		if startMs < w[1] && startMs+durMs > w[0] {
			return true
		}
	}
	return false
}

// motionLoop runs ffmpeg scene-change detection over the substream
// (or the main stream) and posts debounced motion events (§4.3).
func (r *runner) motionLoop() {
	src := r.cam.Substream
	if src == "" {
		src = r.cam.Stream
	}
	var last time.Time
	for {
		select {
		case <-r.done:
			return
		default:
		}
		args := []string{"-hide_banner", "-nostdin", "-loglevel", "info"}
		if strings.HasPrefix(src, "file:") {
			args = append(args, "-re", "-stream_loop", "-1", "-i", strings.TrimPrefix(src, "file:"))
		} else {
			args = append(args, "-rtsp_transport", "tcp", "-i", src)
		}
		args = append(args, "-vf", "select='gt(scene,0.08)',showinfo", "-an", "-f", "null", "-")
		cmd := exec.Command("ffmpeg", args...)
		stderr, err := cmd.StderrPipe()
		if err == nil && cmd.Start() == nil {
			buf := make([]byte, 4096)
			for {
				n, rerr := stderr.Read(buf)
				if n > 0 && bytes.Contains(buf[:n], []byte("pts_time")) && time.Since(last) > eventDebounce {
					last = time.Now()
					r.postEvent("motion", "")
				}
				if rerr != nil {
					break
				}
			}
			_ = cmd.Wait()
		}
		select {
		case <-r.done:
			return
		case <-time.After(5 * time.Second):
		}
	}
}

// uploadLoop drains the spool oldest-first (§4.4): success deletes the
// file; failure leaves it for the next pass, so an outage backfills in
// order with no gaps. In events mode only segments inside an event
// window (or the live-boost lease) upload; the rest age out of the
// rolling pre-buffer locally.
func (r *runner) uploadLoop() {
	for {
		select {
		case <-r.done:
			return
		case <-time.After(time.Second):
		}
		files := r.spoolFiles()
		for i, f := range files {
			base := filepath.Base(f.name)
			if strings.HasPrefix(base, "thumb-") {
				continue // posters ride along with their segment below
			}
			// The newest ffmpeg file may still be mid-write: skip it
			// until a newer one exists or it has gone quiet.
			if i == len(files)-1 && strings.HasPrefix(base, "ff") &&
				time.Since(f.mod) < 2*segmentSeconds*time.Second {
				continue
			}
			startMs, durMs, ok := parseSegName(base, f.mod)
			if !ok {
				_ = os.Remove(f.name)
				continue
			}
			if r.cam.Mode == "events" && !r.inWindow(startMs, durMs) {
				// Rolling pre-buffer: keep the recent past for the next
				// event's pre-roll, drop what's aged out.
				if time.Now().UnixMilli()-startMs > 2*preRollMs {
					_ = os.Remove(f.name)
				}
				continue
			}
			if err := r.uploadSegment(f.name, startMs, durMs); err != nil {
				r.a.log.Warn("upload failed — spooling", "camera", r.cam.Name, "err", err)
				break // keep order: retry this file first next pass
			}
			_ = os.Remove(f.name)
			r.uploadThumbFor(startMs, durMs)
		}
		r.enforceSpoolCap()
	}
}

type spoolFile struct {
	name string
	size int64
	mod  time.Time
}

// spoolFiles lists the camera's spool, oldest first by name (both
// naming schemes embed the epoch, so name order IS time order).
func (r *runner) spoolFiles() []spoolFile {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return nil
	}
	var out []spoolFile
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".tmp") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, spoolFile{name: filepath.Join(r.dir, e.Name()), size: info.Size(), mod: info.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// parseSegName extracts (startMs, durMs) from a spool filename:
// seg-<startMs>-<durMs>.mp4 (loop source) or ff<segLen>-<epochSec>.mp4
// (ffmpeg strftime — the segment length rides in the prefix so boosted
// 1-second files index with the right duration).
func parseSegName(base string, mod time.Time) (int64, int64, bool) {
	if rest, ok := strings.CutPrefix(base, "seg-"); ok {
		parts := strings.SplitN(strings.TrimSuffix(rest, ".mp4"), "-", 2)
		if len(parts) == 2 {
			start, err1 := strconv.ParseInt(parts[0], 10, 64)
			dur, err2 := strconv.ParseInt(parts[1], 10, 64)
			if err1 == nil && err2 == nil {
				return start, dur, true
			}
		}
		return 0, 0, false
	}
	if rest, ok := strings.CutPrefix(base, "ff"); ok {
		lenStr, tsStr, found := strings.Cut(strings.TrimSuffix(rest, ".mp4"), "-")
		if !found {
			return 0, 0, false
		}
		segLen, err1 := strconv.ParseInt(lenStr, 10, 64)
		sec, err2 := strconv.ParseInt(tsStr, 10, 64)
		if err1 == nil && err2 == nil && segLen > 0 {
			return sec * 1000, segLen * 1000, true
		}
	}
	return 0, 0, false
}

// uploadSegment POSTs one spooled segment; the server dedupes on
// (camera, start), so retries after a half-delivered request are safe.
func (r *runner) uploadSegment(path string, startMs, durMs int64) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	resp, err := r.a.post(fmt.Sprintf("/api/v1/smarthome/ingest/segment?cam=%s&start_ms=%d&dur_ms=%d",
		r.cam.ID, startMs, durMs), f, "video/mp4")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return err
	}
	if !out.OK {
		// A refused segment (too old for retention, over caps) must not
		// clog the spool: log once and drop it.
		r.a.log.Warn("segment refused — dropping", "camera", r.cam.Name, "err", out.Error)
		return nil
	}
	return nil
}

// uploadThumbFor sends the poster whose stamp falls inside the just-
// uploaded segment's window, keyed to the segment start so the index
// row's marker matches. Best-effort — a missing poster is a blemish.
func (r *runner) uploadThumbFor(startMs, durMs int64) {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		sec, ok := strings.CutPrefix(e.Name(), "thumb-")
		if !ok {
			continue
		}
		ts, err := strconv.ParseInt(strings.TrimSuffix(sec, ".jpg"), 10, 64)
		if err != nil {
			continue
		}
		tsMs := ts * 1000
		if tsMs < startMs || tsMs >= startMs+durMs {
			if tsMs < startMs-60_000 {
				_ = os.Remove(filepath.Join(r.dir, e.Name())) // orphan
			}
			continue
		}
		f, err := os.Open(filepath.Join(r.dir, e.Name()))
		if err != nil {
			continue
		}
		resp, err := r.a.post(fmt.Sprintf("/api/v1/smarthome/ingest/thumb?cam=%s&ts_ms=%d", r.cam.ID, startMs), f, "image/jpeg")
		f.Close()
		if err == nil {
			resp.Body.Close()
			_ = os.Remove(filepath.Join(r.dir, e.Name()))
		}
		return
	}
}

// enforceSpoolCap drops the OLDEST spooled segments once the camera's
// spool exceeds the cap — surveillance keeps the newest evidence.
func (r *runner) enforceSpoolCap() {
	files := r.spoolFiles()
	var total int64
	for _, f := range files {
		total += f.size
	}
	for _, f := range files {
		if total <= spoolCapBytes {
			return
		}
		_ = os.Remove(f.name)
		total -= f.size
	}
}
