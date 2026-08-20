// jp_reader —— 日语点读笔。
//
// 干什么用：把一段日语粘进来，自己拖选划分成一段一段，每段合成一次语音落盘；
// 之后每次点这一段，直接播缓存里的 MP3，不再打 API、也不再等。
//
// 跟「每次粘进谷歌翻译」的区别：文章、划分、音频全留在本地，
// 换一篇不会把上一篇冲掉，明天回来点还在。
//
// 页面上的动作：
//
//	拖选一段 → 回车     划成一段（自动合成）
//	点一段              播放
//	空格                重播当前段
//	↑ / ↓               上一段 / 下一段，并播放
//	r                   从当前段连读到底（再按一次停）
//	t                   给当前段翻译（中文显示在段下面）
//	Delete / Backspace  取消当前段的划分
package main

import (
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

//go:embed index.html
var indexHTML []byte

const defaultPort = "8086"

var (
	dataDir      string
	port         string
	envPath      string
	defaultVoice string
	defaultRate  float64
	transTo      string
)

func home() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return h
}

// defaultEnvPath 找放 API Key 的 .env。
// 这东西是天天开着用的，不该要求先 cd 进仓库才跑得起来：
// 当前目录有 .env 就用当前的，否则回落到仓库里那份（-env 能覆盖）。
func defaultEnvPath() string {
	if _, err := os.Stat(".env"); err == nil {
		return ".env"
	}
	return filepath.Join(home(), "go-projects", "mini-tools", ".env")
}

const usage = `📖 jp_reader —— 日语点读笔：粘一段日语，自己划段，点一下就念

用法：
  jp_reader                起在 :8086，浏览器打开 http://localhost:8086
  jp_reader -port 9000     换端口

网页里怎么用：
  粘进文章 → 拖选一段按回车把它划成一段 → 之后点那段就念
  嗓子和语速页面上能换，每篇文章各记各的

数据：
  默认 ~/.local/share/jp_reader（文章 + 音频缓存），用 -data 或 JP_READER_DATA_DIR 改
依赖：
  Google TTS / Translate 的 API Key，从 .env 读（默认 ~/go-projects/mini-tools/.env）
  Key 缺了不致命 —— 文章照样存得进去，只是合成和翻译会返回一句人话错误

参数：
`

func main() {
	flag.CommandLine.SetOutput(os.Stdout)
	flag.Usage = func() {
		fmt.Print(usage)
		flag.PrintDefaults()
	}
	flag.StringVar(&dataDir, "data", envOr("JP_READER_DATA_DIR", filepath.Join(home(), ".local", "share", "jp_reader")), "文章和音频缓存存哪儿")
	flag.StringVar(&port, "port", defaultPort, "监听端口")
	flag.StringVar(&envPath, "env", defaultEnvPath(), "从哪读 GOOGLE_TTS_API_KEY / GOOGLE_TRANSLATE_API_KEY")
	flag.StringVar(&defaultVoice, "voice", "ja-JP-Chirp3-HD-Achernar", "默认嗓子（页面上能换，每篇文章各记各的）")
	flag.Float64Var(&defaultRate, "rate", 1.0, "默认语速 0.25–2.0")
	flag.StringVar(&transTo, "to", "zh-CN", "翻译成哪种语言")
	flag.Parse()

	if err := os.MkdirAll(docsDir(), 0o755); err != nil {
		log.Fatalf("建不了数据目录 %s：%v", docsDir(), err)
	}

	http.HandleFunc("/", handleIndex)
	http.HandleFunc("/api/docs", logged("/api/docs", handleDocs))
	http.HandleFunc("/api/doc", logged("/api/doc", handleDoc))
	http.HandleFunc("/api/tts", logged("/api/tts", handleTTS))
	http.HandleFunc("/api/voices", logged("/api/voices", handleVoices))
	http.HandleFunc("/api/cached", logged("/api/cached", handleCached))
	http.HandleFunc("/api/translate", logged("/api/translate", handleTranslate))
	http.HandleFunc("/api/prewarm", logged("/api/prewarm", handlePrewarm))

	// Key 缺了不致命：文章照样存得进去，只有合成/翻译会返回一句人话错误
	initGCP(envPath)

	abs, _ := filepath.Abs(dataDir)
	fmt.Printf("日语点读笔起来了：http://localhost:%s\n", port)
	fmt.Printf("   数据目录：%s\n", abs)
	fmt.Printf("   默认嗓子：%s   语速 %.2f\n", defaultVoice, defaultRate)
	fmt.Printf("   合成 %s   翻译 %s   Key 读自 %s\n", tick(ttsKey != ""), tick(translateKey != ""), envPath)
	fmt.Printf("   用法：拖选一段按回车划成一段，之后点它就念\n")

	if err := http.ListenAndServe("127.0.0.1:"+port, nil); err != nil {
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

// ── 小工具 ────────────────────────────────────────────────────────────

// logged 把 /api/* 的请求和返回码打到终端。
// 浏览器那边的报错经常只剩一句没头没尾的话，看这儿才知道真相。
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(c int) {
	w.code = c
	w.ResponseWriter.WriteHeader(c)
}

// Flush 必须透传，否则包过一层之后 w.(http.Flusher) 断言就失败了，
// /api/prewarm 的 SSE 会直接报「这个连接不支持流式返回」
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func logged(name string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, code: 200}
		t0 := time.Now()
		h(sw, r)
		mark := "ok "
		if sw.code >= 400 {
			mark = "ERR"
		}
		q := r.URL.RawQuery
		if len(q) > 80 {
			q = q[:80] + "…"
		}
		log.Printf("%s %s %s %d %s?%s", mark, r.Method, name, sw.code,
			time.Since(t0).Round(time.Millisecond), q)
	}
}

func tick(ok bool) string {
	if ok {
		return "可用"
	}
	return "没配 Key"
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}
