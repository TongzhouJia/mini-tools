// en_drill —— 英语单词自测：听 In Our Time 的原句，回想那个词。
//
// 干什么用：把 ~/Desktop/学外语/学习材料 里的单词表读进来，
// 每个词回到它在播客里被说出来的那一秒，直接放那 3 秒原声当提示。
//
// 跟「盯着中英对照表背」的区别有两条：
//
//	一、先砍表。2813 条里有 2/3 是只在某一集出现过一次的学科名词
//	    （helium 氦、lipid 脂质、endosymbiont 内共生体）——
//	    中文概念早就有了，缺的只是个标签，这辈子也不会再遇上第二次。
//	    默认只刷「跨集复现 ∪ 全部短语」那 774 个。
//
//	二、考法跟着词性走，不搞一刀切：
//	    phr.  中文 → 想英文（成语性短语的中文释义几乎是唯一解，反着考才有信息量）
//	    有原句 听原句 → 想意思（练的正好是听懂播客那个技能）
//	    剩下  英文 → 想中文
//
// 页面上的动作：
//
//	空格 / 回车   翻面
//	1 / 2 / 3     忘了 / 勉强 / 会
//	r             重播原句
//	s             跳过这张
package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

//go:embed index.html
var assets embed.FS

const defaultPort = "8087"

var (
	deck  *Deck
	store *Store
)

func home() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return h
}

const usage = `en_drill —— 英语单词自测：听原句、回想、自评

用法：
  en_drill                 起在 :8087，浏览器打开 http://localhost:8087
  en_drill -tier all       连只出现过一次的学科名词一起刷（默认只刷核心）
  en_drill -limit 40       一轮最多几张（默认 60）
  en_drill -new 10         一轮最多放几个没见过的新词（默认 20）

网页里怎么用：
  空格翻面，1/2/3 评「忘了 / 勉强 / 会」，r 重播原句，s 跳过

数据：
  单词表读 ~/Desktop/学外语/学习材料/*单词表.csv（-src 改）
  原声读 ~/Desktop/学外语/*.mp3 + 同名 .srt（-media 改）—— 只读不动
  进度写 ~/.local/share/en_drill/progress.json（-data 改）
  主键是折平后的单词，以后再加几集单词表，老词进度接得上

参数：
`

func main() {
	var srcDir, mediaDir, dataDir, port, tier string
	var limit, fresh int

	flag.CommandLine.SetOutput(os.Stdout)
	flag.Usage = func() { fmt.Print(usage); flag.PrintDefaults() }
	flag.StringVar(&srcDir, "src", filepath.Join(home(), "Desktop", "学外语", "学习材料"), "单词表 csv 在哪")
	flag.StringVar(&mediaDir, "media", filepath.Join(home(), "Desktop", "学外语"), "mp3 和 srt 在哪")
	flag.StringVar(&dataDir, "data", envOr("EN_DRILL_DATA_DIR", filepath.Join(home(), ".local", "share", "en_drill")), "进度存哪儿")
	flag.StringVar(&port, "port", defaultPort, "监听端口")
	flag.StringVar(&tier, "tier", "core", "刷哪一档：core（跨集复现+全部短语）/ extra（只出现一次的）/ all")
	flag.IntVar(&limit, "limit", 60, "一轮最多几张")
	flag.IntVar(&fresh, "new", 20, "一轮最多放几个新词")
	var lan bool
	flag.BoolVar(&lan, "lan", false, "监听 0.0.0.0，同一个 Wi-Fi 下手机也能开（默认只有本机能开）")
	flag.Parse()

	var err error
	if deck, err = loadDeck(srcDir, mediaDir); err != nil {
		log.Fatalf("单词表读不进来（-src 指对了吗）：%v", err)
	}
	if len(deck.Cards) == 0 {
		log.Fatalf("在 %s 里一张卡都没读到，检查下 -src", srcDir)
	}
	if store, err = openStore(dataDir); err != nil {
		log.Fatalf("进度存档打不开 %s：%v", dataDir, err)
	}

	http.HandleFunc("/", handleIndex)
	http.HandleFunc("/api/session", logged("/api/session", func(w http.ResponseWriter, r *http.Request) {
		t := qs(r, "tier", tier)
		writeJSON(w, map[string]any{
			"items": store.session(deck, t, qsInt(r, "limit", limit), qsInt(r, "new", fresh)),
			"stats": store.stats(deck, t),
			"eps":   deck.Eps,
			"tier":  t,
		})
	}))
	http.HandleFunc("/api/grade", logged("/api/grade", handleGrade))
	http.HandleFunc("/audio/", logged("/audio/", handleAudio))

	st := store.stats(deck, tier)
	withAudio := 0
	for _, c := range deck.Cards {
		if c.Tier == tier && len(c.Hits) > 0 {
			withAudio++
		}
	}
	fmt.Printf("英语单词自测起来了：http://localhost:%s\n", port)
	fmt.Printf("   单词表：%s\n", srcDir)
	fmt.Printf("   共 %d 个词，核心 %d / 边角 %d，其中 %d 个挂上了播客原声\n", st.Total, st.Core, st.Extra, st.Audio)
	fmt.Printf("   这一档（%s）：今天到期 %d，还没见过 %d\n", tier, st.Due, st.Fresh)
	fmt.Printf("   进度：%s\n", filepath.Join(dataDir, "progress.json"))

	host := "127.0.0.1"
	if lan {
		host = "0.0.0.0"
	}
	if err := http.ListenAndServe(host+":"+port, nil); err != nil {
		log.Fatalf("起不来（端口被占了？换 -port）：%v", err)
	}
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	b, _ := assets.ReadFile("index.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(b)
}

func handleGrade(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     string `json:"id"`
		Rating int    `json:"rating"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
		http.Error(w, "请求里没带 id", http.StatusBadRequest)
		return
	}
	writeJSON(w, store.grade(req.ID, req.Rating))
}

// handleAudio 直接把整集 mp3 交给 ServeContent，让浏览器自己发 Range 请求。
// 前端只管 currentTime 跳到那一秒、到点暂停 —— 不预切 2000 个片段，
// 不占磁盘，也不用等一遍 ffmpeg。
// （pdf_reader 那边踩过：不走 ServeContent 的话 audio 元素直接不认。）
func handleAudio(w http.ResponseWriter, r *http.Request) {
	code := strings.TrimPrefix(r.URL.Path, "/audio/")
	ep := deck.Eps[code]
	if ep == nil || ep.MP3 == "" {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(ep.MP3)
	if err != nil {
		http.Error(w, "音频打不开", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		http.Error(w, "音频读不了", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "audio/mpeg")
	http.ServeContent(w, r, filepath.Base(ep.MP3), fi.ModTime(), f)
}

// ── 小工具 ────────────────────────────────────────────────────────────

func qs(r *http.Request, k, def string) string {
	if v := strings.TrimSpace(r.URL.Query().Get(k)); v != "" {
		return v
	}
	return def
}

func qsInt(r *http.Request, k string, def int) int {
	if n, err := strconv.Atoi(r.URL.Query().Get(k)); err == nil && n > 0 {
		return n
	}
	return def
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

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(c int) { w.code = c; w.ResponseWriter.WriteHeader(c) }

func logged(name string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, code: 200}
		t0 := time.Now()
		h(sw, r)
		mark := "ok "
		if sw.code >= 400 {
			mark = "ERR"
		}
		log.Printf("%s %s %s %d %s", mark, r.Method, name, sw.code, time.Since(t0).Round(time.Millisecond))
	}
}
