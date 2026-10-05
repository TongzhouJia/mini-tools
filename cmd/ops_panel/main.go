// ops_panel —— 常用操作：把 ~/.local/scripts 里带「# 按钮:」头的脚本变成网页按钮，
// 点一下就跑、输出实时显示；每个按钮还能展开「怎么弄」，写着在终端里怎么手动跑。
package main

import (
	"bufio"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed index.html
var assets embed.FS

// Action 是页面上的一个按钮，信息全部来自脚本开头的注释头。
type Action struct {
	File    string   `json:"file"`              // 脚本文件名，兼当 id
	Name    string   `json:"name"`              // # 按钮:
	Group   string   `json:"group"`             // # 分组:
	Desc    string   `json:"desc"`              // # 说明:（多行接起来）
	Confirm string   `json:"confirm,omitempty"` // # 确认: 有就先弹确认框
	Manual  []string `json:"manual,omitempty"`  // # 手动: 一行一条
	Cmd     string   `json:"cmd"`               // 终端里怎么跑
	Running bool     `json:"running"`
}

var (
	home       string
	scriptsDir string

	runMu   sync.Mutex
	running = map[string]bool{}

	headerRe = regexp.MustCompile(`^#\s*(按钮|分组|说明|确认|手动)\s*[:：]\s*(.*)$`)
	ansiRe   = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)
	// 老工具输出里的符号 emoji（✅📊 这种）在页面上一律去掉
	emojiRe = regexp.MustCompile(`[\x{1F300}-\x{1FAFF}\x{2600}-\x{27BF}\x{2B50}\x{2B55}\x{FE0F}\x{200D}] ?`)
)

func main() {
	home, _ = os.UserHomeDir()
	port := flag.Int("port", 8094, "端口")
	dir := flag.String("dir", filepath.Join(home, ".local", "scripts"), "脚本目录")
	flag.Usage = usage
	flag.Parse()
	scriptsDir = *dir

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/api/actions", handleActions)
	mux.HandleFunc("/api/source", handleSource)
	mux.HandleFunc("/api/run", handleRun)

	addr := fmt.Sprintf("127.0.0.1:%d", *port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "端口 %d 起不来：%v\n", *port, err)
		os.Exit(1)
	}
	fmt.Println("常用操作 已启动")
	fmt.Printf("  本机：     http://%s\n", addr)
	fmt.Printf("  脚本目录： %s\n", scriptsDir)
	fmt.Println("  Ctrl+C 退出")
	http.Serve(ln, guard(*port, mux))
}

func usage() {
	fmt.Fprint(os.Stderr, `ops_panel —— 常用操作

干什么:
  把 ~/.local/scripts 里带「# 按钮:」注释头的脚本变成网页上的按钮。
  点「执行」就在本机跑那个脚本，输出实时显示在按钮下面；
  点「怎么弄」显示终端里怎么跑（带复制按钮）、手动的办法，还能看脚本原文。

怎么调:
  ops_panel              起页面，默认 http://127.0.0.1:8094（只监听本机）
  ops_panel -port 9000   换端口
  ops_panel -dir 路径    换脚本目录

加一个按钮:
  在脚本目录里放一个 .sh，开头 40 行内写这几行注释就行，不用重启，刷新页面就出来：
    # 按钮: 按钮上的字（必填，有它才算按钮）
    # 分组: 华为手机（同组的排在一起，按分组名排序）
    # 说明: 一句话说干什么（可以写多行，会接起来）
    # 确认: 删了找不回来，确定？（写了就先弹确认框，删东西的必须写）
    # 手动: 不用脚本的话怎么弄（一行一条，可以写多行）

产物落哪:
  不落盘。每次执行记一行到标准输出（交给 systemd 时在 journalctl --user -u hub-ops_panel）。

依赖什么:
  bash。各个脚本自己的依赖（adb、gcloud……）在 PATH 里能找到就行。

有哪些坑:
  - 只监听 127.0.0.1，手机打不开——它能在电脑上跑命令，不对局域网开放。
  - 别的网站借你的浏览器来点按钮会被挡掉：只认本机地址、同源，执行还要带页面才会加的请求头。
  - 脚本不能等键盘输入（网页没有输入框），要 sudo 的也跑不了，这类只适合写在「手动」里。
  - 关掉页面或刷新，正在跑的脚本会被连同子进程一起杀掉。
`)
}

