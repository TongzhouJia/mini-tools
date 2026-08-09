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
//	a  存进单词本（自动翻译，一天一个 dayNN.csv，两列：英文,中文）
//	s  高亮（再按一次取消）
//	r  朗读（GCP Text-to-Speech，默认澳洲口音）
//	f  翻译（GCP Cloud Translation）。备用，正常靠谷歌翻译插件选中自动弹
//	d  诊断信息打到控制台
//
// 翻译和朗读的结果都按 sha1 存盘缓存，同一个词不会重复烧配额。
package main

import (
	"archive/zip"
	_ "embed"
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
)

//go:embed index.html
var indexHTML []byte

//go:embed capture.js
var captureJS []byte

//go:embed words.html
var wordsHTML []byte

const (
	defaultPort = "8084"
	// pdf.js 官方 release，走 GitHub 上游，不用任何镜像
	pdfjsRelease = "https://api.github.com/repos/mozilla/pdf.js/releases/latest"
	walkMaxDepth = 5 // 扫 PDF 的最大层数，太深了慢
)

var (
	rootDir   string
	pdfjsDir  string
	dataDir   string
	port      string
	envPath   string
	transTo   string
	voiceLang string
	voiceName string
)

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
	flag.StringVar(&dataDir, "data", envOr("PDF_READER_DATA_DIR", filepath.Join("data", "pdf_reader")), "单词本 / 高亮 / 缓存存哪儿")
	flag.StringVar(&port, "port", defaultPort, "监听端口")
	flag.StringVar(&envPath, "env", ".env", "从哪读 GOOGLE_TRANSLATE_API_KEY / GOOGLE_TTS_API_KEY")
	flag.StringVar(&transTo, "to", "zh-CN", "翻译成哪种语言")
	flag.StringVar(&voiceLang, "voice", "en-AU", "朗读语种（选中的是中日韩会自动切 cmn-CN）")
	flag.StringVar(&voiceName, "voice-name", "en-AU-Chirp3-HD-Achernar", "具体哪把嗓子，留空让 Google 自己挑；`gcloud`/voices 接口能列全")
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
	http.HandleFunc("/words", handleWordsPage)
	http.HandleFunc("/api/words", handleWords)
	http.HandleFunc("/api/highlights", handleHighlights)
	http.HandleFunc("/api/translate", handleTranslate)
	http.HandleFunc("/api/tts", handleTTS)
	http.Handle("/files/", http.StripPrefix("/files/", http.FileServer(http.Dir(rootDir))))
	http.HandleFunc("/pdfjs/", handlePDFJS)

	// Key 缺了不致命：单词照样存得进去，中文列留空、翻译/朗读返回一句人话错误
	initGCP(envPath)

	addr := "127.0.0.1:" + port
	fmt.Printf("📖 PDF 阅读器起来了：http://localhost:%s\n", port)
	fmt.Printf("   扫描目录：%s\n", rootDir)
	fmt.Printf("   单词本：  %s\n", wordsDir())
	fmt.Printf("   选中文字后：a 存单词 · s 高亮 · r 朗读 · f 翻译 · d 诊断\n")
	fmt.Printf("   翻译 %s：%s   朗读 %s：%s %s\n",
		tick(translateKey != ""), transTo, tick(ttsKey != ""), voiceLang, voiceName)
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

func handleWordsPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(wordsHTML)
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
