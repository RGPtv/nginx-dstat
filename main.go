package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

var (
	flagAddr           = flag.String("addr", ":8765", "listen address for the metrics server")
	flagLog            = flag.String("log", "/var/log/nginx/access.log", "nginx access log path")
	flagStatus         = flag.String("status", "http://127.0.0.1/nginx_status", "nginx stub_status URL")
	flagInterval       = flag.Duration("interval", 1500*time.Millisecond, "push interval to WebSocket clients")
	flagStatusInterval = flag.Duration("status-interval", 2*time.Second, "stub_status fetch interval")
	flagCORS           = flag.String("cors", "*", "CORS origin for the dashboard")
	flagFromStart      = flag.Bool("from-start", false, "tail log from beginning instead of end")
	flagMaxPaths       = flag.Int("max-paths", 10000, "max unique paths to track before pruning smallest")
)

var logRe = regexp.MustCompile(
	`^(?P<ip>\S+)\s+\S+\s+\S+\s+\[(?P<time>[^\]]+)\]\s+"(?P<req>[^"]*)"\s+(?P<status>\d{3})\s+(?P<bytes>\d+|-)`,
)

type LogLine struct {
	IP     string
	Time   time.Time
	Method string
	Path   string
	Status int
	Bytes  int64
}

func parseLine(line string) (LogLine, bool) {
	m := logRe.FindStringSubmatch(line)
	if m == nil {
		return LogLine{}, false
	}
	// Use named capture groups via SubexpNames for clarity.
	names := logRe.SubexpNames()
	idx := make(map[string]string, len(names))
	for i, name := range names {
		if name != "" {
			idx[name] = m[i]
		}
	}

	status, _ := strconv.Atoi(idx["status"])
	var bytes int64
	if idx["bytes"] != "-" {
		bytes, _ = strconv.ParseInt(idx["bytes"], 10, 64)
	}
	t, err := time.Parse("02/Jan/2006:15:04:05 -0700", idx["time"])
	if err != nil {
		return LogLine{}, false
	}
	reqParts := strings.Fields(idx["req"])
	method, path := "-", "-"
	if len(reqParts) >= 2 {
		method = reqParts[0]
		path = reqParts[1]
	}
	return LogLine{IP: idx["ip"], Time: t.Local(), Method: method, Path: path, Status: status, Bytes: bytes}, true
}

type StubStatus struct {
	ActiveConnections int64
	Accepts           int64
	Handled           int64
	Requests          int64
	Reading           int64
	Writing           int64
	Waiting           int64
}

var stubRe = regexp.MustCompile(
	`Active connections:\s+(\d+)[\s\S]+?(\d+)\s+(\d+)\s+(\d+)\s+Reading:\s+(\d+)\s+Writing:\s+(\d+)\s+Waiting:\s+(\d+)`,
)
var stubHTTPClient = &http.Client{Timeout: 2 * time.Second}

