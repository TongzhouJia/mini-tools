// voice_clip —— 手机上用输入法的语音输入说一段话，点一下就进电脑剪贴板。
//
// 思路：语音识别交给手机输入法（它有系统级麦克风权限，识别质量也最好），
// 网页只负责把识别出来的文字搬到电脑。网页全程不碰麦克风，所以不需要 HTTPS，
// 局域网 http:// 直接能用。
package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

//go:embed index.html
var assets embed.FS

// Item 是一次语音输入的结果。
type Item struct {
	ID      string `json:"id"`
	Text    string `json:"text"`
	Created int64  `json:"created"` // Unix 秒
	From    string `json:"from"`    // 来源 IP，方便看是哪台设备发的
}

const maxItems = 200 // 历史只留这么多条，够回头找就行

// defaultTaskList 是右上角「+」默认写进哪个 Google Tasks 列表。
// 前端选过别的会记在 localStorage 里，以那个为准。
var defaultTaskList = "My Tasks"

var (
	dataDir string
	mu      sync.Mutex
	items   []Item
)

func main() {
	defaultData := filepath.Join(os.Getenv("HOME"), ".local", "share", "voice_clip")
	if v := os.Getenv("VOICE_CLIP_DATA_DIR"); v != "" {
		defaultData = v
	}
	port := flag.Int("port", 8092, "监听端口")
	dir := flag.String("data", defaultData, "数据目录（历史记录存这里）")
	local := flag.Bool("local", false, "只监听 127.0.0.1（手机就连不上了，仅本机自用时才加）")
	noNotify := flag.Bool("no-notify", false, "收到文字后不弹桌面通知")
	taskList := flag.String("task-list", defaultTaskList, "右上角「+」默认写进哪个 Google Tasks 列表")
	flag.Usage = usage
	flag.Parse()

	dataDir = *dir
	defaultTaskList = *taskList
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		log.Fatalf("建不了数据目录 %s：%v", dataDir, err)
	}
	if err := loadItems(); err != nil {
		log.Fatalf("读不了历史记录：%v", err)
	}

	// 开跑前先确认剪贴板工具在，别等手机说完一段话才发现写不进去。
	if _, err := exec.LookPath("wl-copy"); err != nil {
		log.Fatal("找不到 wl-copy，装一下：sudo apt install wl-clipboard")
	}
	if os.Getenv("WAYLAND_DISPLAY") == "" && waylandDisplay() == "" {
		log.Println("警告：环境里没有 WAYLAND_DISPLAY，wl-copy 可能写不进剪贴板")
	}

	notifyOn := !*noNotify

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/api/items", handleList)
	mux.HandleFunc("/api/send", handleSend(notifyOn))
	mux.HandleFunc("/api/recopy", handleRecopy(notifyOn))
	mux.HandleFunc("/api/delete", handleDelete)
	mux.HandleFunc("/api/lists", handleLists)
	mux.HandleFunc("/api/task", handleTask)
	mux.HandleFunc("/api/lan", handleLAN(*port))
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

	fmt.Println("voice_clip 已启动")
	fmt.Printf("  本机：   http://127.0.0.1:%d   （这个页面上有二维码，手机扫）\n", *port)
	if !*local {
		for _, ip := range lanIPs() {
			fmt.Printf("  手机开： http://%s:%d\n", ip, *port)
		}
	}
	fmt.Printf("  历史：   %s\n", filepath.Join(dataDir, "items.jsonl"))
	fmt.Println("  Ctrl+C 退出")

	log.Fatal(http.Serve(ln, logged(mux)))
}

