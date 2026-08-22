// Command screenshots serves the real web UI over a made-up queue, so the
// pictures in docs/ can be regenerated without downloading anything.
//
// Nothing here touches the download engine, and nothing needs to: the server
// precomputes every aggregate the UI displays, so the UI renders straight
// from one snapshot. Feeding it a canned one therefore produces the genuine
// page rather than a mock-up of it. What the page shows is invented — every
// job below is fiction on reserved example domains.
//
// A development command, driven by docs/screenshots.sh.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/cli"
	"github.com/JohanLindvall/HeapLeach/internal/download"
	"github.com/JohanLindvall/HeapLeach/internal/webui"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8099", "address to serve on")
	terminal := flag.Bool("cli", false, "print the terminal display's frame and exit")
	width := flag.Int("width", 96, "terminal width for -cli")
	flag.Parse()

	// The same scene, drawn by the display's own code rather than served to
	// a browser: the headless run's picture and the web UI's are then two
	// views of one queue.
	if *terminal {
		for _, line := range cli.Frame(scene(), *width, 11*time.Second) {
			fmt.Println(line)
		}
		return
	}

	assets, err := webui.FS()
	if err != nil {
		log.Fatalf("read embedded UI: %v", err)
	}
	if !webui.Built() {
		log.Fatal("the embedded UI is only the placeholder — run `make frontend` first")
	}
	index, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		log.Fatalf("read index.html: %v", err)
	}

	snapshot := scene()
	frame, err := json.Marshal(snapshot)
	if err != nil {
		log.Fatalf("encode scene: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write(frame)
	})

	files := http.FileServerFS(assets)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			files.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, seedPage(string(index), r.URL.Query().Get("theme"), string(frame)))
	})

	log.Printf("screenshot scene on http://%s", *addr)
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

// seedPage stamps the browser state a screenshot needs before the app boots.
//
// Three things, all written by a script inserted ahead of the page's own:
// which theme to wear, so the picture does not depend on what the capturing
// browser prefers; a progress record, so the panel has something to show;
// and a stand-in for EventSource.
//
// That last one is not decoration. The app opens a server-sent-events stream
// and lights its "live" badge when the stream opens, but a headless capture
// waits for the page to fall idle and a stream never lets it — the browser
// hangs until it is killed, with no picture to show for it. The stand-in
// delivers one frame and holds no connection, so the badge is honest and the
// page settles.
func seedPage(index, theme, frame string) string {
	if theme != "light" {
		theme = "dark"
	}
	seed := "<script>try{" +
		"localStorage.setItem('heapleach.theme'," + quoted(theme) + ");" +
		"localStorage.setItem('heapleach.progress.v1'," + quoted(progressSeed()) + ");" +
		"}catch(e){}" +
		"window.EventSource=function(){var s={readyState:1,close:function(){}};" +
		"setTimeout(function(){if(s.onopen)s.onopen({});" +
		"if(s.onmessage)s.onmessage({data:" + quoted(frame) + "});},0);return s;};" +
		"</script>"
	return strings.Replace(index, "<head>", "<head>"+seed, 1)
}

