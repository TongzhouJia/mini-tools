// pdf_reader —— 把本地 PDF 变成一个「正常网页」。
//
// 为什么需要它：Chrome/Brave 打开 file:// 的 PDF 时，用的是内置的 PDFium 阅读器，
// 内容渲染在一个沙箱化的 <embed> 里，页面上根本没有 DOM 文字节点。所以翻译插件、
// 划词插件、油猴脚本统统够不着——这跟「允许访问文件网址」那个开关没关系，开了也白搭。
//
// 这个工具用 Mozilla 官方的 pdf.js 在 http://localhost 上重新渲染 PDF。pdf.js 会铺一层
// 真实的 DOM 文字层（text layer），于是它就是一个普通网页：能选中、能划词、插件能注入。
//
// 在此之上内置了三个动作，选中文字后按单个字母触发：
//
//	a  存进错题本 CSV（带页码和上下文）
//	f  翻译（GCP Cloud Translation）
//	r  朗读（GCP Text-to-Speech）
//	d  诊断信息打到控制台
//
// 翻译和朗读的结果都按 sha1 存盘缓存，同一个词不会重复烧配额。
package main

import (
	"archive/zip"
	_ "embed"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed index.html
var indexHTML []byte

//go:embed capture.js
var captureJS []byte

//go:embed wrongbook.html
var wrongbookHTML []byte

const (
	defaultPort = "8084"
	// pdf.js 官方 release，走 GitHub 上游，不用任何镜像
	pdfjsRelease = "https://api.github.com/repos/mozilla/pdf.js/releases/latest"
	csvName      = "错题本.csv"
	walkMaxDepth = 5 // 扫 PDF 的最大层数，太深了慢
)

var csvHeader = []string{"时间", "文件", "页码", "原文", "上下文", "备注"}

var (
	rootDir   string
	pdfjsDir  string
	dataDir   string
	port      string
	envPath   string
	transTo   string
	voiceLang string
)

// 写 CSV 要串行，浏览器可能连点几下
var csvMu sync.Mutex

func home() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return h
}

func main() {
	flag.StringVar(&rootDir, "dir", home(), "扫哪个目录下的 PDF")
	flag.StringVar(&pdfjsDir, "pdfjs", envOr("PDFJS_DIR", filepath.Join(home(), ".local", "share", "pdfjs")), "pdf.js dist 解压后的目录（没有会自动从官方 release 下载）")
	flag.StringVar(&dataDir, "data", envOr("PDF_READER_DATA_DIR", filepath.Join("data", "pdf_reader")), "错题本 CSV 存哪儿")
	flag.StringVar(&port, "port", defaultPort, "监听端口")
	flag.StringVar(&envPath, "env", ".env", "从哪读 GOOGLE_TRANSLATE_API_KEY / GOOGLE_TTS_API_KEY")
	flag.StringVar(&transTo, "to", "zh-CN", "翻译成哪种语言")
	flag.StringVar(&voiceLang, "voice", "en-US", "朗读用哪种嗓子（选中的是中日韩会自动切 cmn-CN）")
	flag.Parse()

	abs, err := filepath.Abs(rootDir)
	if err != nil {
		log.Fatalf("❌ -dir 路径不对：%v", err)
	}
	rootDir = abs

	if err := ensurePDFJS(); err != nil {
		log.Fatalf("❌ pdf.js 没准备好：%v", err)
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		log.Fatalf("❌ 建不了数据目录 %s：%v", dataDir, err)
	}

	// Go 默认不认这几个扩展名，MIME 不对 pdf.js 会直接白屏
	must(mime.AddExtensionType(".mjs", "text/javascript"))
	must(mime.AddExtensionType(".wasm", "application/wasm"))
	must(mime.AddExtensionType(".bcmap", "application/octet-stream"))
	must(mime.AddExtensionType(".ftl", "text/plain; charset=utf-8"))

	http.HandleFunc("/", handleIndex)
	http.HandleFunc("/api/files", handleFiles)
	http.HandleFunc("/read", handleRead)
	http.HandleFunc("/capture.js", handleCaptureJS)
	http.HandleFunc("/wrongbook", handleWrongbookPage)
	http.HandleFunc("/api/wrong", handleWrong)
	http.HandleFunc("/api/highlights", handleHighlights)
	http.HandleFunc("/api/translate", handleTranslate)
	http.HandleFunc("/api/tts", handleTTS)
	http.Handle("/files/", http.StripPrefix("/files/", http.FileServer(http.Dir(rootDir))))
	http.HandleFunc("/pdfjs/", handlePDFJS)

	// Key 缺了不致命：错题本照样能用，只是翻译/朗读会返回一句人话错误
	initGCP(envPath)

	addr := "127.0.0.1:" + port
	fmt.Printf("📖 PDF 阅读器起来了：http://localhost:%s\n", port)
	fmt.Printf("   扫描目录：%s\n", rootDir)
	fmt.Printf("   错题本：  %s\n", filepath.Join(dataDir, csvName))
	fmt.Printf("   选中文字后：a 存错题 · f 翻译 · r 朗读 · d 诊断\n")
	fmt.Printf("   翻译 %s：%s   朗读 %s：%s\n",
		tick(translateKey != ""), transTo, tick(ttsKey != ""), voiceLang)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatalf("❌ 起不来（端口被占了？换 -port）：%v", err)
	}
}