func fetchStubStatus(url string) (StubStatus, error) {
	resp, err := stubHTTPClient.Get(url)
	if err != nil {
		return StubStatus{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return StubStatus{}, err
	}
	m := stubRe.FindSubmatch(body)
	if m == nil {
		return StubStatus{}, fmt.Errorf("stub_status: unexpected format")
	}
	atoi := func(b []byte) int64 { v, _ := strconv.ParseInt(string(b), 10, 64); return v }
	return StubStatus{
		ActiveConnections: atoi(m[1]), Accepts: atoi(m[2]), Handled: atoi(m[3]),
		Requests: atoi(m[4]), Reading: atoi(m[5]), Writing: atoi(m[6]), Waiting: atoi(m[7]),
	}, nil
}

type Window struct {
	mu        sync.Mutex
	lines     []LogLine
	bytesSent int64
}

func (w *Window) add(l LogLine) {
	w.mu.Lock()
	w.lines = append(w.lines, l)
	w.bytesSent += l.Bytes
	w.mu.Unlock()
}

func (w *Window) drain() ([]LogLine, int64) {
	w.mu.Lock()
	out := w.lines
	w.lines = nil
	bytes := w.bytesSent
	w.bytesSent = 0
	w.mu.Unlock()
	return out, bytes
}

type EndpointStat struct {
	Path  string `json:"path"`
	Count int    `json:"count"`
}
type LogEntry struct {
	Time   string `json:"time"`
	IP     string `json:"ip"`
	Method string `json:"method"`
	Path   string `json:"path"`
	Status int    `json:"status"`
	Bytes  int64  `json:"bytes"`
}
type Snapshot struct {
	Timestamp   string         `json:"ts"`
	RPS         float64        `json:"rps"`
	ActiveConns int64          `json:"active_conns"`
	Reading     int64          `json:"reading"`
	Writing     int64          `json:"writing"`
	Waiting     int64          `json:"waiting"`
	BytesOut    int64          `json:"bytes_out_kb"`
	Status2xx   int            `json:"s2xx"`
	Status3xx   int            `json:"s3xx"`
	Status4xx   int            `json:"s4xx"`
	Status5xx   int            `json:"s5xx"`
	ErrRate     float64        `json:"err_rate"`
	TopPaths    []EndpointStat `json:"top_paths"`
	RecentLogs  []LogEntry     `json:"recent_logs"`
}

// safeConn wraps a WebSocket connection with a mutex for concurrent writes,
// and a sync.Once to ensure Close is only called once regardless of which
// goroutine (ping or read) triggers the teardown.
type safeConn struct {
	conn      *websocket.Conn
	mu        sync.Mutex
	closeOnce sync.Once
}

func (sc *safeConn) close() {
	sc.closeOnce.Do(func() { sc.conn.Close() })
}

type Hub struct {
	mu      sync.RWMutex
	clients map[*safeConn]struct{}
}

func newHub() *Hub { return &Hub{clients: make(map[*safeConn]struct{})} }
func (h *Hub) register(c *websocket.Conn) *safeConn {
	sc := &safeConn{conn: c}
	h.mu.Lock()
	h.clients[sc] = struct{}{}
	h.mu.Unlock()
	return sc
}
func (h *Hub) unregister(sc *safeConn) {
	h.mu.Lock()
	delete(h.clients, sc)
	h.mu.Unlock()
	sc.close()
}
func (h *Hub) broadcast(msg []byte) {
	h.mu.RLock()
	var dead []*safeConn
	for sc := range h.clients {
		sc.mu.Lock()
		if err := sc.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
			dead = append(dead, sc)
		}
		sc.mu.Unlock()
	}
	h.mu.RUnlock()
	for _, sc := range dead {
		h.unregister(sc)
	}
}

// tailLog tails the nginx access log, re-opening on rotation or missing file.
// It respects ctx cancellation for clean shutdown.
func tailLog(ctx context.Context, path string, out chan<- LogLine) {
	var f *os.File
	var offset int64
	open := func() {
		if f != nil {
			f.Close()
		}
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			var err error
			f, err = os.Open(path)
			if err == nil {
				if *flagFromStart {
					offset = 0
				} else {
					f.Seek(0, io.SeekEnd)
					offset, _ = f.Seek(0, io.SeekCurrent)
				}
				log.Printf("tail: opened %s at offset %d", path, offset)
				return
			}
			log.Printf("tail: waiting for %s (%v)", path, err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
	}
	open()
	reader := bufio.NewReader(f)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			if f != nil {
				f.Close()
			}
			return
		case <-ticker.C:
			if info, err := os.Stat(path); err == nil {
				if info.Size() < offset {
					log.Println("tail: file rotated, re-opening")
					open()
					reader = bufio.NewReader(f)
					continue
				}
			}
			for {
				line, err := reader.ReadString('\n')
				if err != nil {
					if err != io.EOF {
						log.Printf("tail: read error: %v", err)
					}
					break
				}
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				if l, ok := parseLine(line); ok {
					out <- l
				}
			}
			offset, _ = f.Seek(0, io.SeekCurrent)
		}
	}
}

type StubFetcher struct {
	mu       sync.RWMutex
	last     StubStatus
	url      string
	interval time.Duration
}

func newStubFetcher(url string, interval time.Duration) *StubFetcher {
	sf := &StubFetcher{url: url, interval: interval}
	go sf.run()
	return sf
}
func (sf *StubFetcher) run() {
	ticker := time.NewTicker(sf.interval)
	defer ticker.Stop()
	for range ticker.C {
		if s, err := fetchStubStatus(sf.url); err == nil {
			sf.mu.Lock()
			sf.last = s
			sf.mu.Unlock()
		}
	}
}
func (sf *StubFetcher) Get() StubStatus {
	sf.mu.RLock()
	defer sf.mu.RUnlock()
	return sf.last
}

// prunePaths removes the lowest-count entries from pathCounts when it exceeds maxPaths.
func prunePaths(pathCounts map[string]int, maxPaths int) {
	if len(pathCounts) <= maxPaths {
		return
	}
	type kv struct{ k string; v int }
	all := make([]kv, 0, len(pathCounts))
	for k, v := range pathCounts {
		all = append(all, kv{k, v})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].v < all[j].v })
	// Remove the bottom half.
	for i := 0; i < len(all)/2; i++ {
		delete(pathCounts, all[i].k)
	}
	log.Printf("aggregator: pruned pathCounts from %d to %d entries", len(all), len(pathCounts))
}

