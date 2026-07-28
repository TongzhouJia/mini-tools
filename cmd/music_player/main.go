package main

import (
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

//go:embed index.html
var indexHTML []byte

const (
	defaultMusicDir = "/Users/jiatongzhou/Documents/musics"
	defaultPort     = "8082"
	// Clash Verge 的默认混合端口
	defaultProxyPort = "7897"
)

var (
	musicDir  string
	port      string
	proxyPort string
)

var audioExts = map[string]bool{
	".flac": true, ".mp3": true, ".m4a": true, ".wav": true,
	".aac": true, ".aiff": true, ".aif": true, ".ogg": true,
}

type Song struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// player 保证同一时刻只有一个 afplay 在放歌
type player struct {
	mu      sync.Mutex
	cmd     *exec.Cmd
	current string
}

var p player

func (p *player) stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.killLocked()
}

func (p *player) killLocked() {
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
		_ = p.cmd.Wait()
	}
	p.cmd = nil
	p.current = ""
}

func (p *player) play(path string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.killLocked()

	cmd := exec.Command("afplay", path)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	p.cmd = cmd
	p.current = filepath.Base(path)

	// 播完自动清理状态
	go func(c *exec.Cmd) {
		_ = c.Wait()
		p.mu.Lock()
		if p.cmd == c {
			p.cmd = nil
			p.current = ""
		}
		p.mu.Unlock()
	}(cmd)

	return nil
}

func (p *player) now() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.current
}

func scanSongs() ([]Song, error) {
	var songs []Song
	err := filepath.WalkDir(musicDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // 单个条目出错就跳过，继续扫
		}
		if d.IsDir() {
			return nil
		}
		if !audioExts[strings.ToLower(filepath.Ext(path))] {
			return nil
		}
		songs = append(songs, Song{
			Name: strings.TrimSuffix(d.Name(), filepath.Ext(d.Name())),
			Path: path,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(songs, func(i, j int) bool { return songs[i].Path < songs[j].Path })
	return songs, nil
}

func cors(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func handleSongs(w http.ResponseWriter, r *http.Request) {
	songs, err := scanSongs()
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "songs": songs, "current": p.now()})
}

func handlePlay(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	// 只允许播放音乐目录里的文件
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil || !strings.HasPrefix(abs, musicDir+string(os.PathSeparator)) || !audioExts[strings.ToLower(filepath.Ext(abs))] {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]any{"ok": false, "message": "⚠️ 非法路径"})
		return
	}
	if _, err := os.Stat(abs); err != nil {
		w.WriteHeader(http.StatusNotFound)
		writeJSON(w, map[string]any{"ok": false, "message": "❌ 文件不存在"})
		return
	}
	if err := p.play(abs); err != nil {
		writeJSON(w, map[string]any{"ok": false, "message": "❌ 播放失败: " + err.Error()})
		return
	}
	name := filepath.Base(abs)
	fmt.Printf("▶️  正在播放 %s\n", name)
	writeJSON(w, map[string]any{"ok": true, "current": p.now()})
}

func handleStop(w http.ResponseWriter, r *http.Request) {
	p.stop()
	fmt.Println("⏹️  已停止")
	writeJSON(w, map[string]any{"ok": true, "current": ""})
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"ok": true, "current": p.now()})
}

