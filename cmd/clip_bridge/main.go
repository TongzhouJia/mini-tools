// clip_bridge —— 手机和电脑之间互传文件与文字的本地网页工具。
package main

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

//go:embed index.html
var assets embed.FS

// Item 是一条剪贴内容：一段文字，或一个文件。
type Item struct {
	ID      string `json:"id"`
	Type    string `json:"type"` // "text" | "file"
	Text    string `json:"text,omitempty"`
	Name    string `json:"name,omitempty"`
	Size    int64  `json:"size,omitempty"`
	Mime    string `json:"mime,omitempty"`
	Created int64  `json:"created"` // Unix 秒
	From    string `json:"from"`    // 来源 IP，方便看是手机传的还是电脑传的
}

var (
	dataDir string
	mu      sync.Mutex
	items   []Item
)

const gmailAttachLimit = 25 << 20 // Gmail 附件上限 25MB

func main() {
	defaultData := filepath.Join(os.Getenv("HOME"), ".local", "share", "clip_bridge")
	if v := os.Getenv("CLIP_BRIDGE_DATA_DIR"); v != "" {
		defaultData = v
	}
	port := flag.Int("port", 8088, "监听端口")
	dir := flag.String("data", defaultData, "数据目录（文件和记录都存这里）")
	local := flag.Bool("local", false, "只监听 127.0.0.1（手机就连不上了，仅本机自用时才加）")
	mailTo := flag.String("to", "", "发邮件时的收件人，留空 = 发给自己")
	flag.Usage = usage
	flag.Parse()

	dataDir = *dir
	if err := os.MkdirAll(filepath.Join(dataDir, "files"), 0o755); err != nil {
		log.Fatalf("建不了数据目录 %s：%v", dataDir, err)
	}
	if err := loadItems(); err != nil {
		log.Fatalf("读不了记录：%v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/api/items", handleList)
	mux.HandleFunc("/api/text", handleText)
	mux.HandleFunc("/api/upload", handleUpload)
	mux.HandleFunc("/api/file/", handleFile)
	mux.HandleFunc("/api/delete", handleDelete)
	mux.HandleFunc("/api/mail", mailHandler(*mailTo))
	mux.HandleFunc("/api/qr", handleQR)

	host := "0.0.0.0"
	if *local {
		host = "127.0.0.1"
	}
	addr := fmt.Sprintf("%s:%d", host, *port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("端口 %d 起不来：%v", *port, err)
	}

	fmt.Println("clip_bridge 已启动")
	fmt.Printf("  本机：   http://127.0.0.1:%d\n", *port)
	if !*local {
		for _, ip := range lanIPs() {
			fmt.Printf("  局域网： http://%s:%d   （手机连同一个 Wi-Fi 就能开，页面上有二维码）\n", ip, *port)
		}
	}
	fmt.Printf("  数据：   %s\n", dataDir)
	fmt.Println("  Ctrl+C 退出")

	log.Fatal(http.Serve(ln, logged(mux)))
}

func usage() {
	fmt.Fprint(os.Stderr, `clip_bridge —— 手机和电脑互传文件/文字的网页中转站

干什么:
  起一个局域网网页，手机和电脑打开同一个地址，就能互相丢文件和文字。
  每条内容都能一键复制、下载，或者直接当邮件发给自己（走 gmail-send）。

怎么调:
  clip_bridge                起服务，默认 :8088，监听 0.0.0.0（手机能连）
  clip_bridge -port 9000     换端口
  clip_bridge -local         只监听 127.0.0.1（手机连不上，仅本机自用）
  clip_bridge -to a@b.com    发邮件时寄给别人，默认发给自己
  clip_bridge -data ~/xxx    换数据目录

手机怎么连:
  电脑上打开页面，点右上角「二维码」，手机扫。要在同一个 Wi-Fi 下。
  手机浏览器里可以直接选相册照片、拍照上传；文字框支持粘贴。

产物落哪:
  文件   $HOME/.local/share/clip_bridge/files/<id>_<原文件名>
  记录   $HOME/.local/share/clip_bridge/items.jsonl（一行一条）
  可用 CLIP_BRIDGE_DATA_DIR 环境变量或 -data 改。页面上删除 = 连文件一起删。

依赖什么:
  发邮件要 gmail-send（~/.local/bin/gmail-send）。没装也能用，只是邮件按钮会报错。
  其余全是标准库 + 一个二维码库，不需要 sudo。

有哪些坑:
  - 默认监听 0.0.0.0 且没有密码，同一局域网里谁都能开。在外面的网络（咖啡馆、
    公司网）别开着不管，用完 Ctrl+C。
  - 手机连的是 IP 不是域名，不受全局代理影响；连不上先确认两边在同一个 Wi-Fi。
  - Gmail 附件上限 25MB，超了邮件按钮会直接拒绝（文件本身照样能下载）。
  - 大文件是边收边落盘的，不占内存；但浏览器标签页别中途关。

`)
}

// ---------- 存储 ----------

func itemsPath() string { return filepath.Join(dataDir, "items.jsonl") }

func loadItems() error {
	b, err := os.ReadFile(itemsPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var it Item
		if err := json.Unmarshal([]byte(line), &it); err != nil {
			continue // 坏行跳过，别让整个库读不出来
		}
		items = append(items, it)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Created > items[j].Created })
	return nil
}

