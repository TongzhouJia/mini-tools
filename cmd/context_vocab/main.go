// context_vocab —— 带上下文的单词本。
//
// 解决的问题：单独记一个词等于没记——不知道它在句子里怎么用、搭配什么介词、
// 什么语气。所以这里的最小单位不是「词」，而是「一整句 + 句里圈出来的那几个词」。
//
// 用法：把读到的句子粘进来，句子会自动切成一个个可点的词。
// 点词本身 = 收这一个单词；点词下面那根条 = 词组，连着点几根就把这几个词
// 连成一个词组（"look forward to"）。两种可以叠着用——同一个词既能单独收一遍，
// 又能作为词组的一部分再收一遍，互不影响。选完按「加入并翻译」，
// 配了 GOOGLE_TRANSLATE_API_KEY 就自动填中文（可改）。Enter 存盘。
//
// 存的是 JSONL（一行一条，data/context_vocab/entries.jsonl），
// 另外能导出两列的 CSV（英文,中文），跟 vocabulary_comparison / pdf_reader
// 那套 vocab.csv 格式通用。
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

const defaultPort = "8085"

var (
	dataDir   string
	port      string
	envPath   string
	transTo   string
	tasksList string
	reviewRaw string
	doImport  bool
	doMail    bool
	dryRun    bool
)

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

func main() {
	flag.StringVar(&dataDir, "data", envOr("CONTEXT_VOCAB_DATA_DIR", filepath.Join("data", "context_vocab")), "句子和单词存哪儿")
	flag.StringVar(&port, "port", envOr("CONTEXT_VOCAB_PORT", defaultPort), "监听端口")
	flag.StringVar(&envPath, "env", ".env", "从哪读 GOOGLE_TRANSLATE_API_KEY")
	flag.StringVar(&transTo, "to", "zh-CN", "自动翻译成哪种语言")
	flag.StringVar(&tasksList, "tasks-list", envOr("VOCAB_TASKS_LIST", defaultTasksList), "从哪个 Google Tasks 列表导词")
	flag.StringVar(&reviewRaw, "review-days", envOr("VOCAB_REVIEW_DAYS", defaultReviewDays), "复习间隔（天，逗号分隔）")
	flag.BoolVar(&doImport, "import", false, "从 Tasks 导一次词就退出，不起服务")
	flag.BoolVar(&doMail, "mail", false, "导入 + 发一封单词日报就退出（给定时器用）")
	flag.BoolVar(&dryRun, "dry-run", false, "配合 -mail：只把信打到终端，不真发")
	flag.Parse()

	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		log.Fatalf("❌ 建不了数据目录 %s：%v", dataDir, err)
	}
	if err := loadStore(); err != nil {
		log.Fatalf("❌ 读不了已有的句子：%v", err)
	}

	// Key 缺了不致命：句子和词照样存，中文列留空自己填
	initTranslate(envPath)

	// 一次性模式：给 systemd timer 用的，干完就退，不起服务
	if doImport || doMail {
		runOnce()
		return
	}

	http.HandleFunc("/", handleIndex)
	http.HandleFunc("/api/entries", logged("/api/entries", handleEntries))
	http.HandleFunc("/api/import-tasks", logged("/api/import-tasks", handleImportTasks))
	http.HandleFunc("/api/translate", logged("/api/translate", handleTranslate))
	http.HandleFunc("/api/export.csv", logged("/api/export.csv", handleExportCSV))

	fmt.Printf("📝 上下文单词本起来了：http://localhost:%s\n", port)
	fmt.Printf("   数据文件：%s（已有 %d 条句子）\n", entriesPath(), countEntries())
	fmt.Printf("   粘句子 → 点词（单词）/ 点词下面的条（词组）→ 加入并翻译 → Enter 存\n")
	fmt.Printf("   自动翻译 %s：%s\n", transTo, tick(translateKey != ""))
	fmt.Printf("   手机上记的词：点页面右上角「从 Task 导入」，拉「%s」列表\n", tasksList)

	if err := http.ListenAndServe("127.0.0.1:"+port, nil); err != nil {
		log.Fatalf("❌ 起不来（端口被占了？换 -port）：%v", err)
	}
}

// runOnce 是 -import / -mail 干的活：先从 Tasks 拉词，要发信再发一封日报。
// 导入失败不挡发信——词不全也比一天没信强，正文顶上会写清楚。
func runOnce() {
	var warn string
	res, err := importTasks(tasksList)
	if err != nil {
		warn = fmt.Sprintf("从「%s」导词失败：%v", tasksList, err)
		log.Print(warn)
	} else {
		fmt.Printf("从「%s」导词：%s\n", tasksList, res)
		for _, s := range res.Skipped {
			fmt.Printf("   跳过：%s\n", s)
		}
	}

	if !doMail {
		if err != nil {
			os.Exit(1)
		}
		return
	}

	subject, body := buildDigest(time.Now(), parseReviewDays(reviewRaw))
	if warn != "" {
		body = warn + "\n（所以这封信里的词可能不全）\n\n" + body
	}
	if dryRun {
		fmt.Printf("\n主题：%s\n\n%s", subject, body)
		return
	}
	if err := sendMail(subject, body); err != nil {
		log.Fatalf("日报没发出去：%v", err)
	}
	fmt.Printf("日报已发：%s\n", subject)
}

// handleImportTasks 网页上那个「从 Task 导入」按钮
func handleImportTasks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "要用 POST", http.StatusMethodNotAllowed)
		return
	}
	list := strings.TrimSpace(r.URL.Query().Get("list"))
	if list == "" {
		list = tasksList
	}
	res, err := importTasks(list)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, res)
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(indexHTML)
}

// handleEntries 一个端点管 增/查/改/删，按方法分。
func handleEntries(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, listEntries(r.URL.Query().Get("q")))

	case http.MethodPost, http.MethodPut:
		var e Entry
		if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
			http.Error(w, "请求体不是合法 JSON："+err.Error(), http.StatusBadRequest)
			return
		}
		saved, err := saveEntry(e)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, saved)

	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		if err := deleteEntry(id); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		writeJSON(w, map[string]string{"ok": id})

	default:
		http.Error(w, "不支持这个方法", http.StatusMethodNotAllowed)
	}
}

// handleExportCSV 导出两列 英文,中文，一个词只留最新的那条释义。
// 格式跟 vocabulary_comparison 的 vocab.csv 对得上，攒够了可以直接喂过去。
func handleExportCSV(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="context_vocab_%s.csv"`, time.Now().Format("20060102")))
	if err := exportCSV(w, r.URL.Query().Get("full") == "1"); err != nil {
		log.Printf("❌ 导出失败：%v", err)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

func tick(ok bool) string {
	if ok {
		return "✅"
	}
	return "❌ 没配 Key（中文列留空，手填）"
}

// logged 把 /api/* 的请求和返回码打到终端。浏览器那边报错经常只剩一句废话。
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(c int) {
	w.code = c
	w.ResponseWriter.WriteHeader(c)
}

func logged(name string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, code: 200}
		t0 := time.Now()
		h(sw, r)
		mark := "✅"
		if sw.code >= 400 {
			mark = "❌"
		}
		q := r.URL.RawQuery
		if len(q) > 80 {
			q = q[:80] + "…"
		}
		log.Printf("%s %s %s %d %s?%s", mark, r.Method, name, sw.code,
			time.Since(t0).Round(time.Millisecond), q)
	}
}