// handlePAC 吐一个 PAC 脚本：内网直连，其它流量照旧走电脑上的代理。
//
// 手机把代理设成「自动 / Auto」+ 本接口地址后，既能点歌又不影响科学上网 ——
// 手动代理模式下 iOS 没有 bypass 列表，全局模式的 Clash 又会把内网 IP 也甩给远端
// 节点（远端当然连不到你家局域网），于是返回 502。
func handlePAC(w http.ResponseWriter, r *http.Request) {
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}
	proxy := net.JoinHostPort(host, proxyPort)

	w.Header().Set("Content-Type", "application/x-ns-proxy-autoconfig")
	fmt.Fprintf(w, `function FindProxyForURL(url, host) {
  if (isPlainHostName(host)) return "DIRECT";
  var ip = /^\d+\.\d+\.\d+\.\d+$/.test(host) ? host : dnsResolve(host);
  if (!ip) return "PROXY %[1]s";
  if (isInNet(ip, "10.0.0.0", "255.0.0.0")) return "DIRECT";
  if (isInNet(ip, "172.16.0.0", "255.240.0.0")) return "DIRECT";
  if (isInNet(ip, "192.168.0.0", "255.255.0.0")) return "DIRECT";
  if (isInNet(ip, "127.0.0.0", "255.0.0.0")) return "DIRECT";
  if (isInNet(ip, "169.254.0.0", "255.255.0.0")) return "DIRECT";
  return "PROXY %[1]s";
}
`, proxy)
}

// lanIPs 列出所有可能给手机用的局域网 IPv4，
// 跳过回环 / link-local / 代理软件的 fake-ip 网段 (198.18.0.0/15) 和点对点的 utun
func lanIPs() []string {
	var ips []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return ips
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagPointToPoint != 0 {
			continue
		}
		if strings.HasPrefix(iface.Name, "utun") || strings.HasPrefix(iface.Name, "awdl") || strings.HasPrefix(iface.Name, "llw") {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP.To4()
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			if ip[0] == 198 && (ip[1] == 18 || ip[1] == 19) { // fake-ip
				continue
			}
			ips = append(ips, ip.String())
		}
	}
	sort.Strings(ips)
	return ips
}

func main() {
	flag.StringVar(&musicDir, "dir", defaultMusicDir, "音乐目录")
	flag.StringVar(&port, "port", defaultPort, "监听端口")
	flag.StringVar(&proxyPort, "proxy-port", defaultProxyPort, "电脑上代理软件的混合端口，供 /proxy.pac 使用")
	flag.Parse()

	abs, err := filepath.Abs(musicDir)
	if err != nil {
		log.Fatalf("❌ 音乐目录无效: %s", musicDir)
	}
	musicDir = strings.TrimSuffix(abs, string(os.PathSeparator))
	if _, err := os.Stat(musicDir); err != nil {
		log.Fatalf("❌ 音乐目录不存在: %s", musicDir)
	}

	http.HandleFunc("/", cors(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	}))
	http.HandleFunc("/api/songs", cors(handleSongs))
	http.HandleFunc("/api/play", cors(handlePlay))
	http.HandleFunc("/api/stop", cors(handleStop))
	http.HandleFunc("/api/status", cors(handleStatus))
	http.HandleFunc("/proxy.pac", cors(handlePAC))

	songs, _ := scanSongs()
	fmt.Printf("🎵 点歌台已启动，共 %d 首歌 (%s)\n", len(songs), musicDir)
	fmt.Printf("   本机:  http://localhost:%s\n", port)
	ips := lanIPs()
	if len(ips) == 0 {
		fmt.Println("   ⚠️ 没找到局域网 IP，手机可能连不上")
	}
	for _, ip := range ips {
		fmt.Printf("   手机:  http://%s:%s\n", ip, port)
	}
	fmt.Println("   💡 手机报 502 = 请求被电脑上的代理甩给了远端节点（全局模式常见）")
	if len(ips) > 0 {
		fmt.Printf("      手机代理改成「自动」并填: http://%s:%s/proxy.pac  → 内网直连，其它照旧走代理\n", ips[0], port)
	}

	// 显式监听 IPv4 的 0.0.0.0，避免只开 IPv6 socket 时某些客户端连不上
	ln, err := net.Listen("tcp4", "0.0.0.0:"+port)
	if err != nil {
		log.Fatalf("❌ 端口 %s 占用或不可用: %v", port, err)
	}
	if err := http.Serve(ln, nil); err != nil {
		log.Fatal(err)
	}
}
