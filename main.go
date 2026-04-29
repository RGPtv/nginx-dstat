package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// ──────────────────────────────────────────────
// Config
// ──────────────────────────────────────────────

var (
	flagAddr     = flag.String("addr", ":8765", "listen address for the metrics server")
	flagLog      = flag.String("log", "/var/log/nginx/access.log", "nginx access log path")
	flagStatus   = flag.String("status", "http://127.0.0.1/nginx_status", "nginx stub_status URL")
	flagInterval = flag.Duration("interval", 1500*time.Millisecond, "push interval to WebSocket clients")
	flagCORS     = flag.String("cors", "*", "CORS origin for the dashboard")
)

// ──────────────────────────────────────────────
// Nginx combined log format parser
// $remote_addr - $remote_user [$time_local] "$request" $status $body_bytes_sent "$http_referer" "$http_user_agent"
// ──────────────────────────────────────────────

var logRe = regexp.MustCompile(
	`^(\S+)\s+-\s+\S+\s+\[([^\]]+)\]\s+"(\S+)\s+(\S+)\s+\S+"\s+(\d{3})\s+(\d+)`,
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
	status, _ := strconv.Atoi(m[5])
	bytes, _ := strconv.ParseInt(m[6], 10, 64)
	t, _ := time.Parse("02/Jan/2006:15:04:05 -0700", m[2])
	return LogLine{IP: m[1], Time: t, Method: m[3], Path: m[4], Status: status, Bytes: bytes}, true
}

// ──────────────────────────────────────────────
// Nginx stub_status parser
// Active connections: 8
// server accepts handled requests
//  123 123 456
// Reading: 0 Writing: 1 Waiting: 7
// ──────────────────────────────────────────────

type StubStatus struct {
	ActiveConnections int64
	Accepts           int64
	Handled           int64
	Requests          int64
	Reading           int64
	Writing           int64
	Waiting           int64
}

var stubRe = regexp.MustCompile(`Active connections:\s+(\d+)[\s\S]+?(\d+)\s+(\d+)\s+(\d+)\s+Reading:\s+(\d+)\s+Writing:\s+(\d+)\s+Waiting:\s+(\d+)`)

func fetchStubStatus(url string) (StubStatus, error) {
	resp, err := http.Get(url) //nolint:gosec
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
		ActiveConnections: atoi(m[1]),
		Accepts:           atoi(m[2]),
		Handled:           atoi(m[3]),
		Requests:          atoi(m[4]),
		Reading:           atoi(m[5]),
		Writing:           atoi(m[6]),
		Waiting:           atoi(m[7]),
	}, nil
}

// ──────────────────────────────────────────────
// Metrics accumulator (1-second window)
// ──────────────────────────────────────────────

type Window struct {
	mu       sync.Mutex
	lines    []LogLine
	bytesSent int64
}

func (w *Window) add(l LogLine) {
	w.mu.Lock()
	w.lines = append(w.lines, l)
	atomic.AddInt64(&w.bytesSent, l.Bytes)
	w.mu.Unlock()
}

func (w *Window) drain() []LogLine {
	w.mu.Lock()
	out := w.lines
	w.lines = nil
	atomic.StoreInt64(&w.bytesSent, 0)
	w.mu.Unlock()
	return out
}

// ──────────────────────────────────────────────
// Aggregated snapshot pushed to clients
// ──────────────────────────────────────────────

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
	BytesIn     int64          `json:"bytes_in_kb"`
	BytesOut    int64          `json:"bytes_out_kb"`
	Status2xx   int            `json:"s2xx"`
	Status3xx   int            `json:"s3xx"`
	Status4xx   int            `json:"s4xx"`
	Status5xx   int            `json:"s5xx"`
	TopPaths    []EndpointStat `json:"top_paths"`
	RecentLogs  []LogEntry     `json:"recent_logs"`
}

// ──────────────────────────────────────────────
// Hub – fan-out to all WebSocket clients
// ──────────────────────────────────────────────

type Hub struct {
	mu      sync.RWMutex
	clients map[*websocket.Conn]struct{}
}

func newHub() *Hub { return &Hub{clients: make(map[*websocket.Conn]struct{})} }

func (h *Hub) register(c *websocket.Conn) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
}

func (h *Hub) unregister(c *websocket.Conn) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
	c.Close()
}

func (h *Hub) broadcast(msg []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		if err := c.WriteMessage(websocket.TextMessage, msg); err != nil {
			go h.unregister(c)
		}
	}
}

