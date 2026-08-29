// hub —— 把本机所有自建服务收在一个页面上，点一下就跳过去；顺带用 systemd 让它们开机自启。
package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

//go:embed index.html
var assets embed.FS

// Service 是一个能用浏览器打开的本地服务。
type Service struct {
	ID   string   `json:"id"`             // 唯一标识，也用来拼 systemd 单元名
	Name string   `json:"name"`           // 页面上显示的中文名
	Desc string   `json:"desc"`           // 一句话说明
	Port int      `json:"port"`           // 监听端口
	Path string   `json:"path,omitempty"` // 打开时附加的路径，默认 /
	Exec string   `json:"exec,omitempty"` // 启动命令，空 = hub 不管它的死活
	Dir  string   `json:"dir,omitempty"`  // 工作目录
	Env  []string `json:"env,omitempty"`  // 额外环境变量，KEY=VALUE
	Unit string   `json:"unit,omitempty"` // systemd 用户单元名，空则按 hub-<id>.service 生成
	Auto bool     `json:"auto"`           // 是否让 hub 装 systemd 单元、开机自启
	LAN  bool     `json:"lan"`            // 是否监听 0.0.0.0（手机能开）
}

// Repo 是没有网页的项目，只在页面上列个路径方便复制。
type Repo struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Desc string `json:"desc"`
}

type Config struct {
	Services []Service `json:"services"`
	Repos    []Repo    `json:"repos"`
}

var (
	cfg     Config
	cfgPath string
	cfgMu   sync.RWMutex
)

func main() {
	home, _ := os.UserHomeDir()
	defaultCfg := filepath.Join(home, ".config", "hub", "services.json")

	port := flag.Int("port", 8090, "hub 自己的端口")
	confFlag := flag.String("config", defaultCfg, "配置文件（不存在会用默认清单生成一份）")
	local := flag.Bool("local", false, "只监听 127.0.0.1（手机就打不开了）")
	install := flag.Bool("install", false, "给配置里 auto=true 的服务装 systemd 用户单元并设为开机自启")
	uninstall := flag.Bool("uninstall", false, "撤掉 hub 装过的那些单元")
	status := flag.Bool("status", false, "在终端列一下每个服务现在活没活")
	flag.Usage = usage
	flag.Parse()

	cfgPath = *confFlag
	if err := loadConfig(); err != nil {
		fmt.Fprintln(os.Stderr, "读配置失败：", err)
		os.Exit(1)
	}

	switch {
	case *install:
		doInstall()
		return
	case *uninstall:
		doUninstall()
		return
	case *status:
		doStatus()
		return
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/api/services", handleServices)
	mux.HandleFunc("/api/action", handleAction)
	mux.HandleFunc("/api/reload", handleReload)

	host := "0.0.0.0"
	if *local {
		host = "127.0.0.1"
	}
	addr := fmt.Sprintf("%s:%d", host, *port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "端口 %d 起不来：%v\n", *port, err)
		os.Exit(1)
	}
	fmt.Println("hub 已启动")
	fmt.Printf("  本机：   http://127.0.0.1:%d\n", *port)
	if !*local {
		for _, ip := range lanIPs() {
			fmt.Printf("  局域网： http://%s:%d\n", ip, *port)
		}
	}
	fmt.Printf("  配置：   %s\n", cfgPath)
	fmt.Printf("  服务：   %d 个\n", len(cfg.Services))
	fmt.Println("  Ctrl+C 退出")
	http.Serve(ln, mux)
}