// guard 挡掉别的网站借浏览器来点按钮：Host 必须是本机这个端口（防 DNS 重绑定），
// 带 Origin 的请求必须同源；/api/run 还要带自定义头，跨站请求带不了。
func guard(port int, next http.Handler) http.Handler {
	ok := map[string]bool{
		fmt.Sprintf("127.0.0.1:%d", port): true,
		fmt.Sprintf("localhost:%d", port): true,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ok[r.Host] {
			http.Error(w, "只认本机地址", http.StatusForbidden)
			return
		}
		if o := r.Header.Get("Origin"); o != "" && !ok[strings.TrimPrefix(o, "http://")] {
			http.Error(w, "不接受别的网站发来的请求", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
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

func handleActions(w http.ResponseWriter, r *http.Request) {
	acts, err := scan()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读不了脚本目录：" + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"dir": tilde(scriptsDir), "actions": acts})
}

func handleSource(w http.ResponseWriter, r *http.Request) {
	a := find(r.URL.Query().Get("file"))
	if a == nil {
		http.Error(w, "没有这个按钮", http.StatusNotFound)
		return
	}
	b, err := os.ReadFile(filepath.Join(scriptsDir, a.File))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(b)
}

func handleRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.Header.Get("X-Ops") != "1" {
		http.Error(w, "只接受页面上点按钮", http.StatusForbidden)
		return
	}
	a := find(r.URL.Query().Get("file"))
	if a == nil {
		http.Error(w, "没有这个按钮", http.StatusNotFound)
		return
	}
	runMu.Lock()
	if running[a.File] {
		runMu.Unlock()
		http.Error(w, "这个还在跑，等它跑完", http.StatusConflict)
		return
	}
	running[a.File] = true
	runMu.Unlock()
	defer func() {
		runMu.Lock()
		delete(running, a.File)
		runMu.Unlock()
	}()

	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "不支持流式输出", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")

	start := time.Now()
	cmd := exec.CommandContext(r.Context(), "bash", filepath.Join(scriptsDir, a.File))
	cmd.Dir = home
	cmd.Env = withLocalBin(os.Environ())
	// 自成一个进程组：页面关了要连 adb、curl 这些子进程一起杀
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 3 * time.Second
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		sendEvent(w, fl, "line", "起不来："+err.Error())
		sendEvent(w, fl, "done", map[string]any{"code": -1, "secs": 0})
		return
	}
	waitErr := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		pw.Close()
		waitErr <- err
	}()

	// 按 \n 出整行；单独的 \r 是进度条在原地刷新，发 cr 让页面覆盖当前行
	rd := bufio.NewReader(pr)
	var buf []byte
	for {
		c, err := rd.ReadByte()
		if err != nil {
			break
		}
		switch c {
		case '\n':
			sendEvent(w, fl, "line", clean(buf))
			buf = buf[:0]
		case '\r':
			if nb, _ := rd.Peek(1); len(nb) == 1 && nb[0] == '\n' {
				continue
			}
			sendEvent(w, fl, "cr", clean(buf))
			buf = buf[:0]
		default:
			buf = append(buf, c)
		}
	}
	if len(buf) > 0 {
		sendEvent(w, fl, "line", clean(buf))
	}

	code := 0
	if err := <-waitErr; err != nil {
		code = -1
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
	}
	secs := time.Since(start).Seconds()
	fmt.Printf("[%s] %s 退出码 %d（%.1f 秒）\n", time.Now().Format("01-02 15:04:05"), a.File, code, secs)
	sendEvent(w, fl, "done", map[string]any{"code": code, "secs": secs})
}

// scan 每次现读脚本目录，加了新脚本刷新页面就有。
func scan() ([]Action, error) {
	ents, err := os.ReadDir(scriptsDir)
	if err != nil {
		return nil, err
	}
	var acts []Action
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sh") {
			continue
		}
		a, ok := parseHeader(filepath.Join(scriptsDir, e.Name()))
		if !ok {
			continue
		}
		a.File = e.Name()
		a.Cmd = "bash " + tilde(filepath.Join(scriptsDir, e.Name()))
		runMu.Lock()
		a.Running = running[a.File]
		runMu.Unlock()
		acts = append(acts, a)
	}
	sort.SliceStable(acts, func(i, j int) bool {
		if acts[i].Group != acts[j].Group {
			return acts[i].Group < acts[j].Group
		}
		return acts[i].File < acts[j].File
	})
	return acts, nil
}

func parseHeader(path string) (Action, bool) {
	f, err := os.Open(path)
	if err != nil {
		return Action{}, false
	}
	defer f.Close()
	var a Action
	sc := bufio.NewScanner(f)
	for i := 0; i < 40 && sc.Scan(); i++ {
		m := headerRe.FindStringSubmatch(strings.TrimSpace(sc.Text()))
		if m == nil {
			continue
		}
		v := strings.TrimSpace(m[2])
		switch m[1] {
		case "按钮":
			a.Name = v
		case "分组":
			a.Group = v
		case "说明":
			a.Desc += v
		case "确认":
			a.Confirm = v
		case "手动":
			a.Manual = append(a.Manual, v)
		}
	}
	if a.Group == "" {
		a.Group = "其他"
	}
	return a, a.Name != ""
}

// find 只认扫出来的按钮，文件名带路径的一律不认。
func find(file string) *Action {
	if file == "" || strings.ContainsAny(file, `/\`) {
		return nil
	}
	acts, err := scan()
	if err != nil {
		return nil
	}
	for i := range acts {
		if acts[i].File == file {
			return &acts[i]
		}
	}
	return nil
}

func clean(b []byte) string {
	s := ansiRe.ReplaceAllString(string(b), "")
	return emojiRe.ReplaceAllString(s, "")
}

// withLocalBin 保证 PATH 里有 ~/.local/bin（gcloud、npm 这些在那）。
func withLocalBin(env []string) []string {
	lb := filepath.Join(home, ".local", "bin")
	for i, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			if !strings.Contains(kv, lb) {
				env[i] = "PATH=" + lb + ":" + strings.TrimPrefix(kv, "PATH=")
			}
			return env
		}
	}
	return append(env, "PATH="+lb+":/usr/local/bin:/usr/bin:/bin")
}

func tilde(p string) string {
	if home != "" && strings.HasPrefix(p, home+"/") {
		return "~" + strings.TrimPrefix(p, home)
	}
	return p
}

func sendEvent(w io.Writer, fl http.Flusher, event string, data any) {
	b, _ := json.Marshal(data)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
	fl.Flush()
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
