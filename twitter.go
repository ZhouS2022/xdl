package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	reStatus = regexp.MustCompile(`(?i)(?:twitter\.com|x\.com)/(?:([^/?#]+)/)?status/(\d+)`)
	reRes    = regexp.MustCompile(`/(\d{2,5})x(\d{2,5})/`)
)

type variant struct {
	Author   string
	ID       string
	URL      string
	Width    int
	Height   int
	Bitrate  int
	Duration float64
}

func (v variant) label() string {
	who := v.ID
	if v.Author != "" {
		who = "@" + v.Author + " " + v.ID
	}
	size := "未知画质"
	if v.Width > 0 && v.Height > 0 {
		size = fmt.Sprintf("%dx%d", v.Width, v.Height)
	}
	if v.Duration > 0 {
		sec := int(v.Duration + 0.5)
		return fmt.Sprintf("%s  %s  %d:%02d", who, size, sec/60, sec%60)
	}
	return fmt.Sprintf("%s  %s", who, size)
}

func (v variant) fileName() string {
	var b strings.Builder
	if v.Author != "" {
		b.WriteString(safeName(v.Author))
		b.WriteByte('-')
	}
	if v.ID != "" {
		b.WriteString(safeName(v.ID))
	} else {
		b.WriteString("video")
	}
	if v.Width > 0 && v.Height > 0 {
		fmt.Fprintf(&b, "-%dx%d", v.Width, v.Height)
	}
	b.WriteString(".mp4")
	return b.String()
}

func safeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '<', '>', ':', '"', '/', '\\', '|', '?', '*':
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.Trim(b.String(), " .")
	if out == "" {
		return "video"
	}
	return out
}

func resolve(ctx context.Context, link, proxy string) ([][]variant, error) {
	if direct, ok := directVideo(link); ok {
		return [][]variant{{direct}}, nil
	}
	user, id, err := parseStatus(link)
	if err != nil {
		return nil, err
	}
	client, err := newClient(proxy)
	if err != nil {
		return nil, err
	}
	videos, err := fetchTweet(ctx, client, user, id)
	if err == nil || proxy != "" || !isNetErr(err) {
		return videos, err
	}
	fallback := localProxy()
	if fallback == "" {
		return nil, err
	}
	fmt.Fprintf(os.Stderr, "直连解析失败，改用 %s\n", fallback)
	client, cerr := newClient(fallback)
	if cerr != nil {
		return nil, err
	}
	return fetchTweet(ctx, client, user, id)
}

func parseStatus(link string) (user, id string, err error) {
	m := reStatus.FindStringSubmatch(link)
	if m == nil {
		return "", "", fmt.Errorf("无法识别的链接。请传入 x.com 或 twitter.com 的推文地址，或 mp4 直链")
	}
	user, id = m[1], m[2]
	if strings.EqualFold(user, "i") || strings.EqualFold(user, "intent") {
		user = ""
	}
	return user, id, nil
}

func directVideo(link string) (variant, bool) {
	u, err := url.Parse(link)
	if err != nil || u.Host == "" {
		return variant{}, false
	}
	if !strings.HasSuffix(strings.ToLower(u.Path), ".mp4") {
		return variant{}, false
	}
	v := variant{URL: link}
	if m := reRes.FindStringSubmatch(u.Path); m != nil {
		v.Width, _ = strconv.Atoi(m[1])
		v.Height, _ = strconv.Atoi(m[2])
	}
	base := u.Path
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	base = strings.TrimSuffix(base, ".mp4")
	if base != "" {
		v.ID = safeName(base)
	}
	return v, true
}

func fetchTweet(ctx context.Context, client *http.Client, user, id string) ([][]variant, error) {
	endpoints := make([]string, 0, 2)
	if user != "" {
		endpoints = append(endpoints,
			fmt.Sprintf("https://api.fxtwitter.com/%s/status/%s", url.PathEscape(user), id),
			fmt.Sprintf("https://api.vxtwitter.com/%s/status/%s", url.PathEscape(user), id),
		)
	} else {
		endpoints = append(endpoints,
			"https://api.fxtwitter.com/status/"+id,
			"https://api.vxtwitter.com/i/status/"+id,
		)
	}
	var (
		best [][]variant
		last error
	)
	for _, endpoint := range endpoints {
		videos, err := fetchEndpoint(ctx, client, endpoint)
		if err != nil {
			last = err
			continue
		}
		if len(videos) == 0 {
			last = fmt.Errorf("这条推文没有视频")
			continue
		}
		if variantCount(videos) > variantCount(best) {
			best = videos
		}
	}
	if variantCount(best) == 0 {
		if last == nil {
			last = fmt.Errorf("这条推文没有视频")
		}
		return nil, last
	}
	return best, nil
}

func variantCount(groups [][]variant) int {
	n := 0
	for _, group := range groups {
		n += len(group)
	}
	return n
}

func fetchEndpoint(ctx context.Context, client *http.Client, endpoint string) ([][]variant, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "xdl")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s 返回 %s", hostOf(endpoint), resp.Status)
	}
	if strings.Contains(endpoint, "fxtwitter.com") {
		return parseFx(body)
	}
	return parseVx(body)
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return u.Host
}

type fxResp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Tweet   struct {
		ID     string `json:"id"`
		Author struct {
			ScreenName string `json:"screen_name"`
		} `json:"author"`
		Media struct {
			Videos []fxVideo `json:"videos"`
		} `json:"media"`
	} `json:"tweet"`
}