func usage() {
	fmt.Fprint(os.Stderr, `voice_clip —— 手机说话，文字直接进电脑剪贴板

干什么:
  起一个局域网网页。手机打开它，点输入框唤起输入法，按住语音键说一整段话，
  说完点「发送到电脑」，这段文字立刻进电脑的剪贴板，你在电脑上 Ctrl+V 就行。
  变相实现了「用手机给电脑做语音输入」。

  语音识别是手机输入法干的，这个工具只管把文字搬过来。所以：
  - 用你惯用的输入法就行，识别准不准取决于它，跟本工具无关。
  - 网页不申请麦克风权限，因此不需要 HTTPS 证书，局域网 http:// 直接能用。

怎么用:
  voice_clip                起服务，默认 :8092，监听 0.0.0.0（手机能连）
  voice_clip -port 9000     换端口
  voice_clip -local         只给本机用（手机连不上）
  voice_clip -no-notify     收到文字后不弹桌面通知
  voice_clip -task-list X   「+」默认写进哪个 Google Tasks 列表

  电脑上打开 http://127.0.0.1:8092 会显示二维码，手机扫一下就进同一个页面。
  手机上建议用浏览器的「添加到主屏幕」做成图标，以后一点就开。

页面上有什么:
  - 大输入框 + 「发送到电脑」按钮。发完自动清空，输入法不收起，可以接着说下一段。
  - 按回车直接发送，不用点按钮。想在文字里打换行用 Shift+回车。
    （拼音组字中按回车是上屏，那下不会误发，isComposing 和 keyCode 229 都拦了。）
  - 「说完停 2 秒自动发送」开关（默认关）。开了以后连回车都省了。
  - 右上角「+」：把输入框里的话建成一个 Google 任务（外调 gtasks）。
    紧挨着它左边是列表选择器，选过一次就记住（存在浏览器 localStorage 里）。
    默认写进哪个列表用 -task-list 改，当前默认是 My Tasks。
  - 最近的历史记录。点任意一条 = 把它重新放回电脑剪贴板（剪贴板被别的东西盖掉时用）。

产物落哪:
  历史记录 ~/.local/share/voice_clip/items.jsonl（一行一条 JSON，只留最近 200 条）
  改位置用 -data 或环境变量 VOICE_CLIP_DATA_DIR。

依赖什么:
  wl-copy（wl-clipboard 包）    写 Wayland 剪贴板，必需
  notify-send（libnotify-bin）  弹桌面通知，可选，没有就自动跳过
  gtasks                        建 Google 任务用，只有点「+」时才需要。
                                装在 ~/.local/bin，systemd 的 PATH 不含这个目录，
                                所以代码里做了回落，别指望 LookPath 能找到。

坑:
  - 剪贴板会被覆盖。每发一段就盖掉你当前复制的东西，发之前先把手头要粘的粘完。
  - Wayland 的剪贴板是「由某个进程持有」的，所以 wl-copy 是套 setsid 起的，
    本服务退出后剪贴板内容仍然在。但如果用 systemd 管理，单元里要写
    KillMode=process，否则重启服务会连带把 wl-copy 杀掉，剪贴板立刻变空。
  - 默认监听 0.0.0.0 且没有密码，同一局域网里谁都能往你剪贴板塞东西。
    在外面的网络（咖啡馆、公司）别开，或者加 -local。
  - 手机如果挂着代理（Clash 之类），确认 192.168 段是直连，否则连不上。
    用 IP 不要用主机名，能顺带绕开 fake-IP 那层 DNS。

`)
	flag.PrintDefaults()
}

// ---------- 存储 ----------

func itemsPath() string { return filepath.Join(dataDir, "items.jsonl") }

func loadItems() error {
	f, err := os.Open(itemsPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	for {
		var it Item
		if err := dec.Decode(&it); err != nil {
			break // 半行损坏就停在这儿，前面的照样能用
		}
		items = append(items, it)
	}
	trim()
	return nil
}

func saveItems() error {
	tmp := itemsPath() + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	for _, it := range items {
		if err := enc.Encode(it); err != nil {
			f.Close()
			os.Remove(tmp)
			return err
		}
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, itemsPath())
}

// trim 只保留最新的 maxItems 条。
func trim() {
	if len(items) > maxItems {
		items = items[len(items)-maxItems:]
	}
}

func addItem(it Item) error {
	mu.Lock()
	defer mu.Unlock()
	items = append(items, it)
	trim()
	return saveItems()
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

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ---------- Google Tasks ----------

// findBin 找外部命令。systemd 用户单元的 PATH 不含 ~/.local/bin，
// 而 gtasks 就装在那儿，所以 LookPath 失败要回落去那里找一遍。
func findBin(name string) (string, error) {
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	p := filepath.Join(os.Getenv("HOME"), ".local", "bin", name)
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("找不到 %s", name)
}

// listLineRe 解析 `gtasks lists` 的输出，形如：📋 单词积累  (YWdwNHpqcVdHM2J5VV9aQQ)
var listLineRe = regexp.MustCompile(`^\x{1F4CB}\s+(.*?)\s+\(([^()]+)\)\s*$`)

func taskLists() ([]string, error) {
	bin, err := findBin("gtasks")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "lists").Output()
	if err != nil {
		return nil, fmt.Errorf("gtasks lists 失败：%v", err)
	}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		if m := listLineRe.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			names = append(names, m[1])
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("一个列表都没解析出来")
	}
	return names, nil
}