// 调用前必须持有 mu。
func saveItems() error {
	var sb strings.Builder
	for _, it := range items {
		b, err := json.Marshal(it)
		if err != nil {
			continue
		}
		sb.Write(b)
		sb.WriteByte('\n')
	}
	tmp := itemsPath() + ".tmp"
	if err := os.WriteFile(tmp, []byte(sb.String()), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, itemsPath())
}

// 调用前必须持有 mu。
func addItem(it Item) error {
	items = append([]Item{it}, items...)
	return saveItems()
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func findItem(id string) (Item, bool) {
	mu.Lock()
	defer mu.Unlock()
	for _, it := range items {
		if it.ID == id {
			return it, true
		}
	}
	return Item{}, false
}

func filePath(it Item) string {
	return filepath.Join(dataDir, "files", it.ID+"_"+safeName(it.Name))
}

// safeName 去掉路径分隔符，防止上传的文件名跑出 files 目录。
func safeName(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		name = "file"
	}
	if len(name) > 120 {
		ext := filepath.Ext(name)
		if len(ext) > 12 {
			ext = ""
		}
		name = name[:120-len(ext)] + ext
	}
	return name
}

// ---------- HTTP ----------

type flushWriter struct {
	http.ResponseWriter
	status int
}

func (w *flushWriter) WriteHeader(code int) { w.status = code; w.ResponseWriter.WriteHeader(code) }
func (w *flushWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func logged(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fw := &flushWriter{ResponseWriter: w, status: 200}
		h.ServeHTTP(fw, r)
		if strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/api/items" {
			log.Printf("%s %s %s -> %d", clientIP(r), r.Method, r.URL.Path, fw.status)
		}
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	b, err := assets.ReadFile("index.html")
	if err != nil {
		fail(w, 500, "页面丢了")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(b)
}

func handleList(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	out := make([]Item, len(items))
	copy(out, items)
	mu.Unlock()
	writeJSON(w, map[string]any{"items": out, "mailLimit": gmailAttachLimit})
}

func handleText(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, 405, "只收 POST")
		return
	}
	var req struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&req); err != nil {
		fail(w, 400, "解析不了请求："+err.Error())
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		fail(w, 400, "内容是空的")
		return
	}
	it := Item{ID: newID(), Type: "text", Text: req.Text, Created: time.Now().Unix(), From: clientIP(r)}
	mu.Lock()
	err := addItem(it)
	mu.Unlock()
	if err != nil {
		fail(w, 500, "存不下："+err.Error())
		return
	}
	writeJSON(w, it)
}

func handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, 405, "只收 POST")
		return
	}
	// 用 MultipartReader 边收边落盘，大文件不占内存。
	mr, err := r.MultipartReader()
	if err != nil {
		fail(w, 400, "不是 multipart 表单："+err.Error())
		return
	}
	var saved []Item
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			fail(w, 400, "读取上传流出错："+err.Error())
			return
		}
		if part.FileName() == "" {
			part.Close()
			continue
		}
		it := Item{
			ID:      newID(),
			Type:    "file",
			Name:    safeName(part.FileName()),
			Mime:    part.Header.Get("Content-Type"),
			Created: time.Now().Unix(),
			From:    clientIP(r),
		}
		dst := filePath(it)
		f, err := os.Create(dst)
		if err != nil {
			part.Close()
			fail(w, 500, "建不了文件："+err.Error())
			return
		}
		n, err := io.Copy(f, part)
		f.Close()
		part.Close()
		if err != nil {
			os.Remove(dst)
			fail(w, 500, "写文件出错："+err.Error())
			return
		}
		it.Size = n
		if it.Mime == "" {
			it.Mime = mime.TypeByExtension(strings.ToLower(filepath.Ext(it.Name)))
		}
		mu.Lock()
		err = addItem(it)
		mu.Unlock()
		if err != nil {
			fail(w, 500, "存不下："+err.Error())
			return
		}
		saved = append(saved, it)
	}
	if len(saved) == 0 {
		fail(w, 400, "没收到文件")
		return
	}
	writeJSON(w, map[string]any{"items": saved})
}

