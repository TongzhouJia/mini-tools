// study_pinger —— 定时提醒起来动一动。
//
// 原来这是个「学习时间采样器」：随机间隔弹框问「此刻在干嘛」，把回答攒起来
// 算时间都花哪了。跑了一个月（601 次采样、236 次作答）之后停掉了——
// 采样能算出时间去哪了，但算不出「为什么坐下就是不开始」，而那才是真问题。
// 2026-09-09 起改成现在这样：不问、不记、不统计，只按点提醒。
//
// 改用途的直接起因：X 光查出 L5/S1 椎间盘间隙变窄 + L3-5 略失稳。
// 症状是坐着加重、走路缓解——坐位的椎间盘内压比站着高四成左右。
// 所以真正要干预的不是「学没学」，是「连续坐了多久」。
//
// 跟老版本的两个关键反转：
//   1. 间隔从随机改成固定，而且对齐整点/半点。老版本故意让你预判不了，
//      因为要防止「等它弹完再走神」；现在目的是养成节律，可预期反而是优点。
//   2. 从输入框（--entry）改成通知框（--info），一个按钮，不用打字。
//      一天要弹三十多次，任何需要动手的设计都会在第三天被关掉。
//
// 老数据 pings.jsonl 原样留着没删，只是不再往里写。
package main

import (
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed index.html
var indexHTML []byte

const defaultPort = "8083"

// 默认数据目录：~/.local/share/study_pinger，可用 PINGER_DATA_DIR 覆盖。
// 现在这个目录里只剩一个 .lock 有用，老的 pings.jsonl 留着当存档。
var defaultDataDir = func() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "study_pinger_data"
	}
	return filepath.Join(home, ".local", "share", "study_pinger")
}()

var (
	dataDir  string
	port     string
	everyMin int
	hoursArg string
	pingNow  bool

	stateMu    sync.Mutex
	nextPingAt time.Time
	pingCount  int
	startedAt  = time.Now()
)

func setNextPing(t time.Time) {
	stateMu.Lock()
	nextPingAt = t
	stateMu.Unlock()
}

func getState() (time.Time, int) {
	stateMu.Lock()
	defer stateMu.Unlock()
	return nextPingAt, pingCount
}

// 弹框自动关闭的秒数。人不在电脑前时窗口会一直挂着，挂着就卡住整个循环。
// 比老版本的 300 秒短很多——那时候要等人打字，现在只是看一眼。
const popupTimeoutSec = 90

// 提醒正文的第二行，每次随机挑一条。第一行永远是「站起来」，
// 因为那条才是目的；这些只是防止同一句话看三十遍之后彻底失效。
var tails = []string{
	"走两分钟就行，不用做操。",
	"顺手接杯水。",
	"坐回去的时候，腰后面垫个东西。",
	"别塌着坐，靠背用起来。",
	"站着把这一段想完，再坐下写。",
	"脖子也转两下。",
	"眼睛看一下远处。",
	"上个厕所也算。",
	"不是让你休息，是让你换个姿势。",
	"两分钟，比你以为的短。",
}

// ---------- 弹框 ----------

// notify 弹一个只有确定键的通知框。不收任何输入，也不写任何文件。
func notify() {
	tail := tails[rand.Intn(len(tails))]
	args := []string{
		"--info",
		"--title=起来动一下",
		"--text=<b>坐够 30 分钟了，站起来。</b>\n\n" + tail,
		"--ok-label=知道了",
		"--width=380",
		"--timeout=" + strconv.Itoa(popupTimeoutSec),
	}
	// 取消、超时、关窗都不算错——这里没有「答对答错」，弹到了就算数。
	_ = exec.Command("zenity", args...).Run()
}

// ---------- 时段控制 ----------

type window struct{ startMin, endMin int }

var activeWindow window

