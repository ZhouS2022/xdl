package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

func serveMain(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "网页监听地址")
	dir := fs.String("dir", ".", "安装目录，视频保存在这里")
	proxyFlag := fs.String("proxy", "", "HTTP 代理，例如 http://127.0.0.1:7897")
	fs.Parse(args)

	abs, err := filepath.Abs(*dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "目录无效: %v\n", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "无法创建目录: %v\n", err)
		os.Exit(1)
	}

	s := &webServer{
		dir:   abs,
		proxy: activeProxy(*proxyFlag),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.page)
	mux.HandleFunc("/api/download", s.startDownload)
	mux.HandleFunc("/api/status", s.status)
	mux.HandleFunc("/api/files", s.files)

	srv := &http.Server{Addr: *addr, Handler: mux}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(shut)
	}()

	fmt.Fprintf(os.Stderr, "网页已启动 %s\n视频保存到 %s\n", *addr, abs)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "网页启动失败: %v\n", err)
		os.Exit(1)
	}
}

type webServer struct {
	dir   string
	proxy string

	mu      sync.Mutex
	running bool
	url     string
	quality string
	file    string
	message string
	errText string
	done    int64
	total   int64
	saved   []string
}

func (s *webServer) page(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, pageHTML)
}

func (s *webServer) startDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		URL     string `json:"url"`
		Quality string `json:"quality"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "请求无效", http.StatusBadRequest)
		return
	}
	req.URL = strings.TrimSpace(req.URL)
	if req.URL == "" {
		http.Error(w, "请输入视频链接", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Quality) == "" {
		req.Quality = "best"
	}

	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		http.Error(w, "已有下载正在进行", http.StatusConflict)
		return
	}
	s.running = true
	s.url = req.URL
	s.quality = req.Quality
	s.file = ""
	s.message = "正在解析链接"
	s.errText = ""
	s.done = 0
	s.total = 0
	s.saved = nil
	s.mu.Unlock()

	go s.run(req.URL, req.Quality)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "started"})
}

func (s *webServer) run(link, quality string) {
	defer func() {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
	}()
	hooks := &downloadHooks{
		Status: func(msg string) {
			s.mu.Lock()
			s.message = msg
			s.mu.Unlock()
		},
		File: func(name string) {
			s.mu.Lock()
			s.file = name
			s.done = 0
			s.total = 0
			s.mu.Unlock()
		},
		Progress: func(done, total int64) {
			s.mu.Lock()
			s.done = done
			s.total = total
			s.mu.Unlock()
		},
	}
	paths, err := runDownload(context.Background(), link, quality, s.dir, s.proxy, hooks)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.errText = err.Error()
		s.message = "下载失败"
		return
	}
	s.saved = paths
	s.message = fmt.Sprintf("已保存 %d 个文件", len(paths))
}

func (s *webServer) status(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pct := 0
	if s.total > 0 {
		pct = int(float64(s.done) / float64(s.total) * 100)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"running": s.running,
		"url":     s.url,
		"quality": s.quality,
		"file":    s.file,
		"message": s.message,
		"error":   s.errText,
		"done":    s.done,
		"total":   s.total,
		"percent": pct,
		"saved":   s.saved,
		"dir":     s.dir,
	})
}

func (s *webServer) files(w http.ResponseWriter, r *http.Request) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type item struct {
		Name    string `json:"name"`
		Size    int64  `json:"size"`
		ModTime string `json:"modTime"`
	}
	list := make([]item, 0)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".mp4") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		list = append(list, item{
			Name:    name,
			Size:    info.Size(),
			ModTime: info.ModTime().Format("2006-01-02 15:04"),
		})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ModTime > list[j].ModTime })
	writeJSON(w, http.StatusOK, map[string]any{"dir": s.dir, "files": list})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

const pageHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>视频下载</title>
<style>
  :root { color-scheme: light; }
  body { margin: 0; font: 16px/1.5 "Segoe UI", "PingFang SC", "Microsoft YaHei", sans-serif; background: #f4f5f7; color: #1c1c1c; }
  main { max-width: 720px; margin: 40px auto; padding: 0 16px; }
  h1 { font-size: 22px; font-weight: 650; margin: 0 0 4px; }
  .sub { color: #5c6570; margin: 0 0 20px; }
  form, .card { background: #fff; border: 1px solid #e3e6ea; border-radius: 12px; padding: 16px; }
  label { display: block; font-size: 14px; margin-bottom: 6px; }
  input, select { width: 100%; box-sizing: border-box; font: inherit; padding: 10px 12px; border: 1px solid #cfd5dc; border-radius: 8px; background: #fff; }
  .row { display: flex; gap: 12px; margin-top: 12px; }
  .row .q { width: 140px; flex: none; }
  button { font: inherit; background: #1769e0; color: #fff; border: 0; border-radius: 8px; padding: 10px 16px; cursor: pointer; }
  button:disabled { background: #9bb7e4; cursor: default; }
  #status { margin-top: 16px; }
  .bar { height: 8px; background: #e7ebf0; border-radius: 99px; overflow: hidden; margin-top: 8px; }
  .bar > div { height: 100%; width: 0; background: #1769e0; }
  .err { color: #b42318; }
  ul { list-style: none; padding: 0; margin: 8px 0 0; }
  li { display: flex; justify-content: space-between; gap: 12px; padding: 8px 0; border-top: 1px solid #eef1f4; }
  .name { overflow: hidden; text-overflow: ellipsis; }
  .meta { color: #5c6570; white-space: nowrap; }
</style>
</head>
<body>
<main>
  <h1>视频下载</h1>
  <p class="sub">输入 X 推文链接或 mp4 直链，文件保存到服务器安装目录 <span id="dir"></span></p>
  <form id="form">
    <label for="url">视频链接</label>
    <input id="url" name="url" type="url" required placeholder="https://x.com/用户/status/123" autocomplete="off">
    <div class="row">
      <div class="q">
        <label for="quality">画质</label>
        <select id="quality" name="quality">
          <option value="best">最高</option>
          <option value="4k">4K</option>
          <option value="1080">1080p</option>
          <option value="720">720p</option>
          <option value="480">480p</option>
          <option value="360">360p</option>
        </select>
      </div>
      <div style="flex:1; align-self:end">
        <button id="go" type="submit">下载</button>
      </div>
    </div>
  </form>
  <div id="status"></div>
  <section class="card" style="margin-top:16px">
    <strong>已下载</strong>
    <ul id="files"></ul>
  </section>
</main>
<script>
const statusEl = document.getElementById('status');
const filesEl = document.getElementById('files');
const go = document.getElementById('go');
function size(n) {
  if (!n) return '0 B';
  const u = ['B','KB','MB','GB'];
  let i = 0, v = n;
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
  return (i ? v.toFixed(1) : v) + ' ' + u[i];
}
async function loadFiles() {
  const r = await fetch('/api/files');
  const data = await r.json();
  document.getElementById('dir').textContent = data.dir || '';
  filesEl.innerHTML = '';
  if (!data.files || !data.files.length) {
    filesEl.innerHTML = '<li><span class="meta">还没有文件</span></li>';
    return;
  }
  for (const f of data.files) {
    const li = document.createElement('li');
    li.innerHTML = '<span class="name"></span><span class="meta"></span>';
    li.querySelector('.name').textContent = f.name;
    li.querySelector('.meta').textContent = size(f.size) + '  ' + f.modTime;
    filesEl.appendChild(li);
  }
}
function renderStatus(s) {
  if (!s || (!s.running && !s.message && !s.error)) { statusEl.innerHTML = ''; return; }
  const pct = s.total ? s.percent : (s.running ? 0 : 100);
  const detail = s.file ? (s.file + '  ' + size(s.done) + (s.total ? ' / ' + size(s.total) : '')) : '';
  statusEl.innerHTML = '<div class="card"><div class="msg"></div><div class="meta"></div><div class="bar"><div></div></div><div class="err"></div></div>';
  statusEl.querySelector('.msg').textContent = s.message || '';
  statusEl.querySelector('.meta').textContent = detail;
  statusEl.querySelector('.bar > div').style.width = (s.running || s.total ? pct : 0) + '%';
  statusEl.querySelector('.err').textContent = s.error || '';
  go.disabled = !!s.running;
}
async function poll() {
  const r = await fetch('/api/status');
  const s = await r.json();
  renderStatus(s);
  if (s.running) {
    setTimeout(poll, 400);
  } else {
    go.disabled = false;
    loadFiles();
  }
}
document.getElementById('form').addEventListener('submit', async (e) => {
  e.preventDefault();
  go.disabled = true;
  const res = await fetch('/api/download', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({url: document.getElementById('url').value, quality: document.getElementById('quality').value})
  });
  if (!res.ok) {
    const text = await res.text();
    renderStatus({message: '无法开始', error: text, running: false});
    go.disabled = false;
    return;
  }
  poll();
});
loadFiles();
poll();
</script>
</body>
</html>
`