func handleFile(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/file/")
	it, ok := findItem(id)
	if !ok || it.Type != "file" {
		fail(w, 404, "没这条")
		return
	}
	f, err := os.Open(filePath(it))
	if err != nil {
		fail(w, 404, "文件不在了")
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	disp := "attachment"
	if r.URL.Query().Get("inline") == "1" {
		disp = "inline"
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("%s; filename*=UTF-8''%s", disp, urlEscape(it.Name)))
	if it.Mime != "" {
		w.Header().Set("Content-Type", it.Mime)
	}
	// 一律走 ServeContent：Content-Length / Range / 206 全自动。
	http.ServeContent(w, r, it.Name, st.ModTime(), f)
}

func urlEscape(s string) string {
	var sb strings.Builder
	for _, b := range []byte(s) {
		if (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || strings.IndexByte("-_.~", b) >= 0 {
			sb.WriteByte(b)
		} else {
			sb.WriteString("%" + strings.ToUpper(hex.EncodeToString([]byte{b})))
		}
	}
	return sb.String()
}

func handleDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, 405, "只收 POST")
		return
	}
	var req struct {
		ID  string `json:"id"`
		All bool   `json:"all"`
	}
	json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req)

	mu.Lock()
	defer mu.Unlock()
	var kept []Item
	var gone int
	for _, it := range items {
		if req.All || it.ID == req.ID {
			if it.Type == "file" {
				os.Remove(filePath(it))
			}
			gone++
			continue
		}
		kept = append(kept, it)
	}
	items = kept
	if err := saveItems(); err != nil {
		fail(w, 500, "存不下："+err.Error())
		return
	}
	writeJSON(w, map[string]any{"deleted": gone})
}

func mailHandler(defaultTo string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			fail(w, 405, "只收 POST")
			return
		}
		var req struct {
			ID   string `json:"id"`
			To   string `json:"to"`
			Note string `json:"note"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			fail(w, 400, "解析不了请求："+err.Error())
			return
		}
		it, ok := findItem(req.ID)
		if !ok {
			fail(w, 404, "没这条")
			return
		}
		bin, err := exec.LookPath("gmail-send")
		if err != nil {
			fail(w, 500, "找不到 gmail-send，装好再试")
			return
		}

		var subject, body string
		args := []string{}
		if it.Type == "text" {
			subject = firstLine(it.Text, 60)
			body = it.Text
		} else {
			if it.Size > gmailAttachLimit {
				fail(w, 400, fmt.Sprintf("文件 %s 有 %s，超过 Gmail 的 25MB 附件上限，发不了", it.Name, humanSize(it.Size)))
				return
			}
			subject = "文件：" + it.Name
			body = fmt.Sprintf("来自 clip_bridge\n文件名：%s\n大小：%s", it.Name, humanSize(it.Size))
			args = append(args, "-a", filePath(it))
		}
		if strings.TrimSpace(req.Note) != "" {
			body = req.Note + "\n\n" + body
		}
		to := req.To
		if to == "" {
			to = defaultTo
		}
		if to != "" {
			args = append(args, "-t", to)
		}
		cmd := exec.Command(bin, append([]string{subject, body}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			fail(w, 500, "发信失败："+strings.TrimSpace(string(out)))
			return
		}
		target := to
		if target == "" {
			target = "你自己"
		}
		writeJSON(w, map[string]string{"ok": "已发给 " + target})
	}
}

func handleQR(w http.ResponseWriter, r *http.Request) {
	data := r.URL.Query().Get("data")
	if data == "" {
		fail(w, 400, "没给内容")
		return
	}
	size := 320
	if v, err := strconv.Atoi(r.URL.Query().Get("size")); err == nil && v >= 100 && v <= 1000 {
		size = v
	}
	png, err := qrcode.Encode(data, qrcode.Medium, size)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Length", strconv.Itoa(len(png)))
	w.Write(png)
}

// ---------- 杂项 ----------

func firstLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	r := []rune(s)
	if len(r) > max {
		return string(r[:max]) + "…"
	}
	if len(r) == 0 {
		return "来自 clip_bridge 的文字"
	}
	return s
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}

// lanIPs 挑出真正能给手机用的地址：跳过回环、虚拟网桥和代理网卡。
func lanIPs() []string {
	var out []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		name := ifi.Name
		if strings.HasPrefix(name, "virbr") || strings.HasPrefix(name, "docker") ||
			strings.HasPrefix(name, "br-") || strings.HasPrefix(name, "veth") ||
			strings.HasPrefix(name, "tun") || strings.HasPrefix(name, "utun") ||
			strings.EqualFold(name, "Mihomo") {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipn.IP.To4()
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			if ip[0] == 198 && ip[1] == 18 { // Mihomo 的 fake-IP 段
				continue
			}
			out = append(out, ip.String())
		}
	}
	sort.Strings(out)
	return out
}