func addTask(title, list string) error {
	bin, err := findBin("gtasks")
	if err != nil {
		return err
	}
	args := []string{"add"}
	if list != "" {
		args = append(args, "--list", list)
	}
	args = append(args, title)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("建任务失败：%s", firstLine(msg, 80))
	}
	return nil
}

// ---------- 剪贴板 ----------

// waylandDisplay 在环境变量缺失时，从 XDG_RUNTIME_DIR 里猜一个 socket 出来。
// systemd 用户单元有时候拿不到 WAYLAND_DISPLAY，没有它 wl-copy 直接报错。
func waylandDisplay() string {
	if v := os.Getenv("WAYLAND_DISPLAY"); v != "" {
		return v
	}
	rt := os.Getenv("XDG_RUNTIME_DIR")
	if rt == "" {
		return ""
	}
	for _, name := range []string{"wayland-0", "wayland-1"} {
		if _, err := os.Stat(filepath.Join(rt, name)); err == nil {
			return name
		}
	}
	return ""
}

// sessionEnv 补齐外调图形程序需要的环境变量。
func sessionEnv() []string {
	env := os.Environ()
	if os.Getenv("WAYLAND_DISPLAY") == "" {
		if d := waylandDisplay(); d != "" {
			env = append(env, "WAYLAND_DISPLAY="+d)
		}
	}
	if os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" {
		if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" {
			env = append(env, "DBUS_SESSION_BUS_ADDRESS=unix:path="+filepath.Join(rt, "bus"))
		}
	}
	return env
}

// copyToClipboard 把文字写进剪贴板，失败重试一次。
//
// 实测偶尔会卡一下（接管别的程序持有的剪贴板内容时，比如 Nautilus 剪切文件
// 留下的 application/x-trash），重试立刻就好。写剪贴板是幂等的，多写一次没有
// 副作用，所以重试是白捡的。
func copyToClipboard(text string) error {
	err := copyOnce(text)
	if err == nil {
		return nil
	}
	log.Printf("写剪贴板失败，重试一次：%v", err)
	time.Sleep(300 * time.Millisecond)
	return copyOnce(text)
}

// copyOnce 调一次 wl-copy。两处讲究，少一处都不行：
//
//  1. 套一层 setsid。Wayland 的剪贴板内容是由进程持有的，wl-copy 会 fork 一个
//     后台进程一直守着。不脱离进程组的话，本服务被 Ctrl+C 或 systemd 停掉时会
//     连它一起收走，剪贴板瞬间变空。
//  2. 三个标准流全部用真文件，不能用 bytes.Buffer / strings.Reader。那两个会让
//     Go 建管道，而 wl-copy fork 出来的守护进程继承了管道写端并且永不关闭，
//     cmd.Run() 就永远等不到 EOF——实测必定卡死，请求再也回不来。
func copyOnce(text string) error {
	wlcopy, err := exec.LookPath("wl-copy")
	if err != nil {
		return fmt.Errorf("找不到 wl-copy：%v", err)
	}

	in, err := os.CreateTemp("", "voice_clip-in-*")
	if err != nil {
		return fmt.Errorf("建不了临时文件：%v", err)
	}
	defer os.Remove(in.Name())
	defer in.Close()
	if _, err := in.WriteString(text); err != nil {
		return fmt.Errorf("写不了临时文件：%v", err)
	}
	if _, err := in.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("临时文件回不到开头：%v", err)
	}

	out, err := os.CreateTemp("", "voice_clip-out-*")
	if err != nil {
		return fmt.Errorf("建不了临时文件：%v", err)
	}
	defer os.Remove(out.Name())
	defer out.Close()

	name, args := wlcopy, []string{}
	if setsid, err := exec.LookPath("setsid"); err == nil {
		name, args = setsid, []string{wlcopy}
	}
	args = append(args, "--type", "text/plain;charset=utf-8")

	// 加个超时兜底，万一 wl-copy 真卡住也别把 HTTP 请求一起挂死
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = sessionEnv()
	cmd.Stdin = in
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		msg, _ := os.ReadFile(out.Name())
		if s := strings.TrimSpace(string(msg)); s != "" {
			return fmt.Errorf("wl-copy 失败：%s", s)
		}
		if ctx.Err() != nil {
			return fmt.Errorf("wl-copy 超时没返回")
		}
		return fmt.Errorf("wl-copy 失败：%v", err)
	}
	return nil
}