func usage() {
	fmt.Fprint(os.Stderr, `hub —— 所有自建服务的总入口

干什么:
  一个页面列出本机所有自建的网页服务（lexica、jp_reader、pdf_reader、clip_bridge…），
  显示每个活没活着，点一下就打开。还能一键把它们全设成开机自动启动。

怎么调:
  hub                起页面，默认 :8090，监听 0.0.0.0（手机也能开）
  hub -install       给配置里 auto=true 的服务装 systemd 用户单元 + 开机自启 + 立刻启动
  hub -uninstall     撤掉 hub 装过的单元（不动 study_pinger 这种你自己装的）
  hub -status        终端里列一下谁活着
  hub -port 9000     换端口
  hub -local         只监听本机
  hub -config 路径   换配置文件

产物落哪:
  配置    $HOME/.config/hub/services.json  —— 第一次跑自动生成，之后随便改
  单元    $HOME/.config/systemd/user/hub-<id>.service
  开机自启是 systemd 用户级的，跟着你登录桌面时起。想让它在没登录时也跑：
      sudo loginctl enable-linger $USER

依赖什么:
  systemctl（-install/启停按钮要用）。只看页面、只点链接的话什么都不需要。

有哪些坑:
  - 页面上的链接用的是你当前访问 hub 的主机名。手机打开 hub 没问题，但只有
    lan=true 的服务（监听 0.0.0.0 的）手机才连得上，别的会灰掉并注明。
  - 一个服务如果已经在外面手动跑着，systemd 再启一份会因为端口被占而失败。
    先把手动那份关掉，或者直接用页面上的「重启」按钮交给 systemd 管。
  - study_pinger 早就有自己的单元，配置里 auto=false，hub 不碰它，只显示状态。

`)
}

// ---------- 配置 ----------

func defaultConfig(home string) Config {
	mini := filepath.Join(home, "go-projects", "mini-tools")
	bin := func(n string) string { return filepath.Join(home, ".local", "bin", n) }
	return Config{
		Services: []Service{
			{ID: "hub", Name: "总入口", Desc: "就是这个页面", Port: 8090,
				Exec: bin("hub"), Dir: home, Auto: true, LAN: true},
			{ID: "lexica", Name: "lexica 语言学习", Desc: "翻译、朗读、听写、每日单词的自写服务端", Port: 8080, Path: "/lexica",
				Exec: filepath.Join(home, "go-projects", "lexica", "translate_server"),
				Dir:  filepath.Join(home, "go-projects", "lexica"), Auto: true, LAN: true},
			{ID: "clip_bridge", Name: "互传中转站", Desc: "手机电脑传文件和文字，能一键发邮件", Port: 8088,
				Exec: bin("clip_bridge"), Dir: home, Auto: true, LAN: true},
			{ID: "jp_reader", Name: "日语点读笔", Desc: "粘日语、划段、点一下就念", Port: 8086,
				Exec: bin("jp_reader"), Dir: mini, Auto: true},
			{ID: "en_drill", Name: "英语单词自测", Desc: "拿 In Our Time 的原声当提示", Port: 8087,
				Exec: bin("en_drill"), Dir: mini, Auto: true},
			// 网页那半边住在 lexica 里（/vocab/），没有自己的进程
			{ID: "vocab", Name: "带上下文的单词本", Desc: "粘整句、点词圈中、存下来（在 lexica 里）", Port: 8080,
				Path: "/vocab/", Auto: false, LAN: true},
			{ID: "pdf_reader", Name: "PDF 阅读器", Desc: "本地 PDF 变正常网页，能查词朗读", Port: 8084,
				Exec: bin("pdf_reader"), Dir: mini, Auto: true},
			{ID: "video_duration", Name: "视频时长统计", Desc: "填个目录，算总时长", Port: 8081,
				Exec: bin("video_duration_calculator"),
				Dir:  filepath.Join(mini, "cmd", "video_duration_calculator"), Auto: true},
			{ID: "study_pinger", Name: "学习时间采样", Desc: "随机弹窗问你在干嘛，这页是统计", Port: 8083,
				Unit: "study_pinger.service", Auto: false},
		},
		Repos: []Repo{
			{Name: "mini-tools", Path: filepath.Join(home, "go-projects", "mini-tools"), Desc: "所有小工具的仓库"},
			{Name: "lexica", Path: filepath.Join(home, "go-projects", "lexica"), Desc: "语言学习服务端"},
			{Name: "linux-drills", Path: filepath.Join(home, "go-projects", "linux-drills"), Desc: "Linux 刷题库"},
			{Name: "userscripts", Path: filepath.Join(home, "userscripts"), Desc: "油猴脚本合集"},
			{Name: "garmin", Path: filepath.Join(home, "garmin"), Desc: "佳明数据分析"},
			{Name: "记忆", Path: filepath.Join(home, ".claude", "projects", "-home-tom", "memory"), Desc: "Claude 的记忆库"},
		},
	}
}