// ──────────────────────────────────────────────
// Log tailer (survives logrotate via re-open)
// ──────────────────────────────────────────────

func tailLog(path string, out chan<- LogLine) {
	var (
		f    *os.File
		err  error
		pos  int64
	)

	open := func() {
		if f != nil {
			f.Close()
		}
		for {
			f, err = os.Open(path)
			if err == nil {
				f.Seek(0, io.SeekEnd) // start from end
				pos = 0
				log.Printf("tail: opened %s", path)
				return
			}
			log.Printf("tail: waiting for %s (%v)", path, err)
			time.Sleep(2 * time.Second)
		}
	}

	open()
	scanner := bufio.NewScanner(f)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for range ticker.C {
		// detect logrotate: file shrank
		if info, err2 := os.Stat(path); err2 == nil {
			if info.Size() < pos {
				log.Println("tail: file rotated, re-opening")
				open()
				scanner = bufio.NewScanner(f)
			}
			pos = info.Size()
		}

		for scanner.Scan() {
			line := scanner.Text()
			if l, ok := parseLine(line); ok {
				out <- l
			}
		}
		// reset scanner without reopening (EOF is not permanent)
		scanner = bufio.NewScanner(f)
		f.Seek(pos, io.SeekStart)
	}
}

// ──────────────────────────────────────────────
// Aggregator goroutine
// ──────────────────────────────────────────────

func aggregator(lines <-chan LogLine, hub *Hub, interval time.Duration) {
	window := &Window{}
	pathCounts := map[string]int{}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var prevStub StubStatus
	var recentLogs []LogEntry

	for {
		select {
		case l := <-lines:
			window.add(l)
			pathCounts[l.Path]++
			entry := LogEntry{
				Time:   l.Time.Format("15:04:05"),
				IP:     l.IP,
				Method: l.Method,
				Path:   l.Path,
				Status: l.Status,
				Bytes:  l.Bytes,
			}
			recentLogs = append([]LogEntry{entry}, recentLogs...)
			if len(recentLogs) > 50 {
				recentLogs = recentLogs[:50]
			}

		case <-ticker.C:
			batch := window.drain()
			secs := interval.Seconds()

			var s2, s3, s4, s5 int
			var bytesOut int64
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
				bytesOut += l.Bytes
			}

			stub, err := fetchStubStatus(*flagStatus)
			if err != nil {
				stub = prevStub // use last known
			} else {
				prevStub = stub
			}

			// build top 6 paths
			type kv struct {
				k string
				v int
			}
			var sorted []kv
			for k, v := range pathCounts {
				sorted = append(sorted, kv{k, v})
			}
			for i := 0; i < len(sorted)-1; i++ {
				for j := i + 1; j < len(sorted); j++ {
					if sorted[j].v > sorted[i].v {
						sorted[i], sorted[j] = sorted[j], sorted[i]
					}
				}
			}
			top := []EndpointStat{}
			for i, kv := range sorted {
				if i >= 6 {
					break
				}
				top = append(top, EndpointStat{Path: kv.k, Count: kv.v})
			}

			snap := Snapshot{
				Timestamp:   time.Now().Format("15:04:05"),
				RPS:         float64(len(batch)) / secs,
				ActiveConns: stub.ActiveConnections,
				Reading:     stub.Reading,
				Writing:     stub.Writing,
				Waiting:     stub.Waiting,
				BytesIn:     stub.ActiveConnections * 2, // approx recv
				BytesOut:    bytesOut / 1024,
				Status2xx:   s2,
				Status3xx:   s3,
				Status4xx:   s4,
				Status5xx:   s5,
				TopPaths:    top,
				RecentLogs:  recentLogs,
			}

			msg, _ := json.Marshal(snap)
			hub.broadcast(msg)
		}
	}
}

// ──────────────────────────────────────────────
// HTTP handlers
// ──────────────────────────────────────────────

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func wsHandler(hub *Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Println("ws upgrade:", err)
			return
		}
		hub.register(conn)
		// keep alive – drain pings
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				hub.unregister(conn)
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

func healthHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"status":"ok"}`)
}

// ──────────────────────────────────────────────
// Main
// ──────────────────────────────────────────────

func main() {
	flag.Parse()

	log.Printf("nginx-dstat starting — log=%s status=%s addr=%s", *flagLog, *flagStatus, *flagAddr)

	hub := newHub()
	lines := make(chan LogLine, 1024)

	go tailLog(*flagLog, lines)
	go aggregator(lines, hub, *flagInterval)

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", wsHandler(hub))
	mux.HandleFunc("/health", healthHandler)

	// Serve the dashboard HTML (embedded below) at /
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// Replace __WS_URL__ at runtime with the actual host
		scheme := "ws"
		if r.TLS != nil {
			scheme = "wss"
		}
		wsURL := fmt.Sprintf("%s://%s/ws", scheme, r.Host)
		html := strings.ReplaceAll(dashboardHTML, "__WS_URL__", wsURL)
		fmt.Fprint(w, html)
	})

	handler := corsMiddleware(mux, *flagCORS)
	log.Printf("listening on %s", *flagAddr)
	if err := http.ListenAndServe(*flagAddr, handler); err != nil {
		log.Fatal(err)
	}
}

// ──────────────────────────────────────────────
// Embedded dashboard HTML
// ──────────────────────────────────────────────

const dashboardHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>nginx dstat</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link href="https://fonts.googleapis.com/css2?family=JetBrains+Mono:wght@400;500&family=Syne:wght@400;600;700&display=swap" rel="stylesheet">
<script src="https://cdnjs.cloudflare.com/ajax/libs/Chart.js/4.4.1/chart.umd.js"></script>
<style>
*{box-sizing:border-box;margin:0;padding:0;}
body{background:#0f1117;color:#e2e8f0;font-family:'Syne',sans-serif;padding:1.5rem;}
.header{display:flex;align-items:center;justify-content:space-between;margin-bottom:1.5rem;padding-bottom:1rem;border-bottom:1px solid rgba(255,255,255,.08);}
.dot{width:8px;height:8px;border-radius:50%;background:#22c55e;margin-right:10px;animation:pulse 1.5s ease-in-out infinite;}
.dot.off{background:#ef4444;animation:none;}
@keyframes pulse{0%,100%{opacity:1;}50%{opacity:.35;}}
.title{font-size:16px;font-weight:600;letter-spacing:-.3px;}
.sub{font-size:12px;color:#64748b;font-family:'JetBrains Mono',monospace;margin-top:2px;}
.clock{font-family:'JetBrains Mono',monospace;font-size:12px;color:#64748b;text-align:right;}
.grid6{display:grid;grid-template-columns:repeat(6,1fr);gap:10px;margin-bottom:1.25rem;}
.card{background:#1e2433;border-radius:8px;padding:12px 14px;}
.clabel{font-size:10px;color:#64748b;font-family:'JetBrains Mono',monospace;text-transform:uppercase;letter-spacing:.5px;margin-bottom:4px;}
.cval{font-size:22px;font-weight:600;color:#f1f5f9;font-family:'JetBrains Mono',monospace;line-height:1;}
.cunit{font-size:10px;color:#475569;margin-top:3px;font-family:'JetBrains Mono',monospace;}
.cdelta{font-size:10px;margin-top:2px;font-family:'JetBrains Mono',monospace;}
.up{color:#22c55e;}.dn{color:#ef4444;}.ne{color:#475569;}
.charts{display:grid;grid-template-columns:1fr 1fr;gap:12px;margin-bottom:12px;}
.chart-box{background:#1e2433;border-radius:10px;padding:14px;}
.chart-box.full{grid-column:1/-1;}
.ctitle{font-size:11px;color:#64748b;font-family:'JetBrains Mono',monospace;text-transform:uppercase;letter-spacing:.4px;margin-bottom:10px;}
.leg{display:flex;flex-wrap:wrap;gap:12px;margin-bottom:8px;}
.li{display:flex;align-items:center;gap:4px;font-size:11px;color:#64748b;}
.ld{width:8px;height:8px;border-radius:2px;}
.log-box{background:#1e2433;border-radius:10px;padding:14px;margin-top:12px;}
.log-entries{display:flex;flex-direction:column;gap:2px;max-height:200px;overflow-y:auto;}
.le{display:flex;gap:10px;font-size:11px;font-family:'JetBrains Mono',monospace;padding:3px 6px;border-radius:4px;}
.le:hover{background:rgba(255,255,255,.04);}
.lt{color:#475569;min-width:55px;}.lip{color:#475569;min-width:95px;}
.lm{color:#a78bfa;min-width:36px;}
.ls{min-width:30px;font-weight:500;}
.s2{color:#22c55e;}.s3{color:#3b82f6;}.s4{color:#f59e0b;}.s5{color:#ef4444;}
.lp{color:#e2e8f0;flex:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;}
.lsz{color:#475569;min-width:52px;text-align:right;}
.btm{display:grid;grid-template-columns:1fr 1fr;gap:12px;margin-top:12px;}
.ep-row{display:flex;align-items:center;gap:8px;font-size:12px;font-family:'JetBrains Mono',monospace;margin-bottom:4px;}
.ep-path{min-width:105px;color:#64748b;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;}
.ep-bw{flex:1;height:5px;background:#2d3748;border-radius:3px;overflow:hidden;}
.ep-b{height:100%;background:#3b82f6;border-radius:3px;transition:width .5s;}
.ep-c{min-width:42px;text-align:right;color:#f1f5f9;font-weight:500;}
</style>
</head>
<body>
<div class="header">
  <div style="display:flex;align-items:center;">
    <div class="dot" id="dot"></div>
    <div><div class="title">nginx / live traffic</div><div class="sub" id="sub">connecting to __WS_URL__…</div></div>
  </div>
  <div class="clock" id="clock">--:--:--</div>
</div>

<div class="grid6">
  <div class="card"><div class="clabel">req/s</div><div class="cval" id="m-rps">—</div><div class="cunit">requests/sec</div><div class="cdelta ne" id="d-rps">—</div></div>
  <div class="card"><div class="clabel">active</div><div class="cval" id="m-conn">—</div><div class="cunit">connections</div><div class="cdelta ne" id="d-conn">—</div></div>
  <div class="card"><div class="clabel">reading</div><div class="cval" id="m-reading">—</div><div class="cunit">nginx reading</div><div class="cdelta ne">&nbsp;</div></div>
  <div class="card"><div class="clabel">writing</div><div class="cval" id="m-writing">—</div><div class="cunit">nginx writing</div><div class="cdelta ne">&nbsp;</div></div>
  <div class="card"><div class="clabel">bytes out</div><div class="cval" id="m-bout">—</div><div class="cunit">KB/s sent</div><div class="cdelta ne" id="d-bout">—</div></div>
  <div class="card"><div class="clabel">errors</div><div class="cval" id="m-err">—</div><div class="cunit">4xx+5xx</div><div class="cdelta ne" id="d-err">—</div></div>
</div>

<div class="charts">
  <div class="chart-box"><div class="ctitle">requests / sec</div><div style="position:relative;height:120px"><canvas id="c-rps" role="img" aria-label="RPS over time"></canvas></div></div>
  <div class="chart-box">
    <div class="ctitle">response codes</div>
    <div class="leg"><div class="li"><div class="ld" style="background:#22c55e"></div>2xx</div><div class="li"><div class="ld" style="background:#3b82f6"></div>3xx</div><div class="li"><div class="ld" style="background:#f59e0b"></div>4xx</div><div class="li"><div class="ld" style="background:#ef4444"></div>5xx</div></div>
    <div style="position:relative;height:96px"><canvas id="c-codes" role="img" aria-label="HTTP codes over time"></canvas></div>
  </div>
</div>

<div class="btm">
  <div class="chart-box"><div class="ctitle">top endpoints</div><div id="ep-list"></div></div>
  <div class="chart-box">
    <div class="ctitle">bandwidth (KB/s out)</div>
    <div style="position:relative;height:130px"><canvas id="c-bw" role="img" aria-label="Bandwidth over time"></canvas></div>
  </div>
</div>

<div class="log-box">
  <div class="ctitle">access log — live tail</div>
  <div class="log-entries" id="log-list"></div>
</div>

<script>
const N=40,labels=[],rps=[],bw=[],c2=[],c3=[],c4=[],c5=[];
for(let i=0;i<N;i++){labels.push('');rps.push(0);bw.push(0);c2.push(0);c3.push(0);c4.push(0);c5.push(0);}

const mkLine=(id,sets)=>new Chart(document.getElementById(id),{type:'line',data:{labels,datasets:sets},options:{responsive:true,maintainAspectRatio:false,animation:{duration:180},plugins:{legend:{display:false},tooltip:{enabled:false}},scales:{x:{display:false},y:{display:true,grid:{color:'rgba(255,255,255,.05)'},ticks:{font:{size:9,family:'JetBrains Mono'},color:'rgba(255,255,255,.25)',maxTicksLimit:4}}}}});

const cRps=mkLine('c-rps',[{data:rps,borderColor:'#3b82f6',borderWidth:1.5,fill:true,backgroundColor:'rgba(59,130,246,.07)',tension:.4,pointRadius:0}]);
const cBw=mkLine('c-bw',[{data:bw,borderColor:'#22c55e',borderWidth:1.5,fill:true,backgroundColor:'rgba(34,197,94,.07)',tension:.4,pointRadius:0}]);
const cCodes=mkLine('c-codes',[{data:c2,borderColor:'#22c55e',borderWidth:1.5,fill:false,tension:.4,pointRadius:0},{data:c3,borderColor:'#3b82f6',borderWidth:1.5,fill:false,tension:.4,pointRadius:0},{data:c4,borderColor:'#f59e0b',borderWidth:1.5,fill:false,tension:.4,pointRadius:0},{data:c5,borderColor:'#ef4444',borderWidth:1.5,fill:false,tension:.4,pointRadius:0}]);

let pRps=0,pConn=0,pBout=0,pErr=0;

function delta(now,prev,unit){
  const d=now-prev;
  if(Math.abs(d)<0.5)return '<span class="ne">—</span>';
  return d>0?'<span class="up">▲ '+Math.abs(d).toFixed(1)+' '+unit+'</span>':'<span class="dn">▼ '+Math.abs(d).toFixed(1)+' '+unit+'</span>';
}

function update(s){
  document.getElementById('clock').textContent=s.ts;
  document.getElementById('sub').textContent='ws connected — nginx live';
  document.getElementById('dot').classList.remove('off');
  const rpsVal=+s.rps.toFixed(1);
  const errVal=s.s4xx+s.s5xx;
  document.getElementById('m-rps').textContent=rpsVal;
  document.getElementById('m-conn').textContent=s.active_conns;
  document.getElementById('m-reading').textContent=s.reading;
  document.getElementById('m-writing').textContent=s.writing;
  document.getElementById('m-bout').textContent=s.bytes_out_kb;
  document.getElementById('m-err').textContent=errVal;
  document.getElementById('d-rps').innerHTML=delta(rpsVal,pRps,'r/s');
  document.getElementById('d-conn').innerHTML=delta(s.active_conns,pConn,'');
  document.getElementById('d-bout').innerHTML=delta(s.bytes_out_kb,pBout,'KB/s');
  document.getElementById('d-err').innerHTML=delta(errVal,pErr,'');
  pRps=rpsVal;pConn=s.active_conns;pBout=s.bytes_out_kb;pErr=errVal;

  [rps,bw,c2,c3,c4,c5,labels].forEach(a=>a.shift());
  rps.push(rpsVal);bw.push(s.bytes_out_kb);
  c2.push(s.s2xx);c3.push(s.s3xx);c4.push(s.s4xx);c5.push(s.s5xx);
  labels.push(s.ts.slice(3));
  cRps.update('none');cBw.update('none');cCodes.update('none');

  if(s.top_paths&&s.top_paths.length){
    const max=s.top_paths[0].count||1;
    document.getElementById('ep-list').innerHTML=s.top_paths.map(p=>'<div class="ep-row"><span class="ep-path">'+p.path+'</span><div class="ep-bw"><div class="ep-b" style="width:'+Math.round(p.count/max*100)+'%"></div></div><span class="ep-c">'+p.count+'</span></div>').join('');
  }

  if(s.recent_logs&&s.recent_logs.length){
    const sc=s2=>s2<300?'s2':s2<400?'s3':s2<500?'s4':'s5';
    document.getElementById('log-list').innerHTML=s.recent_logs.slice(0,40).map(l=>'<div class="le"><span class="lt">'+l.time+'</span><span class="lip">'+l.ip+'</span><span class="lm">'+l.method+'</span><span class="ls '+sc(l.status)+'">'+l.status+'</span><span class="lp">'+l.path+'</span><span class="lsz">'+l.bytes+'B</span></div>').join('');
  }
}

function connect(){
  const ws=new WebSocket('__WS_URL__');
  ws.onopen=()=>console.log('ws connected');
  ws.onmessage=e=>{try{update(JSON.parse(e.data));}catch(err){console.error(err);}};
  ws.onclose=()=>{
    document.getElementById('dot').classList.add('off');
    document.getElementById('sub').textContent='reconnecting…';
    setTimeout(connect,3000);
  };
}
connect();
setInterval(()=>{const n=new Date();document.getElementById('clock').textContent=n.toTimeString().slice(0,8);},1000);
</script>
</body>
</html>`