// parseHours 解析 "06:00-23:30" 这种活动时段，时段外不打扰。
func parseHours(s string) (window, error) {
	parts := strings.Split(strings.TrimSpace(s), "-")
	if len(parts) != 2 {
		return window{}, fmt.Errorf("时段格式应该是 06:00-23:30")
	}
	var w window
	for i, p := range parts {
		var h, m int
		if _, err := fmt.Sscanf(strings.TrimSpace(p), "%d:%d", &h, &m); err != nil {
			return window{}, fmt.Errorf("看不懂时间 %q", p)
		}
		if i == 0 {
			w.startMin = h*60 + m
		} else {
			w.endMin = h*60 + m
		}
	}
	return w, nil
}

func (w window) contains(t time.Time) bool {
	cur := t.Hour()*60 + t.Minute()
	if w.startMin <= w.endMin {
		return cur >= w.startMin && cur < w.endMin
	}
	return cur >= w.startMin || cur < w.endMin // 跨午夜，比如 22:00-02:00
}

// ---------- 提醒循环 ----------

// nextTick 返回下一个对齐的时刻。every=30 就是每个整点和半点。
// 对齐而不是「从启动时刻开始数」，是为了让它跟墙上的钟对得上——
// 重启一次就整体偏移几分钟的话，节律感就没了。
func nextTick(now time.Time, every int) time.Time {
	step := time.Duration(every) * time.Minute
	base := now.Truncate(time.Minute)
	t := base.Truncate(step)
	for !t.After(now) {
		t = t.Add(step)
	}
	return t
}

// 按墙上时钟等到 target。不能直接 time.Sleep——那用的是单调时钟，
// 机器挂起期间它不走。电脑一睡一整夜，醒来之后计时器还剩大半没走完，
// 于是整个上午一次都不弹（2026-08-06 就这么丢过一上午）。
func sleepUntil(target time.Time) {
	for {
		left := time.Until(target)
		if left <= 0 {
			return
		}
		if left > 30*time.Second {
			left = 30 * time.Second
		}
		time.Sleep(left)
	}
}

func pingLoop(w window) {
	for {
		next := nextTick(time.Now(), everyMin)
		setNextPing(next)
		fmt.Printf("下一次提醒：%s\n", next.Format("15:04"))
		sleepUntil(next)

		now := time.Now()
		if !w.contains(now) {
			fmt.Printf("%s 不在活动时段（%s），跳过\n", now.Format("15:04"), hoursArg)
			continue
		}
		stateMu.Lock()
		pingCount++
		n := pingCount
		stateMu.Unlock()
		fmt.Printf("%s 提醒（今天第 %d 次）\n", now.Format("15:04"), n)
		notify()
	}
}

// ---------- 状态页 ----------

type Status struct {
	NextPing  string `json:"next_ping"`
	StartedAt string `json:"started_at"`
	Hours     string `json:"hours"`
	EveryMin  int    `json:"every_min"`
	InWindow  bool   `json:"in_window"`
	Count     int    `json:"count"`
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	next, n := getState()
	st := Status{
		StartedAt: startedAt.Format(time.RFC3339),
		Hours:     hoursArg,
		EveryMin:  everyMin,
		InWindow:  activeWindow.contains(time.Now()),
		Count:     n,
	}
	if !next.IsZero() {
		st.NextPing = next.Format(time.RFC3339)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(st)
}

func cors(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		h(w, r)
	}
}

// ---------- 预检 ----------

func checkDeps() error {
	if _, err := exec.LookPath("zenity"); err != nil {
		return fmt.Errorf("找不到 zenity（弹框靠它）：sudo apt install zenity")
	}
	return nil
}