func aggregator(lines <-chan LogLine, hub *Hub, interval time.Duration, stubFetcher *StubFetcher) {
	window := &Window{}
	pathCounts := make(map[string]int)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Fixed-size ring buffer for recent log entries (avoids unbounded backing array growth).
	const maxRecent = 50
	recentBuf := make([]LogEntry, maxRecent)
	recentHead := 0
	recentCount := 0

	hub.broadcast([]byte(`{"ts":"--:--:--","rps":0,"active_conns":0,"reading":0,"writing":0,"waiting":0,"bytes_out_kb":0,"s2xx":0,"s3xx":0,"s4xx":0,"s5xx":0,"err_rate":0,"top_paths":[],"recent_logs":[]}`))

	for {
		select {
		case l := <-lines:
			window.add(l)
			pathCounts[l.Path]++
			// Write into ring buffer (paths are stored raw; JS escapes on render).
			recentBuf[recentHead] = LogEntry{
				Time:   l.Time.Format("15:04:05"),
				IP:     l.IP,
				Method: l.Method,
				Path:   l.Path,
				Status: l.Status,
				Bytes:  l.Bytes,
			}
			recentHead = (recentHead + 1) % maxRecent
			if recentCount < maxRecent {
				recentCount++
			}
			// Prune path map if it grows too large.
			if len(pathCounts) > *flagMaxPaths {
				prunePaths(pathCounts, *flagMaxPaths)
			}

		case <-ticker.C:
			batch, bytesOut := window.drain()
			secs := interval.Seconds()
			var s2, s3, s4, s5 int
			for _, l := range batch {
				switch l.Status / 100 {
				case 2:
					s2++
				case 3:
					s3++
				case 4:
					s4++
				case 5:
					s5++
				}
			}
			stub := stubFetcher.Get()

			type kv struct {
				k string
				v int
			}
			sorted := make([]kv, 0, len(pathCounts))
			for k, v := range pathCounts {
				sorted = append(sorted, kv{k, v})
			}
			sort.Slice(sorted, func(i, j int) bool { return sorted[i].v > sorted[j].v })

			top := make([]EndpointStat, 0, min(50, len(sorted)))
			for i, item := range sorted {
				if i >= 50 {
					break
				}
				// No Go-side HTML escaping — JS handles it on render.
				top = append(top, EndpointStat{Path: item.k, Count: item.v})
			}

			totalReqs := s2 + s3 + s4 + s5
			errRate := 0.0
			if totalReqs > 0 {
				errRate = float64(s4+s5) / float64(totalReqs) * 100
			}

			// Collect ring buffer contents in reverse-chronological order.
			recentSnap := make([]LogEntry, recentCount)
			for i := 0; i < recentCount; i++ {
				// Most recent first: walk backwards from (recentHead-1).
				idx := (recentHead - 1 - i + maxRecent) % maxRecent
				recentSnap[i] = recentBuf[idx]
			}

			snap := Snapshot{
				Timestamp:   time.Now().Format("15:04:05"),
				RPS:         float64(len(batch)) / secs,
				ActiveConns: stub.ActiveConnections,
				Reading:     stub.Reading,
				Writing:     stub.Writing,
				Waiting:     stub.Waiting,
				BytesOut:    bytesOut / 1024,
				Status2xx:   s2,
				Status3xx:   s3,
				Status4xx:   s4,
				Status5xx:   s5,
				ErrRate:     errRate,
				TopPaths:    top,
				RecentLogs:  recentSnap,
			}
			msg, _ := json.Marshal(snap)
			hub.broadcast(msg)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

var upgrader = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

func wsHandler(hub *Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Println("ws upgrade:", err)
			return
		}
		sc := hub.register(conn)

		// Shared done channel so ping and read goroutines coordinate teardown.
		done := make(chan struct{})

		// Ping goroutine.
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-done:
					return
				case <-ticker.C:
					sc.mu.Lock()
					err := sc.conn.WriteMessage(websocket.PingMessage, nil)
					sc.mu.Unlock()
					if err != nil {
						hub.unregister(sc)
						// Signal read goroutine if it hasn't already exited.
						select {
						case <-done:
						default:
							close(done)
						}
						return
					}
				}
			}
		}()

		conn.SetPongHandler(func(string) error {
			conn.SetReadDeadline(time.Now().Add(60 * time.Second))
			return nil
		})
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))

		// Read loop — blocks until client disconnects.
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				hub.unregister(sc)
				select {
				case <-done:
				default:
					close(done)
				}
				return
			}
		}
	}
}