func quoted(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// progressSeed is the persisted progress the panel reads.
//
// countedItems names every finished item in the scene, so folding the
// snapshot in adds nothing: the totals stay where they are put, and no
// achievement pops a notification into the picture.
func progressSeed() string {
	b, _ := json.Marshal(map[string]any{
		"filesCompleted":  128,
		"bytesDownloaded": 41_000_000_000,
		"peakSpeed":       58_000_000,
		"maxParallel":     8,
		"hostsUsed":       []string{"bunkr", "erome", "gofile", "mega", "pixhost", "youtube"},
		"unlocked":        []string{"first-blood", "ten-files", "hundred-files", "one-gig", "ten-gigs", "fast-50", "parallel-8"},
		"countedItems":    finishedItemIDs(),
	})
	return string(b)
}

const (
	kB = 1 << 10
	MB = 1 << 20
	GB = 1 << 30
)

// scene is the queue the pictures show: a mix of states, so one frame covers
// running transfers, a split file, a finished job and one still queued.
func scene() download.Snapshot {
	now := time.Now()
	jobs := []download.JobView{
		{
			ID: "j1", Title: "Coastal Drone Reel — 4K", Host: "bunkr",
			Source: "https://example.com/a/9fL2xQ", Status: download.StatusRunning,
			CreatedAt: now.Add(-4 * time.Minute), SizeKnown: true,
			Total: 1, Active: 1, Size: 1800 * MB, Downloaded: 326 * MB, Speed: 13.2 * MB,
			Items: []download.ItemView{{
				ID: "i1", Name: "coastal-drone-reel-2160p.mp4", Status: download.StatusRunning,
				Size: 1800 * MB, Downloaded: 326 * MB, Speed: 13.2 * MB, Streams: 5, Elapsed: 26,
			}},
		},
		{
			ID: "j2", Title: "Studio Session — Photo Set", Host: "pixeldrain",
			Source: "https://example.com/a/7Kd0mB", Status: download.StatusRunning,
			CreatedAt: now.Add(-3 * time.Minute), SizeKnown: true,
			Total: 5, Done: 2, Active: 1, Size: 96 * MB, Downloaded: 61 * MB, Speed: 6.4 * MB,
			Items: []download.ItemView{
				{ID: "i2", Name: "studio-session-014.jpg", Status: download.StatusDone,
					Size: 18 * MB, Downloaded: 18 * MB, Path: "Studio Session/studio-session-014.jpg", Elapsed: 4},
				{ID: "i3", Name: "studio-session-015.jpg", Status: download.StatusDone,
					Size: 21 * MB, Downloaded: 21 * MB, Path: "Studio Session/studio-session-015.jpg", Elapsed: 5},
				{ID: "i4", Name: "studio-session-016.mp4", Status: download.StatusRunning,
					Size: 31 * MB, Downloaded: 22 * MB, Speed: 6.4 * MB, Streams: 2, Elapsed: 9},
				{ID: "i5", Name: "studio-session-017.mp4", Status: download.StatusQueued, Size: 14 * MB},
				{ID: "i6", Name: "studio-session-018.mp4", Status: download.StatusQueued, Size: 12 * MB},
			},
		},
		{
			ID: "j3", Title: "Aurora Timelapse", Host: "mega",
			Source: "https://example.com/file/4Tq8ZP", Status: download.StatusRunning,
			CreatedAt: now.Add(-2 * time.Minute), SizeKnown: true,
			Total: 1, Active: 1, Size: 46 * MB, Downloaded: 34*MB + 600*kB, Speed: 4.1 * MB,
			Items: []download.ItemView{{
				ID: "i7", Name: "aurora-timelapse-04k.mp4", Status: download.StatusRunning,
				Size: 46 * MB, Downloaded: 34*MB + 600*kB, Speed: 4.1 * MB, Streams: 2, Elapsed: 11,
			}},
		},
		{
			ID: "j4", Title: "Field Recordings vol. 3", Host: "gofile",
			Source: "https://example.com/d/Rm2xVt", Status: download.StatusQueued,
			CreatedAt: now.Add(-90 * time.Second), SizeKnown: true,
			Total: 6, Size: 212 * MB,
			Items: []download.ItemView{
				{ID: "i8", Name: "field-recordings-vol3-01.flac", Status: download.StatusQueued, Size: 38 * MB},
				{ID: "i9", Name: "field-recordings-vol3-02.flac", Status: download.StatusQueued, Size: 41 * MB},
				{ID: "i10", Name: "field-recordings-vol3-03.flac", Status: download.StatusQueued, Size: 33 * MB},
				{ID: "i11", Name: "field-recordings-vol3-04.flac", Status: download.StatusQueued, Size: 36 * MB},
				{ID: "i12", Name: "field-recordings-vol3-05.flac", Status: download.StatusQueued, Size: 30 * MB},
				{ID: "i13", Name: "field-recordings-vol3-06.flac", Status: download.StatusQueued, Size: 34 * MB},
			},
		},
		{
			ID: "j5", Title: "Wildlife s01e03 — The Delta", Host: "youtube",
			Source: "https://example.com/watch/Xa71", Status: download.StatusDone,
			CreatedAt: now.Add(-9 * time.Minute), SizeKnown: true,
			Total: 1, Done: 1, Size: 742 * MB, Downloaded: 742 * MB,
			Items: []download.ItemView{{
				ID: "i14", Name: "wildlife-s01e03-the-delta.mp4", Status: download.StatusDone,
				Size: 742 * MB, Downloaded: 742 * MB, Path: "wildlife-s01e03-the-delta.mp4", Elapsed: 63,
			}},
		},
	}

	return download.Snapshot{
		Jobs:        jobs,
		Concurrency: 4,
		MaxConcur:   32,
		Streams:     8,
		MaxStreams:  16,
		Active:      3,
		Queued:      8,
		SpeedLimit:  25 * MB,
		Speed:       23.7 * MB,
		DownloadDir: "/home/you/Downloads",
		HostCount:   35,
	}
}

// finishedItemIDs lists the items the scene shows as complete.
func finishedItemIDs() []string {
	var ids []string
	for _, job := range scene().Jobs {
		for _, item := range job.Items {
			if item.Status == download.StatusDone {
				ids = append(ids, item.ID)
			}
		}
	}
	return ids
}