func tick(ok bool) string {
	if ok {
		return "✅"
	}
	return "❌ 没配 Key"
}

func must(err error) {
	if err != nil {
		log.Fatalf("❌ %v", err)
	}
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// ── pdf.js 准备 ───────────────────────────────────────────────────────

// ensurePDFJS 检查 pdf.js 在不在，不在就从官方 release 下一份。
// 只认 mozilla/pdf.js 的 GitHub release，网络不通就报错退出，不做任何降级。
func ensurePDFJS() error {
	viewer := filepath.Join(pdfjsDir, "web", "viewer.html")
	if _, err := os.Stat(viewer); err == nil {
		return nil
	}

	fmt.Printf("📦 没找到 pdf.js（%s），从官方 release 下一份……\n", pdfjsDir)
	resp, err := http.Get(pdfjsRelease)
	if err != nil {
		return fmt.Errorf("查 release 失败（网络不通？）：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("查 release 返回 %s", resp.Status)
	}

	var rel struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return fmt.Errorf("解析 release 失败：%w", err)
	}

	var dl string
	for _, a := range rel.Assets {
		// legacy 那份是给老浏览器的，不要
		if strings.HasSuffix(a.Name, "-dist.zip") && !strings.Contains(a.Name, "legacy") {
			dl = a.URL
			break
		}
	}
	if dl == "" {
		return fmt.Errorf("release %s 里没找到 dist zip", rel.TagName)
	}

	fmt.Printf("⬇️  %s\n", dl)
	zr, err := http.Get(dl)
	if err != nil {
		return fmt.Errorf("下载失败：%w", err)
	}
	defer zr.Body.Close()
	if zr.StatusCode != http.StatusOK {
		return fmt.Errorf("下载返回 %s", zr.Status)
	}

	tmp, err := os.CreateTemp("", "pdfjs-*.zip")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, zr.Body); err != nil {
		tmp.Close()
		return fmt.Errorf("写临时文件失败：%w", err)
	}
	tmp.Close()

	if err := unzip(tmp.Name(), pdfjsDir); err != nil {
		return fmt.Errorf("解压失败：%w", err)
	}
	fmt.Printf("✅ pdf.js %s 装好了\n", rel.TagName)
	return nil
}

func unzip(src, dst string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer r.Close()

	for _, f := range r.File {
		// zip 里的路径不可信，挡一下 ../ 穿越
		target, err := safeJoin(dst, f.Name)
		if err != nil {
			return err
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(out, rc)
		out.Close()
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// safeJoin 把 rel 接到 base 下面，并保证结果没跑出 base
func safeJoin(base, rel string) (string, error) {
	target := filepath.Join(base, filepath.Clean("/"+rel))
	absBase, err := filepath.Abs(base)
	if err != nil {
		return "", err
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}
	if absTarget != absBase && !strings.HasPrefix(absTarget, absBase+string(os.PathSeparator)) {
		return "", fmt.Errorf("路径跑到 %s 外面去了：%s", base, rel)
	}
	return absTarget, nil
}

// ── 路由 ──────────────────────────────────────────────────────────────

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(indexHTML)
}

func handleCaptureJS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Write(captureJS)
}

func handleWrongbookPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(wrongbookHTML)
}

// handlePDFJS 伺服 pdf.js dist；viewer.html 要特殊处理——把 capture.js 塞进去。
// 走服务端注入而不是油猴脚本，是为了跟工具版本绑死，不用每次改完再去浏览器里粘一遍。
func handlePDFJS(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(r.URL.Path, "/pdfjs/")
	target, err := safeJoin(pdfjsDir, rel)
	if err != nil {
		http.Error(w, "路径不合法", http.StatusBadRequest)
		return
	}

	if strings.HasSuffix(rel, "web/viewer.html") {
		b, err := os.ReadFile(target)
		if err != nil {
			http.Error(w, "读不到 viewer.html", http.StatusNotFound)
			return
		}
		injected := strings.Replace(string(b), "</body>",
			`<script src="/capture.js"></script></body>`, 1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, injected)
		return
	}

	http.ServeFile(w, r, target)
}