type fxVideo struct {
	URL      string     `json:"url"`
	Width    int        `json:"width"`
	Height   int        `json:"height"`
	Duration float64    `json:"duration"`
	Formats  []fxFormat `json:"formats"`
	Variants []fxFormat `json:"variants"`
}

type fxFormat struct {
	URL         string `json:"url"`
	Bitrate     int    `json:"bitrate"`
	Container   string `json:"container"`
	ContentType string `json:"content_type"`
}

func parseFx(body []byte) ([][]variant, error) {
	var resp fxResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	if resp.Code != 0 && resp.Code != http.StatusOK {
		if resp.Message != "" {
			return nil, fmt.Errorf("fxtwitter: %s", resp.Message)
		}
		return nil, fmt.Errorf("fxtwitter: code %d", resp.Code)
	}
	author := resp.Tweet.Author.ScreenName
	id := resp.Tweet.ID
	groups := make([][]variant, 0, len(resp.Tweet.Media.Videos))
	for _, video := range resp.Tweet.Media.Videos {
		formats := video.Formats
		if len(formats) == 0 {
			formats = video.Variants
		}
		group := make([]variant, 0, len(formats)+1)
		for _, format := range formats {
			container := format.Container
			if container == "" {
				container = format.ContentType
			}
			if !isMP4(format.URL, container) {
				continue
			}
			v := variant{
				Author:   author,
				ID:       id,
				URL:      format.URL,
				Bitrate:  format.Bitrate,
				Duration: video.Duration,
			}
			v.Width, v.Height = resolutionOf(format.URL)
			group = append(group, v)
		}
		if len(group) == 0 && isMP4(video.URL, "mp4") {
			group = append(group, variant{
				Author: author, ID: id, URL: video.URL,
				Width: video.Width, Height: video.Height, Duration: video.Duration,
			})
		}
		if len(group) > 0 {
			groups = append(groups, group)
		}
	}
	return groups, nil
}

type vxResp struct {
	TweetID        string `json:"tweetID"`
	UserScreenName string `json:"user_screen_name"`
	MediaExtended  []struct {
		Type           string `json:"type"`
		URL            string `json:"url"`
		DurationMillis int    `json:"duration_millis"`
		Size           struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"size"`
	} `json:"media_extended"`
}

func parseVx(body []byte) ([][]variant, error) {
	var resp vxResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	groups := make([][]variant, 0)
	for _, media := range resp.MediaExtended {
		if !strings.EqualFold(media.Type, "video") && !strings.EqualFold(media.Type, "gif") {
			continue
		}
		if !isMP4(media.URL, "") {
			continue
		}
		w, h := media.Size.Width, media.Size.Height
		if w == 0 || h == 0 {
			w, h = resolutionOf(media.URL)
		}
		groups = append(groups, []variant{{
			Author:   resp.UserScreenName,
			ID:       resp.TweetID,
			URL:      media.URL,
			Width:    w,
			Height:   h,
			Duration: float64(media.DurationMillis) / 1000,
		}})
	}
	return groups, nil
}

func isMP4(rawURL, container string) bool {
	if strings.EqualFold(container, "m3u8") {
		return false
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return strings.HasSuffix(strings.ToLower(u.Path), ".mp4")
}

func resolutionOf(rawURL string) (int, int) {
	u, err := url.Parse(rawURL)
	path := rawURL
	if err == nil {
		path = u.Path
	}
	m := reRes.FindStringSubmatch(path)
	if m == nil {
		return 0, 0
	}
	w, _ := strconv.Atoi(m[1])
	h, _ := strconv.Atoi(m[2])
	return w, h
}

func pickVariant(group []variant, quality string) (variant, error) {
	if len(group) == 0 {
		return variant{}, fmt.Errorf("没有可用的 mp4")
	}
	want, best, err := parseQuality(quality)
	if err != nil {
		return variant{}, err
	}
	sorted := append([]variant(nil), group...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Height != sorted[j].Height {
			return sorted[i].Height > sorted[j].Height
		}
		return sorted[i].Bitrate > sorted[j].Bitrate
	})
	if best {
		return sorted[0], nil
	}
	var available []string
	seen := map[string]bool{}
	for _, v := range sorted {
		if v.Height == want || v.Width == want {
			return v, nil
		}
		label := "未知分辨率"
		if v.Width > 0 && v.Height > 0 {
			label = fmt.Sprintf("%dx%d", v.Width, v.Height)
		}
		if !seen[label] {
			seen[label] = true
			available = append(available, label)
		}
	}
	return variant{}, fmt.Errorf("没有 %d 画质，可选: %s", want, strings.Join(available, "、"))
}

func parseQuality(q string) (want int, best bool, err error) {
	switch strings.ToLower(strings.TrimSpace(q)) {
	case "", "best", "max":
		return 0, true, nil
	case "4k", "2160", "2160p":
		return 2160, false, nil
	case "1440", "1440p":
		return 1440, false, nil
	case "1080", "1080p", "fhd":
		return 1080, false, nil
	case "720", "720p", "hd":
		return 720, false, nil
	case "480", "480p":
		return 480, false, nil
	case "360", "360p":
		return 360, false, nil
	case "270", "270p":
		return 270, false, nil
	default:
		n, convErr := strconv.Atoi(strings.TrimSuffix(strings.ToLower(strings.TrimSpace(q)), "p"))
		if convErr != nil || n <= 0 {
			return 0, false, fmt.Errorf("无法识别的画质 %q，可用 best、4k、1080、720、480、360", q)
		}
		return n, false, nil
	}
}