func notify(text string) {
	bin, err := exec.LookPath("notify-send")
	if err != nil {
		return // 没装就算了，不是必需的
	}
	cmd := exec.Command(bin, "-a", "voice_clip", "-t", "2500",
		"已进剪贴板", firstLine(text, 40))
	cmd.Env = sessionEnv()
	cmd.Run()
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
	// 新的在前，手机上一眼就看到刚发的那条
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	writeJSON(w, map[string]any{"items": out})
}

func handleSend(notifyOn bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			fail(w, 405, "只收 POST")
			return
		}
		var req struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			fail(w, 400, "请求读不懂："+err.Error())
			return
		}
		text := strings.TrimSpace(req.Text)
		if text == "" {
			fail(w, 400, "没有内容")
			return
		}

		// 先写剪贴板再记账：写不进去就别假装成功了
		if err := copyToClipboard(text); err != nil {
			fail(w, 500, err.Error())
			return
		}

		it := Item{ID: newID(), Text: text, Created: time.Now().Unix(), From: clientIP(r)}
		if err := addItem(it); err != nil {
			log.Printf("历史存不下：%v", err) // 剪贴板已经成了，不算失败
		}
		if notifyOn {
			notify(text)
		}
		log.Printf("收到 %d 字：%s", len([]rune(text)), firstLine(text, 30))
		writeJSON(w, map[string]any{"ok": true, "item": it})
	}
}

func handleRecopy(notifyOn bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			fail(w, 405, "只收 POST")
			return
		}
		var req struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			fail(w, 400, "请求读不懂："+err.Error())
			return
		}
		it, ok := findItem(req.ID)
		if !ok {
			fail(w, 404, "没这条记录")
			return
		}
		if err := copyToClipboard(it.Text); err != nil {
			fail(w, 500, err.Error())
			return
		}
		if notifyOn {
			notify(it.Text)
		}
		writeJSON(w, map[string]any{"ok": true})
	}
}

func handleDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, 405, "只收 POST")
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, 400, "请求读不懂："+err.Error())
		return
	}
	mu.Lock()
	defer mu.Unlock()
	kept := items[:0]
	found := false
	for _, it := range items {
		if it.ID == req.ID {
			found = true
			continue
		}
		kept = append(kept, it)
	}
	items = kept
	if !found {
		fail(w, 404, "没这条记录")
		return
	}
	if err := saveItems(); err != nil {
		fail(w, 500, "删是删了，但存不下："+err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func handleLists(w http.ResponseWriter, r *http.Request) {
	names, err := taskLists()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]any{"lists": names, "default": defaultTaskList})
}

func handleTask(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, 405, "只收 POST")
		return
	}
	var req struct {
		Text string `json:"text"`
		List string `json:"list"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, 400, "请求读不懂："+err.Error())
		return
	}
	title := strings.TrimSpace(req.Text)
	if title == "" {
		fail(w, 400, "没有内容")
		return
	}
	// Google Tasks 的标题是单行的，语音里的换行换成空格
	title = strings.Join(strings.Fields(title), " ")
	if err := addTask(title, req.List); err != nil {
		fail(w, 500, err.Error())
		return
	}
	log.Printf("建任务到「%s」：%s", req.List, firstLine(title, 30))
	writeJSON(w, map[string]any{"ok": true})
}

func handleLAN(port int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var urls []string
		for _, ip := range lanIPs() {
			urls = append(urls, fmt.Sprintf("http://%s:%d", ip, port))
		}
		writeJSON(w, map[string]any{"urls": urls})
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
	return s
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