// handleRead 把 /read?f=相对路径 转成 pdf.js viewer 的 URL
func handleRead(w http.ResponseWriter, r *http.Request) {
	f := r.URL.Query().Get("f")
	if f == "" {
		http.Error(w, "缺 f 参数", http.StatusBadRequest)
		return
	}
	if _, err := safeJoin(rootDir, f); err != nil {
		http.Error(w, "路径不合法", http.StatusBadRequest)
		return
	}
	src := "/files/" + strings.TrimPrefix(f, "/")
	// 空格必须编成 %20 —— QueryEscape 会编成 '+'，pdf.js 那边不保证还原得回空格，
	// 文件名带空格的（TOEIC 那些）就会 404
	dst := "/pdfjs/web/viewer.html?file=" + strings.ReplaceAll(url.QueryEscape(src), "+", "%20")
	http.Redirect(w, r, dst, http.StatusFound)
}

type pdfEntry struct {
	Name string `json:"name"`
	Rel  string `json:"rel"`
	Dir  string `json:"dir"`
	Size int64  `json:"size"`
}

func handleFiles(w http.ResponseWriter, r *http.Request) {
	var out []pdfEntry
	err := filepath.WalkDir(rootDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // 权限不够之类的，跳过就行，别整个中断
		}
		rel, rerr := filepath.Rel(rootDir, path)
		if rerr != nil {
			return nil
		}
		if d.IsDir() {
			if path == rootDir {
				return nil
			}
			// 隐藏目录和一眼就知道没 PDF 的地方直接剪掉，不然扫 home 会很慢
			name := d.Name()
			if strings.HasPrefix(name, ".") || name == "node_modules" || name == "snap" {
				return filepath.SkipDir
			}
			if strings.Count(rel, string(os.PathSeparator))+1 >= walkMaxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.EqualFold(filepath.Ext(path), ".pdf") {
			return nil
		}
		info, ierr := d.Info()
		var size int64
		if ierr == nil {
			size = info.Size()
		}
		out = append(out, pdfEntry{
			Name: d.Name(),
			Rel:  filepath.ToSlash(rel),
			Dir:  filepath.ToSlash(filepath.Dir(rel)),
			Size: size,
		})
		return nil
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Rel < out[j].Rel })

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(out)
}

// ── 错题本 ────────────────────────────────────────────────────────────

type wrongItem struct {
	Time    string `json:"time"`
	File    string `json:"file"`
	Page    string `json:"page"`
	Text    string `json:"text"`
	Context string `json:"context"`
	Note    string `json:"note"`
}

func handleWrong(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		items, err := readWrong()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(items)

	case http.MethodPost:
		var it wrongItem
		if err := json.NewDecoder(r.Body).Decode(&it); err != nil {
			http.Error(w, "请求体不对："+err.Error(), http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(it.Text) == "" {
			http.Error(w, "没选中任何文字", http.StatusBadRequest)
			return
		}
		it.Time = time.Now().Format("2006-01-02 15:04:05")
		n, err := appendWrong(it)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "total": n})

	default:
		http.Error(w, "只收 GET / POST", http.StatusMethodNotAllowed)
	}
}

func csvPath() string { return filepath.Join(dataDir, csvName) }

func appendWrong(it wrongItem) (int, error) {
	csvMu.Lock()
	defer csvMu.Unlock()

	path := csvPath()
	_, statErr := os.Stat(path)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	wcsv := csv.NewWriter(f)
	if os.IsNotExist(statErr) {
		// 头一次写：先来个 BOM，不然 Excel 打开中文是乱码
		if _, err := f.WriteString("\uFEFF"); err != nil {
			return 0, err
		}
		if err := wcsv.Write(csvHeader); err != nil {
			return 0, err
		}
	}
	if err := wcsv.Write([]string{it.Time, it.File, it.Page, it.Text, it.Context, it.Note}); err != nil {
		return 0, err
	}
	wcsv.Flush()
	if err := wcsv.Error(); err != nil {
		return 0, err
	}

	items, err := readWrongLocked()
	if err != nil {
		return 0, nil // 写成功了就算成功，数不出来无所谓
	}
	return len(items), nil
}

func readWrong() ([]wrongItem, error) {
	csvMu.Lock()
	defer csvMu.Unlock()
	return readWrongLocked()
}

func readWrongLocked() ([]wrongItem, error) {
	f, err := os.Open(csvPath())
	if err != nil {
		if os.IsNotExist(err) {
			return []wrongItem{}, nil
		}
		return nil, err
	}
	defer f.Close()

	rd := csv.NewReader(f)
	rd.FieldsPerRecord = -1
	rows, err := rd.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("错题本 CSV 读不动（是不是手改坏了？）：%w", err)
	}

	items := []wrongItem{}
	for i, row := range rows {
		if i == 0 {
			continue // 表头
		}
		get := func(n int) string {
			if n < len(row) {
				return row[n]
			}
			return ""
		}
		items = append(items, wrongItem{
			Time: get(0), File: get(1), Page: get(2),
			Text: get(3), Context: get(4), Note: get(5),
		})
	}
	return items, nil
}
