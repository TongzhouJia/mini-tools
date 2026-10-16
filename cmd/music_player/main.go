// music_player —— 局域网点歌台：手机上点歌，这台电脑用 mpv 放。
// 一首放完就停，不自动下一首；开了单曲循环就一直放这一首。
// 2026-08 写的，09-01 删过一次；10-16 他要在「语音剪贴板」App 左上角点歌，捞回来加了暂停和单曲循环。
package main

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"
)

//go:embed index.html
var indexHTML []byte

var (
	musicDir string
	mpvBin   string
)

// B 站下的 DJ 版是 .flv（带画面），mpv --no-video 只出声，所以视频格式也算歌。
var audioExts = map[string]bool{
	".flac": true, ".mp3": true, ".m4a": true, ".wav": true, ".aac": true, ".ogg": true,
	".opus": true, ".aiff": true, ".aif": true,
	".flv": true, ".mp4": true, ".webm": true, ".mkv": true,
}

// Song 是音乐目录里的一个文件。File 是相对音乐目录的路径，点歌就传它。
type Song struct {
	Name string `json:"name"` // 去掉扩展名，一般是「歌名 - 歌手」
	File string `json:"file"`
}

// ---------- 播放 ----------

// proc 是正在放的那个 mpv。
type proc struct {
	cmd  *exec.Cmd
	sock string // mpv 的控制口，暂停 / 单曲循环都走它
	song Song
	done chan struct{}
}

type player struct {
	mu   sync.Mutex
	cur  *proc
	loop bool // 单曲循环。是个开关：开着的时候换歌也照样循环
	seq  int
}

var p player

func (p *player) play(s Song) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopLocked()

	p.seq++
	sock := filepath.Join(runtimeDir(), fmt.Sprintf("music_player-%d-%d.sock", os.Getpid(), p.seq))
	// --no-config：不吃 ~/.config/mpv 里的配置和脚本，哪天那边加了 loop / keep-open 也不会变成连播。
	// 不加 --loop / --playlist，一个文件放完 mpv 自己就退了，这就是「一首放完就停」。
	args := []string{"--no-config", "--no-video", "--no-terminal",
		"--audio-client-name=点歌台", "--input-ipc-server=" + sock}
	if p.loop {
		args = append(args, "--loop-file=inf")
	}
	args = append(args, "--", filepath.Join(musicDir, s.File))
	cmd := exec.Command(mpvBin, args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	pr := &proc{cmd: cmd, sock: sock, song: s, done: make(chan struct{})}
	p.cur = pr
	log.Printf("正在放：%s", s.Name)

	go func() {
		err := cmd.Wait()
		os.Remove(sock)
		close(pr.done) // 先关 done 再拿锁：stopLocked 拿着锁在等它
		p.mu.Lock()
		if p.cur == pr {
			p.cur = nil
			if err != nil {
				log.Printf("放不下去了：%s（%v）", s.Name, err)
			} else {
				log.Printf("放完了：%s", s.Name)
			}
		}
		p.mu.Unlock()
	}()
	return nil
}

// 调用前必须持有 p.mu。
func (p *player) stopLocked() {
	pr := p.cur
	if pr == nil {
		return
	}
	p.cur = nil
	pr.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-pr.done:
	case <-time.After(2 * time.Second):
		pr.cmd.Process.Kill()
		<-pr.done
	}
	log.Printf("停了：%s", pr.song.Name)
}

func (p *player) stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopLocked()
}

func (p *player) setPause(pause bool) error {
	p.mu.Lock()
	pr := p.cur
	p.mu.Unlock()
	if pr == nil {
		return errNotPlaying
	}
	_, err := ipc(pr.sock, "set_property", "pause", pause)
	return err
}

func (p *player) setLoop(loop bool) error {
	p.mu.Lock()
	p.loop = loop
	pr := p.cur
	p.mu.Unlock()
	if pr == nil {
		return nil
	}
	v := "no"
	if loop {
		v = "inf"
	}
	_, err := ipc(pr.sock, "set_property", "loop-file", v)
	return err
}

type status struct {
	Song   *Song `json:"song"` // 正在放的，null = 没在放
	Paused bool  `json:"paused"`
	Loop   bool  `json:"loop"`
}

func (p *player) status() status {
	p.mu.Lock()
	pr, loop := p.cur, p.loop
	p.mu.Unlock()
	st := status{Loop: loop}
	if pr == nil {
		return st
	}
	s := pr.song
	st.Song = &s
	// 暂停状态每次问 mpv 本人，不自己记，免得两边对不上
	if v, err := ipc(pr.sock, "get_property", "pause"); err == nil {
		st.Paused, _ = v.(bool)
	}
	return st
}

