package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		serveMain(os.Args[2:])
		return
	}

	out := flag.String("o", ".", "输出文件或目录")
	quality := flag.String("q", "best", "画质: best、4k、2160、1080、720、480、360")
	proxyFlag := flag.String("proxy", "", "HTTP 代理，例如 http://127.0.0.1:7897。未指定时使用环境变量，直连失败会尝试本机常见代理端口")
	flag.Usage = usage
	os.Args = append([]string{os.Args[0]}, normalizeArgs(os.Args[1:])...)
	flag.Parse()
	if flag.NArg() != 1 {
		usage()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	proxy := activeProxy(*proxyFlag)
	if strings.TrimSpace(*proxyFlag) == "" && proxy != "" && proxyFromEnv() == "" {
		fmt.Fprintf(os.Stderr, "使用代理 %s\n", proxy)
	}

	paths, err := runDownload(ctx, flag.Arg(0), *quality, *out, proxy, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	for _, p := range paths {
		fmt.Println(p)
	}
}

type downloadHooks struct {
	Status   func(string)
	File     func(string)
	Progress func(done, total int64)
}

func runDownload(ctx context.Context, link, quality, out, proxy string, h *downloadHooks) ([]string, error) {
	if h != nil && h.Status != nil {
		h.Status("正在解析链接")
	}
	videos, err := resolve(ctx, link, proxy)
	if err != nil {
		return nil, fmt.Errorf("解析失败: %w", err)
	}
	chosen := make([]variant, 0, len(videos))
	for _, group := range videos {
		v, err := pickVariant(group, quality)
		if err != nil {
			return nil, fmt.Errorf("选择画质失败: %w", err)
		}
		chosen = append(chosen, v)
	}
	paths := make([]string, 0, len(chosen))
	for i, v := range chosen {
		dest, err := destPath(out, v, i, len(chosen))
		if err != nil {
			return nil, fmt.Errorf("输出路径无效: %w", err)
		}
		if h != nil && h.File != nil {
			h.File(filepath.Base(dest))
		}
		if h == nil {
			fmt.Fprintf(os.Stderr, "下载 %s  %s\n", v.label(), dest)
		} else if h.Status != nil {
			h.Status("正在下载 " + filepath.Base(dest))
		}
		if err := save(ctx, v.URL, dest, proxy, h); err != nil {
			return nil, fmt.Errorf("下载失败: %w", err)
		}
		paths = append(paths, dest)
	}
	return paths, nil
}

func activeProxy(flagValue string) string {
	proxy := strings.TrimSpace(flagValue)
	if proxy == "" {
		proxy = proxyFromEnv()
	}
	if proxy == "" {
		proxy = localProxy()
	}
	return proxy
}

// normalizeArgs lets the link appear before flags. The standard flag
// parser stops at the first non-flag argument.
func normalizeArgs(args []string) []string {
	var flags, positionals []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positionals = append(positionals, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "-") && a != "-" && !strings.Contains(a, "://") {
			flags = append(flags, a)
			name := strings.TrimLeft(a, "-")
			if !strings.Contains(name, "=") && (name == "o" || name == "q" || name == "proxy") && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		positionals = append(positionals, a)
	}
	return append(flags, positionals...)
}

func usage() {
	fmt.Fprintf(os.Stderr, `用法:
  xdl <推文链接或视频直链> [-o 输出] [-q 画质] [-proxy 代理]
  xdl serve [-addr :8080] [-dir 安装目录] [-proxy 代理]

从 X/Twitter 推文链接解析视频并下载。也可以直接传入 mp4 地址。
serve 启动网页，把视频保存到安装目录。

示例:
  xdl https://x.com/user/status/123
  xdl https://x.com/user/status/123 -q 1080 -o D:\videos
  xdl serve -addr :8080 -dir /opt/xdl

`)
	flag.PrintDefaults()
}

func proxyFromEnv() string {
	for _, key := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	return ""
}

func newClient(proxy string) (*http.Client, error) {
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 8 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     false,
		TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{},
		TLSHandshakeTimeout:   8 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       30 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	if proxy != "" {
		u, err := parseProxy(proxy)
		if err != nil {
			return nil, err
		}
		tr.Proxy = http.ProxyURL(u)
	}
	return &http.Client{Transport: tr}, nil
}

func parseProxy(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("代理地址无效: %s", raw)
	}
	return u, nil
}

