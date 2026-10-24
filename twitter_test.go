package main

import (
	"strings"
	"testing"
)

func TestParseStatus(t *testing.T) {
	user, id, err := parseStatus("https://x.com/WannakaLeaf/status/2113279645592502603/video/1")
	if err != nil || user != "WannakaLeaf" || id != "2113279645592502603" {
		t.Fatalf("got %q %q %v", user, id, err)
	}
	user, id, err = parseStatus("https://twitter.com/i/status/99")
	if err != nil || user != "" || id != "99" {
		t.Fatalf("i/status got %q %q %v", user, id, err)
	}
	if _, _, err := parseStatus("https://example.com/watch?v=1"); err == nil {
		t.Fatal("expected error")
	}
}

func TestPickVariant(t *testing.T) {
	group := []variant{
		{Width: 480, Height: 270, Bitrate: 256000, URL: "270"},
		{Width: 1280, Height: 720, Bitrate: 2176000, URL: "720"},
		{Width: 3840, Height: 2160, Bitrate: 25128000, URL: "2160"},
	}
	best, err := pickVariant(group, "best")
	if err != nil || best.URL != "2160" {
		t.Fatalf("best: %+v %v", best, err)
	}
	hd, err := pickVariant(group, "720")
	if err != nil || hd.URL != "720" {
		t.Fatalf("720: %+v %v", hd, err)
	}
	if _, err := pickVariant(group, "1080"); err == nil {
		t.Fatal("expected missing 1080")
	}
}

func TestNormalizeArgs(t *testing.T) {
	got := normalizeArgs([]string{"https://x.com/a/status/1", "-q", "720", "-o", `D:\videos`})
	want := []string{"-q", "720", "-o", `D:\videos`, "https://x.com/a/status/1"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got %#v", got)
	}
}

func TestDirectVideo(t *testing.T) {
	v, ok := directVideo("https://video.twimg.com/amplify_video/1/vid/avc1/1280x720/abc.mp4?tag=29")
	if !ok || v.Width != 1280 || v.Height != 720 {
		t.Fatalf("%+v %v", v, ok)
	}
	if _, ok := directVideo("https://x.com/a/status/1"); ok {
		t.Fatal("tweet url is not a direct video")
	}
}