var errNotPlaying = errors.New("现在没在放歌")

// ipc 往 mpv 的控制口发一条命令，等它的回话。控制口上还会混着 mpv 自己广播的事件，跳过。
func ipc(sock string, cmd ...any) (any, error) {
	c, err := net.DialTimeout("unix", sock, time.Second)
	if err != nil {
		return nil, fmt.Errorf("连不上 mpv：%w", err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	req, _ := json.Marshal(map[string]any{"command": cmd, "request_id": 1})
	if _, err := c.Write(append(req, '\n')); err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(c)
	for sc.Scan() {
		var resp struct {
			Error     string `json:"error"`
			Data      any    `json:"data"`
			RequestID *int   `json:"request_id"`
		}
		if json.Unmarshal(sc.Bytes(), &resp) != nil || resp.RequestID == nil {
			continue
		}
		if resp.Error != "success" {
			return nil, fmt.Errorf("mpv 说：%s", resp.Error)
		}
		return resp.Data, nil
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, io.ErrUnexpectedEOF
}

func runtimeDir() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return d
	}
	return os.TempDir()
}

// ---------- 歌单 ----------

var zh = collate.New(language.Chinese) // 按拼音排

func scanSongs() []Song {
	var songs []Song
	filepath.WalkDir(musicDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !audioExts[strings.ToLower(filepath.Ext(path))] {
			return nil // 单个条目出错就跳过，继续扫
		}
		rel, err := filepath.Rel(musicDir, path)
		if err != nil {
			return nil
		}
		songs = append(songs, Song{Name: strings.TrimSuffix(d.Name(), filepath.Ext(d.Name())), File: rel})
		return nil
	})
	names := make([]string, len(songs))
	idx := map[string]Song{}
	for i, s := range songs {
		names[i] = s.Name + "\x00" + s.File // 同名不同格式的别并成一首
		idx[names[i]] = s
	}
	zh.SortStrings(names)
	for i, n := range names {
		songs[i] = idx[n]
	}
	return songs
}

// findSong 只认音乐目录里真实存在、扩展名对得上的文件，别让人点着点着放了别处的东西。
func findSong(file string) (Song, bool) {
	clean := filepath.Clean("/" + file)[1:]
	if clean == "" || !audioExts[strings.ToLower(filepath.Ext(clean))] {
		return Song{}, false
	}
	st, err := os.Stat(filepath.Join(musicDir, clean))
	if err != nil || st.IsDir() {
		return Song{}, false
	}
	base := filepath.Base(clean)
	return Song{Name: strings.TrimSuffix(base, filepath.Ext(base)), File: clean}, true
}

// ---------- HTTP ----------

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// readBody 解析 POST 的 JSON。
func readBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Method != http.MethodPost {
		fail(w, 405, "只收 POST")
		return false
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(v); err != nil {
		fail(w, 400, "解析不了请求："+err.Error())
		return false
	}
	return true
}

func handleSongs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"songs": scanSongs(), "status": p.status()})
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, p.status())
}

func handlePlay(w http.ResponseWriter, r *http.Request) {
	var req struct {
		File string `json:"file"`
	}
	if !readBody(w, r, &req) {
		return
	}
	s, ok := findSong(req.File)
	if !ok {
		fail(w, 404, "音乐目录里没这首："+req.File)
		return
	}
	if err := p.play(s); err != nil {
		fail(w, 500, "放不了："+err.Error())
		return
	}
	writeJSON(w, p.status())
}

func handlePause(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Pause bool `json:"pause"`
	}
	if !readBody(w, r, &req) {
		return
	}
	if err := p.setPause(req.Pause); err != nil {
		code := 500
		if errors.Is(err, errNotPlaying) {
			code = 409
		}
		fail(w, code, err.Error())
		return
	}
	writeJSON(w, p.status())
}

func handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, 405, "只收 POST")
		return
	}
	p.stop()
	writeJSON(w, p.status())
}

func handleLoop(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Loop bool `json:"loop"`
	}
	if !readBody(w, r, &req) {
		return
	}
	if err := p.setLoop(req.Loop); err != nil {
		fail(w, 500, "开关单曲循环出错："+err.Error())
		return
	}
	writeJSON(w, p.status())
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(c int) { w.code = c; w.ResponseWriter.WriteHeader(c) }

