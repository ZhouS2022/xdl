# xdl

从 X（Twitter）推文链接或 mp4 直链下载视频。可以在命令行直接保存，也可以开一个网页，把文件下到安装目录。

支持 `x.com`、`twitter.com` 的推文地址，以及 `video.twimg.com` 上的 mp4 直链。一条推文里有多段视频时会全部下载。默认保存最高画质，文件名形如 `用户名-推文ID-宽x高.mp4`。

## 命令行

```text
xdl <推文链接或视频直链> [-o 输出] [-q 画质] [-proxy 代理]
```

```text
xdl https://x.com/user/status/123
xdl https://x.com/user/status/123 -q 1080 -o D:\videos
xdl https://video.twimg.com/.../vid/avc1/1280x720/xx.mp4 -o clip.mp4
```

`-o` 可以是目录或具体文件名，默认当前目录。`-q` 可用 `best`（默认）、`4k`、`1080`、`720`、`480`、`360`。没有该画质时，程序会列出这条视频实际有的分辨率。

直连视频地址失败时，会使用环境变量里的 `HTTPS_PROXY` / `HTTP_PROXY`。未设置代理、且本机 `127.0.0.1:7897` 或 `7890` 在监听时，会自动改走该代理。也可以用 `-proxy http://127.0.0.1:7897` 指定。

## 网页

```text
xdl serve [-addr :8080] [-dir 安装目录] [-proxy 代理]
```

```text
xdl serve -addr :8080 -dir /opt/xdl
```

浏览器打开监听地址，粘贴链接并选择画质。视频保存在 `-dir` 指定的目录，页面上能看到下载进度和已经保存的文件。同一时间只进行一个下载。

## 构建

需要 Go 1.26 或更高版本。

```text
go build -o xdl .
```
