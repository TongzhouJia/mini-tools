// jp_flash —— 日语过词：粘一张四列词表，空格/回车一个一个过。
//
// 仿 lexica（:8080）里过单词那个窗口，只留「过」这一件事：不打分、不存进度。
//
// 词表就是 ~/temp/日语 那些 CSV 的格式：日文,假名,中文,音频文件。
// 表头有没有都行，逗号或 Tab 分隔都行，第 4 列可以空。
//
// 两种过法：
//
//	看日语  先只显示日文 → 空格/回车：念出来 + 显示假名和中文 → 再按：下一个
//	先听    什么都不显示先念 → 空格/回车：显示日文、假名、中文 → 再按：下一个
//
// 发音：第 4 列有文件名就放 lexica 那份课本录音（data/japanese/audio）；
// 空着或找不到就拿 Google TTS 现合成（有假名念假名，没有念日文），合成过的落盘缓存。
package main

import (
	"crypto/sha1"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

//go:embed index.html
var indexHTML []byte

const (
	defaultPort = "8083"
	ttsAPI      = "https://texttospeech.googleapis.com/v1/text:synthesize"
	// 念的是单个词，这个上限只是挡住误传整篇文章
	maxTextLen = 300
)

var (
	audioDir string
	dataDir  string
	voice    string
	ttsKey   string
)

const usage = `jp_flash —— 日语过词：粘一张四列词表，空格/回车一个一个过

用法：
  jp_flash                 起在 :8083，浏览器打开 http://localhost:8083
  jp_flash -lan            同一个 Wi-Fi 下手机也能开（手机上点屏幕 = 按空格）

网页里怎么用：
  把词表粘进框里（日文,假名,中文,音频文件 —— 第 4 列可以空），选一种过法：
    看日语  先只显示日文，按空格/回车念出来并显示中文，再按下一个
    先听    先只念，按空格/回车显示日文和中文，再按下一个
  R 重念一遍，Esc 回到粘贴页。上次粘的词表会留在框里

发音从哪来：
  第 4 列有文件名 → 放 -audio 目录里那个 mp3（默认 lexica 的课本录音）
  空着或找不到    → Google TTS 现合成，缓存在 -data 目录，第二遍不再打 API

依赖：
  合成要 GOOGLE_TTS_API_KEY，从 .env 读（默认 ~/go-projects/mini-tools/.env）
  没 Key 也能起，只是没录音的词念不出来，页面上会写原因

参数：
`

func main() {
	flag.CommandLine.SetOutput(os.Stdout)
	flag.Usage = func() {
		fmt.Print(usage)
		flag.PrintDefaults()
	}
	port := flag.String("port", defaultPort, "监听端口")
	flag.StringVar(&audioDir, "audio", filepath.Join(home(), "go-projects", "lexica", "data", "japanese", "audio"), "第 4 列那些 mp3 在哪个目录")
	flag.StringVar(&dataDir, "data", envOr("JP_FLASH_DATA_DIR", filepath.Join(home(), ".local", "share", "jp_flash")), "合成音频的缓存存哪儿")
	envPath := flag.String("env", defaultEnvPath(), "从哪读 GOOGLE_TTS_API_KEY")
	flag.StringVar(&voice, "voice", "ja-JP-Chirp3-HD-Achernar", "没有录音的词用哪把嗓子合成")
	lan := flag.Bool("lan", false, "监听 0.0.0.0，同一个 Wi-Fi 下手机也能开（默认只有本机能开）")
	flag.Parse()

	loadEnv(*envPath)
	ttsKey = strings.TrimSpace(os.Getenv("GOOGLE_TTS_API_KEY"))

	http.HandleFunc("/", handleIndex)
	http.HandleFunc("/api/say", logged(handleSay))

	recs := "找不到这个目录，只能合成"
	if entries, err := os.ReadDir(audioDir); err == nil {
		recs = fmt.Sprintf("%d 个文件", len(entries))
	}
	synthState := "可用"
	if ttsKey == "" {
		synthState = "没配 Key（有录音的词照样能念）"
	}
	fmt.Printf("日语过词起来了：http://localhost:%s\n", *port)
	fmt.Printf("   课本录音：%s（%s）\n", audioDir, recs)
	fmt.Printf("   合成：%s   嗓子 %s   缓存 %s\n", synthState, voice, filepath.Join(dataDir, "tts"))

	host := "127.0.0.1"
	if *lan {
		host = "0.0.0.0"
	}
	if err := http.ListenAndServe(host+":"+*port, nil); err != nil {
		log.Fatalf("起不来（端口被占了？换 -port）：%v", err)
	}
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(indexHTML)
}

// GET /api/say?file=<第 4 列>&text=<要念的字>
// 先找课本录音，没有再合成。
func handleSay(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if p, ok := recording(q.Get("file")); ok {
		serveMP3(w, r, p)
		return
	}
	text := strings.TrimSpace(q.Get("text"))
	if text == "" {
		http.Error(w, "没有录音，也没有能念的字", http.StatusBadRequest)
		return
	}
	if len(text) > maxTextLen {
		http.Error(w, "要念的字太长了，这不像一个词", http.StatusBadRequest)
		return
	}
	p, err := synth(text)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	serveMP3(w, r, p)
}

// recording 找第 4 列写的那个录音文件。只认文件名，带目录的一律不认，免得 ../ 读到别处。
func recording(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" || name != filepath.Base(name) || strings.HasPrefix(name, ".") {
		return "", false
	}
	for _, n := range []string{name, name + ".mp3"} {
		p := filepath.Join(audioDir, n)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, true
		}
	}
	return "", false
}