func loadConfig() error {
	home, _ := os.UserHomeDir()
	b, err := os.ReadFile(cfgPath)
	if os.IsNotExist(err) {
		cfg = defaultConfig(home)
		if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
			return err
		}
		out, _ := json.MarshalIndent(cfg, "", "  ")
		if err := os.WriteFile(cfgPath, append(out, '\n'), 0o644); err != nil {
			return err
		}
		fmt.Println("已生成配置：", cfgPath)
		return nil
	}
	if err != nil {
		return err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return fmt.Errorf("%s 不是合法 JSON：%w", cfgPath, err)
	}
	cfg = c
	return nil
}

func unitName(s Service) string {
	if s.Unit != "" {
		return s.Unit
	}
	return "hub-" + s.ID + ".service"
}

// ---------- systemd ----------

func unitDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "systemd", "user")
}

func unitText(s Service) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "[Unit]\nDescription=%s (%s)\nAfter=network.target\n", s.Name, s.ID)
	sb.WriteString("StartLimitIntervalSec=300\nStartLimitBurst=5\n\n")
	sb.WriteString("[Service]\nType=simple\n")
	fmt.Fprintf(&sb, "ExecStart=%s\n", s.Exec)
	if s.Dir != "" {
		fmt.Fprintf(&sb, "WorkingDirectory=%s\n", s.Dir)
	}
	for _, e := range s.Env {
		fmt.Fprintf(&sb, "Environment=%s\n", e)
	}
	sb.WriteString("Restart=on-failure\nRestartSec=10\n\n")
	sb.WriteString("[Install]\nWantedBy=default.target\n")
	return sb.String()
}

func systemctl(args ...string) (string, error) {
	out, err := exec.Command("systemctl", append([]string{"--user"}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func doInstall() {
	dir := unitDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "建不了单元目录：", err)
		os.Exit(1)
	}
	var written []string
	for _, s := range cfg.Services {
		if !s.Auto {
			fmt.Printf("[跳过] %s：配置里 auto=false\n", s.Name)
			continue
		}
		if s.Exec == "" {
			fmt.Printf("[跳过] %s：没写 exec，不知道怎么启动\n", s.Name)
			continue
		}
		if _, err := os.Stat(s.Exec); err != nil {
			fmt.Printf("[跳过] %s：找不到 %s\n", s.Name, s.Exec)
			continue
		}
		name := unitName(s)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(unitText(s)), 0o644); err != nil {
			fmt.Printf("[失败] %s：%v\n", s.Name, err)
			continue
		}
		written = append(written, name)
		fmt.Printf("[写好] %s -> %s\n", s.Name, name)
	}
	if len(written) == 0 {
		fmt.Println("没有可装的单元")
		return
	}
	if out, err := systemctl("daemon-reload"); err != nil {
		fmt.Fprintln(os.Stderr, "daemon-reload 失败：", out, err)
		os.Exit(1)
	}
	var ok, bad int
	for _, name := range written {
		if out, err := systemctl("enable", "--now", name); err != nil {
			bad++
			fmt.Printf("[起不来] %s：%s\n", name, out)
			continue
		}
		ok++
		fmt.Printf("[已自启] %s\n", name)
	}
	fmt.Printf("完成：成功 %d，失败 %d\n", ok, bad)
	fmt.Println("这些服务会在你登录桌面时自动起来。想让它们在没登录时也跑，运行一次：")
	fmt.Println("  sudo loginctl enable-linger $USER")
}