// logged 记下点歌、暂停这些操作（状态轮询太多，不记）。
func logged(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, code: 200}
		h.ServeHTTP(sw, r)
		if r.Method == http.MethodPost {
			log.Printf("%s %s -> %d", r.RemoteAddr, r.URL.Path, sw.code)
		}
	})
}

// cors 放行同一台电脑上别的端口的页面：「语音剪贴板」App 的页面挂在 voice_clip（:8092）的源上，
// 调这边算跨域。只认主机名跟本次请求 Host 一样的 Origin，外面的网站照样调不动。
func cors(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Origin")
		if o := r.Header.Get("Origin"); o != "" && sameHost(o, r.Host) {
			w.Header().Set("Access-Control-Allow-Origin", o)
			if r.Method == http.MethodOptions { // POST JSON 会先来这一下预检
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}

func sameHost(origin, host string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return u.Hostname() != "" && u.Hostname() == strings.Trim(host, "[]")
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(indexHTML)
}

func usage() {
	fmt.Fprint(os.Stderr, `music_player —— 局域网点歌台：手机上点歌，声音从这台电脑出

干什么:
  列出音乐目录里的歌（按拼音排），点一首就用 mpv 在这台电脑上放。
  一首放完就停，不会自动下一首；开了「单曲循环」就一直放这一首（换歌也照样循环）。
  能暂停 / 接着放 / 停止。手机上的「语音剪贴板」App 左上角「点歌」就是调它。

怎么调:
  music_player                起服务，默认 :8082，监听 0.0.0.0（手机能连）
  music_player -dir ~/xxx     换音乐目录（默认 ~/Music，子目录也会扫）
  music_player -port 9000     换端口
  music_player -local         只监听 127.0.0.1（手机就连不上了）
  电脑上打开 http://127.0.0.1:8082 也能点。

接口（App 用的）:
  GET  /api/songs    歌单 + 现在的状态
  GET  /api/status   {"song": 正在放的或 null, "paused": 是否暂停, "loop": 单曲循环开没开}
  POST /api/play     {"file": "庐州月 - 许嵩.flac"}（相对音乐目录）
  POST /api/pause    {"pause": true / false}
  POST /api/stop
  POST /api/loop     {"loop": true / false}

依赖什么:
  mpv（/usr/bin/mpv）。不带用户配置跑（--no-config），~/.config/mpv 里改什么都不影响这里。
  支持 flac/mp3/m4a/wav/aac/ogg/opus，还有 B 站下的 flv/mp4/webm/mkv（只出声）。

有哪些坑:
  - 同一时刻只放一首，点新歌会先把正在放的停掉。
  - 服务重启 = 歌停、单曲循环回到关。
  - 监听 0.0.0.0 没有密码，同一个局域网谁都能点歌（只能放这个目录里的歌）。
  - 三指轻点暂停管不到它（它没走 MPRIS），暂停在手机上点。

`)
}

func main() {
	home, _ := os.UserHomeDir()
	dir := flag.String("dir", filepath.Join(home, "Music"), "音乐目录")
	port := flag.Int("port", 8082, "监听端口")
	local := flag.Bool("local", false, "只监听 127.0.0.1（手机就连不上了）")
	flag.Usage = usage
	flag.Parse()

	abs, err := filepath.Abs(*dir)
	if err != nil {
		log.Fatalf("音乐目录不对：%s", *dir)
	}
	musicDir = abs
	if st, err := os.Stat(musicDir); err != nil || !st.IsDir() {
		log.Fatalf("音乐目录不存在：%s", musicDir)
	}
	if mpvBin, err = exec.LookPath("mpv"); err != nil {
		log.Fatal("找不到 mpv，先装：sudo apt install mpv")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/api/songs", handleSongs)
	mux.HandleFunc("/api/status", handleStatus)
	mux.HandleFunc("/api/play", handlePlay)
	mux.HandleFunc("/api/pause", handlePause)
	mux.HandleFunc("/api/stop", handleStop)
	mux.HandleFunc("/api/loop", handleLoop)

	host := "0.0.0.0"
	if *local {
		host = "127.0.0.1"
	}
	// 明着听 IPv4，免得只开了 IPv6 的口子手机连不上
	ln, err := net.Listen("tcp4", fmt.Sprintf("%s:%d", host, *port))
	if err != nil {
		log.Fatalf("端口 %d 起不来：%v", *port, err)
	}
	fmt.Printf("点歌台已启动，%d 首歌（%s）\n", len(scanSongs()), musicDir)
	fmt.Printf("  本机：http://127.0.0.1:%d\n", *port)
	log.Fatal(http.Serve(ln, cors(logged(mux))))
}