// acquireLock 拿数据目录里的排他锁，保证同一时间只有一个在跑。
// 没有这道锁的话，手动跑一个 + systemd 再跑一个 = 双倍弹框还撞端口
// （已经因此崩过 299 次）。锁随进程退出自动释放。
func acquireLock() (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dataDir, ".lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

const usage = `study_pinger —— 每半小时提醒你站起来动两分钟

用法：
  study_pinger            后台跑着，每 30 分钟弹一次（对齐整点和半点）
  study_pinger -now       启动就先弹一次，用来试效果
  study_pinger -every 45  改成每 45 分钟一次

状态页面：http://localhost:8083

不记录任何数据。老的采样数据 pings.jsonl 还在，只是不再写入。

依赖：
  zenity（GNOME 自带的弹窗程序），没有就弹不出来
环境变量：
  PINGER_EVERY_MIN  间隔分钟数（默认 30）
  PINGER_HOURS      活动时段，时段外不打扰（默认 06:00-23:30）
  PINGER_DATA_DIR   放锁文件的目录

参数：
`

func main() {
	flag.CommandLine.SetOutput(os.Stdout)
	flag.Usage = func() {
		fmt.Print(usage)
		flag.PrintDefaults()
	}
	flag.StringVar(&dataDir, "data", "", "数据目录（默认 ~/.local/share/study_pinger，或 PINGER_DATA_DIR）")
	flag.StringVar(&port, "port", defaultPort, "状态页面端口")
	flag.IntVar(&everyMin, "every", 0, "提醒间隔/分钟（默认 30，或 PINGER_EVERY_MIN）")
	flag.StringVar(&hoursArg, "hours", "", "活动时段，时段外不打扰（默认 06:00-23:30，或 PINGER_HOURS）")
	flag.BoolVar(&pingNow, "now", false, "启动时立刻弹一次（用来试效果）")
	var lan bool
	flag.BoolVar(&lan, "lan", false, "状态页面监听 0.0.0.0，同一个 Wi-Fi 下手机也能看（默认只有本机能开）")
	flag.Parse()

	if dataDir == "" {
		if d := strings.TrimSpace(os.Getenv("PINGER_DATA_DIR")); d != "" {
			dataDir = d
		} else {
			dataDir = defaultDataDir
		}
	}
	if everyMin == 0 {
		everyMin = 30
		if s := strings.TrimSpace(os.Getenv("PINGER_EVERY_MIN")); s != "" {
			if v, err := strconv.Atoi(s); err == nil && v > 0 {
				everyMin = v
			}
		}
	}
	if hoursArg == "" {
		hoursArg = "06:00-23:30"
		if s := strings.TrimSpace(os.Getenv("PINGER_HOURS")); s != "" {
			hoursArg = s
		}
	}

	if err := checkDeps(); err != nil {
		log.Fatalf("%v", err)
	}
	w, err := parseHours(hoursArg)
	if err != nil {
		log.Fatalf("%v", err)
	}
	activeWindow = w
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		log.Fatalf("建不了数据目录 %s: %v", dataDir, err)
	}

	// 已经有一个在跑就安静退出。注意是 exit 0 而不是失败——
	// 退成失败会让 systemd 的 Restart=on-failure 一直重启，正是之前崩 299 次的成因。
	lock, err := acquireLock()
	if err != nil {
		fmt.Println("已经有一个在跑了，这个就不启动了")
		fmt.Printf("   状态页面: http://localhost:%s\n", port)
		return
	}
	defer lock.Close()

	fmt.Println("起来动一下 提醒器已启动")
	fmt.Printf("   间隔: 每 %d 分钟（对齐整点/半点）\n", everyMin)
	fmt.Printf("   活动时段: %s\n", hoursArg)
	fmt.Printf("   状态页面: http://localhost:%s\n", port)
	fmt.Println("   不记录任何数据")

	if pingNow {
		go notify()
	}
	go pingLoop(w)

	http.HandleFunc("/", cors(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	}))
	http.HandleFunc("/api/status", cors(handleStatus))

	// 提醒才是核心功能，web 只是拿来看死活的。端口起不来就只警告，
	// 循环照跑——之前这里是 log.Fatalf，附属功能把主功能一起杀了。
	listenHost := "127.0.0.1"
	if lan {
		listenHost = "0.0.0.0"
	}
	ln, err := net.Listen("tcp4", listenHost+":"+port)
	if err != nil {
		fmt.Printf("端口 %s 用不了（%v），状态页面开不了，但提醒照常进行\n", port, err)
		select {}
	}
	if err := http.Serve(ln, nil); err != nil {
		fmt.Printf("状态页面挂了: %v；提醒照常进行\n", err)
		select {}
	}
}