func corsMiddleware(next http.Handler, origin string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func main() {
	flag.Parse()
	log.Printf("nginx-dstat starting — log=%s status=%s addr=%s", *flagLog, *flagStatus, *flagAddr)
	hub := newHub()
	lines := make(chan LogLine, 4096)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tailLog(ctx, *flagLog, lines)
	stubFetcher := newStubFetcher(*flagStatus, *flagStatusInterval)
	go aggregator(lines, hub, *flagInterval, stubFetcher)
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", wsHandler(hub))
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, dashboardHTML)
	})
	srv := &http.Server{Addr: *flagAddr, Handler: corsMiddleware(mux, *flagCORS)}
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		log.Println("shutting down...")
		cancel()
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutCancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			log.Printf("shutdown error: %v", err)
		}
	}()
	log.Printf("listening on %s", *flagAddr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

const dashboardHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>nginx dstat</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link href="https://fonts.googleapis.com/css2?family=JetBrains+Mono:wght@400;500;600&family=Inter:wght@400;500;600;700&display=swap" rel="stylesheet">
<script src="https://cdnjs.cloudflare.com/ajax/libs/Chart.js/4.4.1/chart.umd.js"></script>
<style>
:root{--bg:#0b0f19;--surface:rgba(22,27,40,0.65);--border:rgba(255,255,255,0.06);--text:#e2e8f0;--muted:#94a3b8;--accent:#3b82f6;--green:#22c55e;--amber:#f59e0b;--red:#ef4444;--purple:#8b5cf6;--cyan:#06b6d4;}
*{box-sizing:border-box;margin:0;padding:0;}
body{background:var(--bg);color:var(--text);font-family:'Inter',sans-serif;padding:1.5rem;min-height:100vh;background-image:radial-gradient(circle at 10% 0%,rgba(59,130,246,0.08) 0%,transparent 40%),radial-gradient(circle at 90% 100%,rgba(139,92,246,0.06) 0%,transparent 40%);}
.header{display:flex;align-items:center;justify-content:space-between;margin-bottom:1.5rem;padding-bottom:1rem;border-bottom:1px solid var(--border);}
.dot{width:9px;height:9px;border-radius:50%;background:var(--green);margin-right:10px;box-shadow:0 0 8px rgba(34,197,94,0.4);animation:pulse 2s ease-in-out infinite;}
.dot.off{background:var(--red);box-shadow:0 0 8px rgba(239,68,68,0.4);animation:none;}
@keyframes pulse{0%,100%{opacity:1;transform:scale(1);}50%{opacity:0.5;transform:scale(0.85);}}
.title{font-size:18px;font-weight:700;letter-spacing:-0.4px;}
.sub{font-size:12px;color:var(--muted);font-family:'JetBrains Mono',monospace;margin-top:3px;}
.clock{font-family:'JetBrains Mono',monospace;font-size:13px;color:var(--muted);text-align:right;background:var(--surface);padding:6px 10px;border-radius:8px;border:1px solid var(--border);}
.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(140px,1fr));gap:12px;margin-bottom:1.25rem;}
.card{background:var(--surface);backdrop-filter:blur(12px);border:1px solid var(--border);border-radius:12px;padding:14px 16px;transition:all 0.2s ease;}
.card:hover{border-color:rgba(255,255,255,0.12);transform:translateY(-1px);}
.card.warn{border-left:3px solid var(--amber);}
.card.crit{border-left:3px solid var(--red);}
.clabel{font-size:10px;color:var(--muted);font-family:'JetBrains Mono',monospace;text-transform:uppercase;letter-spacing:0.6px;margin-bottom:6px;}
.cval{font-size:24px;font-weight:700;color:#f8fafc;font-family:'JetBrains Mono',monospace;line-height:1;}
.cunit{font-size:11px;color:var(--muted);margin-top:4px;font-family:'JetBrains Mono',monospace;}
.cdelta{font-size:11px;margin-top:4px;font-family:'JetBrains Mono',monospace;font-weight:500;}
.up{color:var(--green);}.dn{color:var(--red);}.ne{color:var(--muted);}
.charts{display:grid;grid-template-columns:1fr 1fr;gap:12px;margin-bottom:12px;}
@media(max-width:820px){.charts{grid-template-columns:1fr;}}
.chart-box{background:var(--surface);backdrop-filter:blur(12px);border:1px solid var(--border);border-radius:12px;padding:16px;}
.ctitle{font-size:11px;color:var(--muted);font-family:'JetBrains Mono',monospace;text-transform:uppercase;letter-spacing:0.5px;margin-bottom:12px;display:flex;align-items:center;gap:6px;}
.ctitle::before{content:'';display:block;width:6px;height:6px;border-radius:50%;background:var(--accent);}
.log-box{background:var(--surface);backdrop-filter:blur(12px);border:1px solid var(--border);border-radius:12px;padding:16px;margin-top:12px;}
.log-entries{display:flex;flex-direction:column;gap:3px;max-height:240px;overflow-y:auto;scrollbar-width:thin;scrollbar-color:rgba(255,255,255,0.1) transparent;}
.log-entries::-webkit-scrollbar{width:6px;}
.log-entries::-webkit-scrollbar-thumb{background:rgba(255,255,255,0.15);border-radius:3px;}
.le{display:flex;align-items:center;gap:10px;font-size:11px;font-family:'JetBrains Mono',monospace;padding:5px 8px;border-radius:6px;transition:background 0.15s;}
.le:hover{background:rgba(255,255,255,0.04);}
.lt{color:var(--muted);min-width:58px;}.lip{color:var(--muted);min-width:100px;}
.lm{color:var(--purple);min-width:38px;font-weight:500;}
.badge{padding:2px 6px;border-radius:4px;font-size:10px;font-weight:600;min-width:32px;text-align:center;}
.s2{background:rgba(34,197,94,0.15);color:var(--green);}
.s3{background:rgba(59,130,246,0.15);color:var(--accent);}
.s4{background:rgba(245,158,11,0.15);color:var(--amber);}
.s5{background:rgba(239,68,68,0.15);color:var(--red);}
.lp{color:var(--text);flex:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;}
.lsz{color:var(--muted);min-width:54px;text-align:right;}
.btm{display:grid;grid-template-columns:1fr 1fr;gap:12px;margin-top:12px;}
@media(max-width:820px){.btm{grid-template-columns:1fr;}}
#m-waiting{color:var(--purple);}
.ep-layout{display:flex;gap:16px;align-items:center;}
@media(max-width:900px){.ep-layout{flex-direction:column;align-items:stretch;}}
.ep-chart{flex:1;min-width:0;position:relative;height:180px;}
.ep-list-box{width:240px;flex-shrink:0;}
@media(max-width:900px){.ep-list-box{width:100%;}}
.ep-list-title{font-size:10px;color:var(--muted);font-family:'JetBrains Mono',monospace;text-transform:uppercase;letter-spacing:0.5px;margin-bottom:8px;}
.ep-list{max-height:150px;overflow-y:auto;scrollbar-width:thin;scrollbar-color:rgba(255,255,255,0.1) transparent;}
.ep-list::-webkit-scrollbar{width:4px;}
.ep-list::-webkit-scrollbar-thumb{background:rgba(255,255,255,0.15);border-radius:2px;}
.ep-row{display:flex;justify-content:space-between;align-items:center;padding:5px 0;border-bottom:1px solid var(--border);font-size:11px;font-family:'JetBrains Mono',monospace;}
.ep-row:last-child{border-bottom:none;}
.ep-rank{color:var(--muted);min-width:26px;font-weight:500;}
.ep-path{flex:1;color:var(--text);overflow:hidden;text-overflow:ellipsis;white-space:nowrap;margin:0 8px;}
.ep-count{color:var(--accent);font-weight:600;min-width:45px;text-align:right;}
</style>
</head>
<body>
<div class="header">
  <div style="display:flex;align-items:center;"><div class="dot" id="dot"></div><div><div class="title">nginx / live traffic</div><div class="sub" id="sub">connecting…</div></div></div>
  <div class="clock" id="clock">--:--:--</div>
</div>
<div class="grid">
  <div class="card" id="card-rps"><div class="clabel">req/s</div><div class="cval" id="m-rps">—</div><div class="cunit">requests/sec</div><div class="cdelta ne" id="d-rps">—</div></div>
  <div class="card" id="card-conn"><div class="clabel">active</div><div class="cval" id="m-conn">—</div><div class="cunit">connections</div><div class="cdelta ne" id="d-conn">—</div></div>
  <div class="card"><div class="clabel">reading</div><div class="cval" id="m-reading">—</div><div class="cunit">nginx reading</div><div class="cdelta ne">&nbsp;</div></div>
  <div class="card"><div class="clabel">writing</div><div class="cval" id="m-writing">—</div><div class="cunit">nginx writing</div><div class="cdelta ne">&nbsp;</div></div>
  <div class="card"><div class="clabel">waiting</div><div class="cval" id="m-waiting">—</div><div class="cunit">keep-alive</div><div class="cdelta ne">&nbsp;</div></div>
  <div class="card" id="card-err"><div class="clabel">errors</div><div class="cval" id="m-err">—</div><div class="cunit" id="err-unit">4xx+5xx</div><div class="cdelta ne" id="d-err">—</div></div>
  <div class="card" id="card-bw"><div class="clabel">bandwidth</div><div class="cval" id="m-bout">—</div><div class="cunit">KB/s out</div><div class="cdelta ne" id="d-bout">—</div></div>
</div>
<div class="charts">
  <div class="chart-box"><div class="ctitle">requests / sec</div><div style="position:relative;height:130px"><canvas id="c-rps"></canvas></div></div>
  <div class="chart-box">
    <div class="ctitle">response codes</div>
    <div style="display:flex;gap:12px;margin-bottom:8px;font-size:11px;color:var(--muted);">
      <span style="display:flex;align-items:center;gap:4px;"><span style="width:8px;height:8px;border-radius:2px;background:var(--green);display:inline-block;"></span>2xx</span>
      <span style="display:flex;align-items:center;gap:4px;"><span style="width:8px;height:8px;border-radius:2px;background:var(--accent);display:inline-block;"></span>3xx</span>
      <span style="display:flex;align-items:center;gap:4px;"><span style="width:8px;height:8px;border-radius:2px;background:var(--amber);display:inline-block;"></span>4xx</span>
      <span style="display:flex;align-items:center;gap:4px;"><span style="width:8px;height:8px;border-radius:2px;background:var(--red);display:inline-block;"></span>5xx</span>
    </div>
    <div style="position:relative;height:100px"><canvas id="c-codes"></canvas></div>
  </div>
</div>
<div class="btm">
  <div class="chart-box ep-container">
    <div class="ctitle">all endpoints (cumulative)</div>
    <div class="ep-layout">
      <div class="ep-chart"><canvas id="c-endpoints"></canvas></div>
      <div class="ep-list-box">
        <div class="ep-list-title">Top 10 Paths</div>
        <div class="ep-list" id="ep-top10"></div>
      </div>
    </div>
  </div>
  <div class="chart-box"><div class="ctitle">bandwidth (KB/s out)</div><div style="position:relative;height:180px"><canvas id="c-bw"></canvas></div></div>
</div>
<div class="log-box">
  <div class="ctitle">access log — live tail</div>
  <div class="log-entries" id="log-list"></div>
</div>
<script>
(function () {
'use strict';

/* ── rolling window buffers ───────────────────────────────────────────── */
var N = 40;
var labels = [], rpsData = [], bwData = [], c2Data = [], c3Data = [], c4Data = [], c5Data = [];
for (var i = 0; i < N; i++) {
  labels.push(''); rpsData.push(0); bwData.push(0);
  c2Data.push(0); c3Data.push(0); c4Data.push(0); c5Data.push(0);
}

/* ── shared chart options ─────────────────────────────────────────────── */
var chartOpts = {
  responsive: true, maintainAspectRatio: false,
  animation: { duration: 180 },
  plugins: {
    legend: { display: false },
    tooltip: {
      backgroundColor: 'rgba(11,15,25,0.9)', titleColor: '#e2e8f0',
      bodyColor: '#94a3b8', borderColor: 'rgba(255,255,255,0.1)', borderWidth: 1,
      padding: 10, cornerRadius: 8, mode: 'index', intersect: false
    }
  },
  scales: {
    x: { display: false },
    y: {
      display: true, grid: { color: 'rgba(255,255,255,0.04)' },
      ticks: { font: { size: 10, family: 'JetBrains Mono' }, color: 'rgba(255,255,255,0.3)', maxTicksLimit: 4 }
    }
  }
};

/* ── FIX: correct Chart.js constructor — data key present, dataset uses data: array ── */
var cRps = new Chart(document.getElementById('c-rps'), {
  type: 'line',
  data: {
    labels: labels,
    datasets: [{
      data: rpsData, borderColor: '#3b82f6', borderWidth: 2,
      fill: true, backgroundColor: 'rgba(59,130,246,0.08)', tension: 0.4, pointRadius: 0
    }]
  },
  options: chartOpts
});

var cBw = new Chart(document.getElementById('c-bw'), {
  type: 'line',
  data: {
    labels: labels,
    datasets: [{
      data: bwData, borderColor: '#22c55e', borderWidth: 2,
      fill: true, backgroundColor: 'rgba(34,197,94,0.08)', tension: 0.4, pointRadius: 0
    }]
  },
  options: chartOpts
});

/* ── FIX: all four datasets now have data: key ───────────────────────── */
var cCodes = new Chart(document.getElementById('c-codes'), {
  type: 'line',
  data: {
    labels: labels,
    datasets: [
      { data: c2Data, borderColor: '#22c55e', borderWidth: 1.5, fill: false, tension: 0.4, pointRadius: 0 },
      { data: c3Data, borderColor: '#3b82f6', borderWidth: 1.5, fill: false, tension: 0.4, pointRadius: 0 },
      { data: c4Data, borderColor: '#f59e0b', borderWidth: 1.5, fill: false, tension: 0.4, pointRadius: 0 },
      { data: c5Data, borderColor: '#ef4444', borderWidth: 1.5, fill: false, tension: 0.4, pointRadius: 0 }
    ]
  },
  options: chartOpts
});

/* ── center-text plugin for doughnut ─────────────────────────────────── */
var centerTextPlugin = {
  id: 'centerText',
  afterDraw: function (chart) {
    var opts = chart.options.plugins.centerText;
    if (!opts || !opts.display) return;
    var ctx = chart.ctx;
    ctx.save();
    ctx.textAlign = 'center'; ctx.textBaseline = 'middle';
    var cx = chart.width / 2, cy = chart.height / 2;
    ctx.font = "bold 15px 'JetBrains Mono'"; ctx.fillStyle = '#e2e8f0';
    ctx.fillText(opts.text, cx, cy - 10);
    ctx.font = "11px 'JetBrains Mono'"; ctx.fillStyle = '#94a3b8';
    ctx.fillText(opts.subtext, cx, cy + 10);
    ctx.restore();
  }
};
Chart.register(centerTextPlugin);

var truncate = function (str, len) { return str && str.length > len ? str.slice(0, len) + '…' : str; };

var generateColors = function (count) {
  var colors = [];
  for (var i = 0; i < count; i++) {
    var hue = (i * 137.508) % 360;
    colors.push('hsl(' + hue.toFixed(1) + ', 65%, 55%)');
  }
  return colors;
};

/* ── FIX: doughnut dataset has data: [] key ──────────────────────────── */
var cEndpoints = new Chart(document.getElementById('c-endpoints'), {
  type: 'doughnut',
  data: {
    labels: [],
    datasets: [{ data: [], backgroundColor: [], borderWidth: 0, hoverOffset: 10 }]
  },
  options: {
    responsive: true, maintainAspectRatio: false, cutout: '65%',
    plugins: {
      centerText: { display: true, text: 'Waiting', subtext: 'for traffic' },
      legend: { display: false },
      tooltip: {
        backgroundColor: 'rgba(11,15,25,0.9)', titleColor: '#e2e8f0',
        bodyColor: '#94a3b8', borderColor: 'rgba(255,255,255,0.1)', borderWidth: 1,
        padding: 10, cornerRadius: 8,
        callbacks: {
          label: function (ctx) {
            var total = ctx.dataset.data.reduce(function (a, b) { return a + b; }, 0);
            var pct = total > 0 ? ((ctx.parsed / total) * 100).toFixed(1) : 0;
            return ' ' + ctx.label + ': ' + ctx.parsed + ' reqs (' + pct + '%)';
          }
        }
      }
    }
  }
});

/* ── state ────────────────────────────────────────────────────────────── */
var pRps = 0, pConn = 0, pBout = 0, pErr = 0;
var lastLogKeys = [];

/* ── helpers ──────────────────────────────────────────────────────────── */
function delta(now, prev, unit) {
  var d = now - prev;
  if (Math.abs(d) < 0.5) return '<span class="ne">—</span>';
  return d > 0
    ? '<span class="up">▲ ' + Math.abs(d).toFixed(1) + ' ' + unit + '</span>'
    : '<span class="dn">▼ ' + Math.abs(d).toFixed(1) + ' ' + unit + '</span>';
}

function updateCardThresholds(s) {
  var r = document.getElementById('card-rps');
  var e = document.getElementById('card-err');
  var c = document.getElementById('card-conn');
  r.classList.remove('warn', 'crit');
  if (s.rps > 100) r.classList.add('crit'); else if (s.rps > 50) r.classList.add('warn');
  e.classList.remove('warn', 'crit');
  if (s.err_rate > 5) e.classList.add('crit'); else if (s.err_rate > 1) e.classList.add('warn');
  c.classList.remove('warn', 'crit');
  if (s.active_conns > 1000) c.classList.add('crit'); else if (s.active_conns > 500) c.classList.add('warn');
}

/* ── FIX: esc() is used for all HTML insertion — no double-escaping from Go ── */
function esc(str) {
  return String(str)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');
}

/* ── main update ──────────────────────────────────────────────────────── */
function update(s) {
  document.getElementById('clock').textContent = s.ts;
  document.getElementById('sub').textContent = 'ws connected — nginx live';
  document.getElementById('dot').classList.remove('off');

  var rpsVal = +s.rps.toFixed(1);
  document.getElementById('m-rps').textContent = rpsVal;
  document.getElementById('m-conn').textContent = s.active_conns;
  document.getElementById('m-reading').textContent = s.reading;
  document.getElementById('m-writing').textContent = s.writing;
  document.getElementById('m-waiting').textContent = s.waiting;
  document.getElementById('m-bout').textContent = s.bytes_out_kb;

  var errEl = document.getElementById('m-err');
  errEl.textContent = s.err_rate.toFixed(1) + '%';
  errEl.className = s.err_rate > 5 ? 'cval crit' : s.err_rate > 1 ? 'cval warn' : 'cval';
  document.getElementById('err-unit').textContent = 'error rate';

  document.getElementById('d-rps').innerHTML = delta(rpsVal, pRps, 'r/s');
  document.getElementById('d-conn').innerHTML = delta(s.active_conns, pConn, '');
  document.getElementById('d-bout').innerHTML = delta(s.bytes_out_kb, pBout, 'KB/s');
  document.getElementById('d-err').innerHTML = delta(s.err_rate, pErr, '%');

  pRps = rpsVal; pConn = s.active_conns; pBout = s.bytes_out_kb; pErr = s.err_rate;
  updateCardThresholds(s);

  /* rolling window shift */
  labels.shift(); rpsData.shift(); bwData.shift();
  c2Data.shift(); c3Data.shift(); c4Data.shift(); c5Data.shift();
  labels.push(s.ts.slice(3));
  rpsData.push(rpsVal); bwData.push(s.bytes_out_kb);
  c2Data.push(s.s2xx); c3Data.push(s.s3xx); c4Data.push(s.s4xx); c5Data.push(s.s5xx);
  cRps.update('none'); cBw.update('none'); cCodes.update('none');

  /* endpoint doughnut + top-10 list */
  if (s.top_paths && s.top_paths.length > 0) {
    var paths = s.top_paths;
    cEndpoints.data.labels = paths.map(function (p) { return p.path; });
    cEndpoints.data.datasets[0].data = paths.map(function (p) { return p.count; });
    cEndpoints.data.datasets[0].backgroundColor = generateColors(paths.length);
    cEndpoints.options.plugins.centerText.text = truncate(paths[0].path, 14);
    cEndpoints.options.plugins.centerText.subtext = paths[0].count + ' reqs';
    cEndpoints.update('none');

    var top10 = paths.slice(0, 10);
    var listHtml = top10.map(function (p, i) {
      return '<div class="ep-row">'
        + '<span class="ep-rank">#' + (i + 1) + '</span>'
        + '<span class="ep-path" title="' + esc(p.path) + '">' + esc(truncate(p.path, 30)) + '</span>'
        + '<span class="ep-count">' + p.count + '</span>'
        + '</div>';
    }).join('');
    document.getElementById('ep-top10').innerHTML = listHtml;
  }

  /* live log tail */
  if (s.recent_logs && s.recent_logs.length) {
    var logList = document.getElementById('log-list');
    var newLogs = s.recent_logs.slice(0, 40);

    /* FIX: dedup comparison without ES2015 Set spread */
    var newKey = newLogs.map(function (l) { return l.time + l.ip + l.method + l.path + l.status; }).join('|');
    var oldKey = lastLogKeys.join('|');
    if (newKey !== oldKey) {
      lastLogKeys = newKey.split('|');
      logList.innerHTML = newLogs.map(function (l) {
        var sClass = l.status < 300 ? '2' : l.status < 400 ? '3' : l.status < 500 ? '4' : '5';
        return '<div class="le">'
          + '<span class="lt">'  + esc(l.time)   + '</span>'
          + '<span class="lip">' + esc(l.ip)     + '</span>'
          + '<span class="lm">'  + esc(l.method) + '</span>'
          + '<span class="badge s' + sClass + '">' + l.status + '</span>'
          + '<span class="lp">'  + esc(l.path)   + '</span>'
          + '<span class="lsz">' + l.bytes       + 'B</span>'
          + '</div>';
      }).join('');
    }
  }
}

/* ── FIX: WS URL built client-side — no server-injected r.Host ───────── */
function wsUrl() {
  var proto = location.protocol === 'https:' ? 'wss' : 'ws';
  return proto + '://' + location.host + '/ws';
}

function connect() {
  var ws = new WebSocket(wsUrl());
  ws.onopen = function () { console.log('ws connected'); };
  ws.onmessage = function (e) {
    try { update(JSON.parse(e.data)); } catch (err) { console.error('parse err:', err); }
  };
  ws.onclose = function () {
    document.getElementById('dot').classList.add('off');
    document.getElementById('sub').textContent = 'reconnecting…';
    setTimeout(connect, 3000);
  };
  ws.onerror = function (err) { console.error('ws error:', err); };
}

connect();
setInterval(function () {
  var n = new Date();
  document.getElementById('clock').textContent = n.toTimeString().slice(0, 8);
}, 1000);

})();
</script>
</body>
</html>`