// synth 返回这个词的 MP3 缓存路径，没有就打一次 API 再落盘。缓存 key 带上嗓子，换嗓子不串。
func synth(text string) (string, error) {
	sum := sha1.Sum([]byte(text + "|" + voice))
	ck := filepath.Join(dataDir, "tts", hex.EncodeToString(sum[:])+".mp3")
	if fi, err := os.Stat(ck); err == nil && fi.Size() > 0 {
		return ck, nil
	}
	if ttsKey == "" {
		return "", fmt.Errorf("这个词没有录音，合成又没配 GOOGLE_TTS_API_KEY（放进 ~/go-projects/mini-tools/.env）")
	}

	reqBody, _ := json.Marshal(map[string]any{
		"input":       map[string]string{"text": text},
		"voice":       map[string]string{"languageCode": "ja-JP", "name": voice},
		"audioConfig": map[string]string{"audioEncoding": "MP3"},
	})
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Post(ttsAPI+"?key="+url.QueryEscape(ttsKey), "application/json", strings.NewReader(string(reqBody)))
	if err != nil {
		return "", fmt.Errorf("合成请求失败（网络？）：%w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("TTS API 返回 %s：%s", resp.Status, squash(body))
	}
	var parsed struct {
		AudioContent string `json:"audioContent"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.AudioContent == "" {
		return "", fmt.Errorf("TTS 结果看不懂：%s", squash(body))
	}
	mp3, err := base64.StdEncoding.DecodeString(parsed.AudioContent)
	if err != nil {
		return "", fmt.Errorf("音频解码失败：%w", err)
	}
	if err := os.MkdirAll(filepath.Dir(ck), 0o755); err != nil {
		return "", fmt.Errorf("建不了缓存目录：%w", err)
	}
	if err := os.WriteFile(ck, mp3, 0o644); err != nil {
		return "", fmt.Errorf("写不了缓存：%w", err)
	}
	return ck, nil
}

// serveMP3 一律用 http.ServeContent 吐音频，别自己 w.Write：
// 自己写会走 chunked、不带 Content-Length，Range 请求也不回 206，<audio> 直接报 no supported source。
func serveMP3(w http.ResponseWriter, r *http.Request, path string) {
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "音频读不到："+err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		http.Error(w, "音频看不了："+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "audio/mpeg")
	http.ServeContent(w, r, filepath.Base(path), fi.ModTime(), f)
}

// ── 小工具 ────────────────────────────────────────────────────────────

// logged 把 /api/* 的请求和返回码打到终端：浏览器那边的报错常常只剩一句废话，看这儿才知道真相。
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(c int) {
	w.code = c
	w.ResponseWriter.WriteHeader(c)
}

func logged(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, code: 200}
		t0 := time.Now()
		h(sw, r)
		mark := "ok "
		if sw.code >= 400 {
			mark = "ERR"
		}
		q, _ := url.QueryUnescape(r.URL.RawQuery)
		log.Printf("%s %s %d %s %s", mark, r.URL.Path, sw.code, time.Since(t0).Round(time.Millisecond), q)
	}
}

// loadEnv 手动读 .env，格式就是 KEY=VALUE，# 开头当注释；已有的环境变量优先
func loadEnv(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		if os.Getenv(k) == "" {
			os.Setenv(k, strings.Trim(strings.TrimSpace(v), `"'`))
		}
	}
}

// defaultEnvPath 当前目录有 .env 就用当前的，否则回落到仓库里那份 —— 在哪个目录起都行
func defaultEnvPath() string {
	if _, err := os.Stat(".env"); err == nil {
		return ".env"
	}
	return filepath.Join(home(), "go-projects", "mini-tools", ".env")
}

func home() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return h
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func squash(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