func doUninstall() {
	dir := unitDir()
	var n int
	for _, s := range cfg.Services {
		name := unitName(s)
		if !strings.HasPrefix(name, "hub-") {
			continue // 只撤 hub 自己装的，别动他手写的单元
		}
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err != nil {
			continue
		}
		systemctl("disable", "--now", name)
		os.Remove(p)
		n++
		fmt.Printf("[撤掉] %s\n", name)
	}
	systemctl("daemon-reload")
	fmt.Printf("完成：撤掉 %d 个单元\n", n)
}

func doStatus() {
	for _, s := range cfg.Services {
		mark := "停着"
		if alive(s.Port) {
			mark = "活着"
		}
		unit := "-"
		if st, _ := systemctl("is-enabled", unitName(s)); st != "" {
			unit = st
		}
		fmt.Printf("%-6s :%-5d %-16s 自启:%s\n", mark, s.Port, s.Name, unit)
	}
}

// ---------- HTTP ----------

func alive(port int) bool {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 400*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
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

type serviceView struct {
	Service
	Alive   bool   `json:"alive"`
	Managed bool   `json:"managed"` // hub 能不能启停它
	Unit    string `json:"unit"`
	Enabled string `json:"enabled"` // enabled / disabled / ""
}

func handleServices(w http.ResponseWriter, r *http.Request) {
	cfgMu.RLock()
	svcs := append([]Service(nil), cfg.Services...)
	repos := append([]Repo(nil), cfg.Repos...)
	cfgMu.RUnlock()

	views := make([]serviceView, len(svcs))
	var wg sync.WaitGroup
	for i, s := range svcs {
		wg.Add(1)
		go func(i int, s Service) {
			defer wg.Done()
			en, _ := systemctl("is-enabled", unitName(s))
			views[i] = serviceView{
				Service: s,
				Alive:   alive(s.Port),
				Managed: s.Exec != "" || s.Unit != "",
				Unit:    unitName(s),
				Enabled: en,
			}
		}(i, s)
	}
	wg.Wait()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]any{"services": views, "repos": repos})
}

func handleAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "只收 POST", 405)
		return
	}
	var req struct{ ID, Action string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "解析不了请求："+err.Error())
		return
	}
	switch req.Action {
	case "start", "restart", "stop":
	default:
		writeErr(w, 400, "只认 start / restart / stop")
		return
	}
	cfgMu.RLock()
	var found *Service
	for i := range cfg.Services {
		if cfg.Services[i].ID == req.ID {
			found = &cfg.Services[i]
			break
		}
	}
	cfgMu.RUnlock()
	if found == nil {
		writeErr(w, 404, "配置里没有这个服务")
		return
	}
	name := unitName(*found)
	if _, err := os.Stat(filepath.Join(unitDir(), name)); err != nil {
		writeErr(w, 400, "还没装 systemd 单元，先在终端跑一次 hub -install")
		return
	}
	out, err := systemctl(req.Action, name)
	if err != nil {
		writeErr(w, 500, name+" "+req.Action+" 失败："+out)
		return
	}
	// systemd 说启动了不等于端口开了，等一下再探。
	time.Sleep(700 * time.Millisecond)
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "alive": alive(found.Port)})
}

func handleReload(w http.ResponseWriter, r *http.Request) {
	cfgMu.Lock()
	err := loadConfig()
	cfgMu.Unlock()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "count": len(cfg.Services)})
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

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
		n := ifi.Name
		if strings.HasPrefix(n, "virbr") || strings.HasPrefix(n, "docker") || strings.HasPrefix(n, "br-") ||
			strings.HasPrefix(n, "veth") || strings.HasPrefix(n, "tun") || strings.EqualFold(n, "Mihomo") {
			continue
		}
		addrs, _ := ifi.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipn.IP.To4()
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || (ip[0] == 198 && ip[1] == 18) {
				continue
			}
			out = append(out, ip.String())
		}
	}
	return out
}