func save(ctx context.Context, rawURL, dest, proxy string, h *downloadHooks) error {
	err := download(ctx, rawURL, dest, proxy, h)
	if err == nil || proxy != "" || !isNetErr(err) {
		return err
	}
	fallback := localProxy()
	if fallback == "" {
		return err
	}
	if h != nil && h.Status != nil {
		h.Status("直连失败，改用 " + fallback)
	} else {
		fmt.Fprintf(os.Stderr, "直连失败，改用 %s\n", fallback)
	}
	return download(ctx, rawURL, dest, fallback, h)
}

func localProxy() string {
	for _, addr := range []string{"127.0.0.1:7897", "127.0.0.1:7890"} {
		conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
		if err == nil {
			conn.Close()
			return "http://" + addr
		}
	}
	return ""
}

func isNetErr(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	return errors.As(err, &netErr) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || strings.Contains(err.Error(), "connection reset") || strings.Contains(err.Error(), "EOF")
}

func download(ctx context.Context, rawURL, dest, proxy string, h *downloadHooks) error {
	client, err := newClient(proxy)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Referer", "https://x.com/")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("视频地址返回 %s", resp.Status)
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	part := dest + ".part"
	f, err := os.Create(part)
	if err != nil {
		return err
	}

	written, copyErr := copyProgress(ctx, f, resp.Body, resp.ContentLength, h)
	closeErr := f.Close()
	if copyErr != nil {
		os.Remove(part)
		return copyErr
	}
	if closeErr != nil {
		os.Remove(part)
		return closeErr
	}
	if resp.ContentLength > 0 && written != resp.ContentLength {
		os.Remove(part)
		return fmt.Errorf("下载不完整: %d / %d 字节", written, resp.ContentLength)
	}
	if err := os.Remove(dest); err != nil && !os.IsNotExist(err) {
		os.Remove(part)
		return err
	}
	if err := os.Rename(part, dest); err != nil {
		os.Remove(part)
		return err
	}
	if h == nil {
		fmt.Fprintln(os.Stderr)
	}
	return nil
}

func copyProgress(ctx context.Context, dst io.Writer, src io.Reader, total int64, h *downloadHooks) (int64, error) {
	buf := make([]byte, 256*1024)
	var written int64
	last := time.Now()
	report := func() {
		if h != nil && h.Progress != nil {
			h.Progress(written, total)
			return
		}
		fmt.Fprintf(os.Stderr, "\r%s", progressLine(written, total))
	}
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		n, err := src.Read(buf)
		if n > 0 {
			nw, werr := dst.Write(buf[:n])
			written += int64(nw)
			if time.Since(last) > 200*time.Millisecond {
				report()
				last = time.Now()
			}
			if werr != nil {
				return written, werr
			}
		}
		if err == io.EOF {
			report()
			return written, nil
		}
		if err != nil {
			return written, err
		}
	}
}

func progressLine(done, total int64) string {
	if total > 0 {
		pct := float64(done) / float64(total) * 100
		return fmt.Sprintf("%s / %s  %.0f%%", humanSize(done), humanSize(total), pct)
	}
	return humanSize(done)
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	v := float64(n)
	for _, suffix := range []string{"KB", "MB", "GB"} {
		v /= unit
		if v < unit {
			return fmt.Sprintf("%.1f %s", v, suffix)
		}
	}
	return fmt.Sprintf("%.1f TB", v/unit)
}

func destPath(out string, v variant, index, total int) (string, error) {
	name := v.fileName()
	if total > 1 {
		ext := filepath.Ext(name)
		name = fmt.Sprintf("%s-%d%s", strings.TrimSuffix(name, ext), index+1, ext)
	}
	out = strings.TrimSpace(out)
	if out == "" {
		out = "."
	}
	info, err := os.Stat(out)
	asDir := err == nil && info.IsDir() || strings.HasSuffix(out, string(os.PathSeparator)) || strings.HasSuffix(out, "/")
	if !asDir && err != nil && !os.IsNotExist(err) {
		return "", err
	}
	if !asDir && (out == "." || filepath.Ext(out) == "") {
		asDir = true
	}
	if asDir {
		if err := os.MkdirAll(out, 0o755); err != nil {
			return "", err
		}
		return filepath.Join(out, name), nil
	}
	if total > 1 {
		ext := filepath.Ext(out)
		out = fmt.Sprintf("%s-%d%s", strings.TrimSuffix(out, ext), index+1, ext)
	}
	return out, nil
}
