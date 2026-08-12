// study_pinger —— 学习时间采样器。
//
// 解决的问题：一整天都在学，却说不清时间到底花哪了。靠回忆估时间误差极大，
// 所以这里用「随机采样」：每隔平均 45 分钟随机弹一个框问「此刻在干嘛」，
// 答一行字（3 秒的事）。跑一周，把每个标签的 ping 次数乘以平均间隔，
// 就得到真实的时间分布——不依赖记忆。
//
// 间隔取指数分布（无记忆性），所以你没法预判下一次什么时候来，
// 也就没法「等它弹完再走神」。弹框本身也是干预：知道随时要交代，人会自己收回来。
//
// 跑起来后同时开一个 http://localhost:8083 看统计。
package main

import (
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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
	meanMin  float64
	hoursArg string
	pingNow  bool

	mu sync.Mutex // 护住 jsonl 文件的读写

	// 采样器的心跳状态。没有它就只能靠「等了很久没弹」来猜死活，
	// 而随机间隔本来就可能等 70 分钟——两者分不开人会以为程序坏了。
	stateMu    sync.Mutex
	nextPingAt time.Time
	startedAt  = time.Now()
)

func setNextPing(t time.Time) {
	stateMu.Lock()
	nextPingAt = t
	stateMu.Unlock()
}

func getNextPing() time.Time {
	stateMu.Lock()
	defer stateMu.Unlock()
	return nextPingAt
}

// 采样间隔的上下界（分钟）。指数分布尾巴很长，不夹一下会出现 3 分钟连弹
// 或者两小时不响的极端值。
const (
	minGapMin = 15
	maxGapMin = 70
)

// zenity 等回答的上限。人不在电脑前时窗口会一直挂着，挂着就卡住整个循环，
// 所以到点自动关掉记成「未答」——未答本身也是有用的数据（大概率是离开了）。
const answerTimeoutSec = 300

// 非学习标签。答案里含这些词就算「不在学」，用来算专注率。
// 可用 PINGER_IDLE_TAGS 覆盖（逗号分隔）。
var defaultIdleTags = []string{"摸鱼", "休息", "吃饭", "睡觉", "发呆", "刷手机", "刷视频", "家务", "聊天", "游戏", "洗澡", "出门"}

// Ping 是一条采样记录，一行一条存进 pings.jsonl。
type Ping struct {
	At         string `json:"at"`          // 弹框时间 RFC3339
	Answer     string `json:"answer"`      // 回答原文，未答为空
	Answered   bool   `json:"answered"`    // 有没有答
	ElapsedSec int    `json:"elapsed_sec"` // 从弹出到答完用了多久
	GapMin     int    `json:"gap_min"`     // 距上一次 ping 的间隔，统计时长用这个
}

func dataFile() string { return filepath.Join(dataDir, "pings.jsonl") }

// ---------- 数据读写 ----------

func appendPing(p Ping) error {
	mu.Lock()
	defer mu.Unlock()
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(dataFile(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	line, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	return err
}

func loadPings() []Ping {
	mu.Lock()
	defer mu.Unlock()
	raw, err := os.ReadFile(dataFile())
	if err != nil {
		return nil // 还没开始记，或者目录不存在，都当空数据
	}
	var out []Ping
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var p Ping
		if err := json.Unmarshal([]byte(line), &p); err != nil {
			continue // 单行坏了不影响其余
		}
		out = append(out, p)
	}
	return out
}

// lastAnswer 取最近一次有效回答，用来预填输入框——
// 大多数时候你还在干同一件事，直接回车就行，这是把填写成本压到最低的关键。
func lastAnswer(pings []Ping) string {
	for i := len(pings) - 1; i >= 0; i-- {
		if pings[i].Answered && strings.TrimSpace(pings[i].Answer) != "" {
			return pings[i].Answer
		}
	}
	return ""
}

// ---------- 弹框 ----------

// ask 弹 zenity 输入框问「此刻在干嘛」，返回回答和是否答了。
func ask(prefill string) (string, bool) {
	args := []string{
		"--entry",
		"--title=⏱ 在干嘛？",
		"--text=<b>此刻你正在做什么？</b>\n\n照实写，一个词就行（例：k8s网络、看yt、摸鱼）。\n在学就写学的内容；没在学就写 摸鱼/休息/吃饭。",
		"--width=460",
		"--timeout=" + strconv.Itoa(answerTimeoutSec),
	}
	if prefill != "" {
		args = append(args, "--entry-text="+prefill)
	}
	out, err := exec.Command("zenity", args...).Output()
	if err != nil {
		return "", false // 取消（exit 1）或超时（exit 5）都算未答
	}
	answer := strings.TrimSpace(string(out))
	return answer, answer != ""
}

// ---------- 时段控制 ----------

type window struct{ startMin, endMin int }

var activeWindow window

// parseHours 解析 "09:00-23:00" 这种活动时段，时段外不打扰。
func parseHours(s string) (window, error) {
	parts := strings.Split(strings.TrimSpace(s), "-")
	if len(parts) != 2 {
		return window{}, fmt.Errorf("时段格式应该是 09:00-23:00")
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

// nextGap 抽下一次间隔：指数分布，均值 meanMin，夹在 [minGapMin, maxGapMin]。
func nextGap() time.Duration {
	m := -meanMin * math.Log(1-rand.Float64())
	if m < minGapMin {
		m = minGapMin
	}
	if m > maxGapMin {
		m = maxGapMin
	}
	return time.Duration(m * float64(time.Minute))
}

// ---------- 采样循环 ----------

// 按墙上时钟等到 target。不能直接 time.Sleep(gap)——那用的是单调时钟，
// 机器挂起期间它不走。电脑一睡一整夜，醒来之后计时器还剩大半没走完，
// 于是整个上午一次都不弹（2026-08-06 就这么丢了一上午）。
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
	lastAt := time.Now()
	first := true
	for {
		gap := nextGap()
		if first {
			// 冷启动先来一次短的。随机间隔最长能到 70 分钟，启动后干等这么久
			// 没有任何动静，人只会以为程序死了（已经因此被怀疑两次）。
			// 先弹一次自证还活着，之后再进入正常的随机节奏。
			gap = time.Duration(2+rand.Intn(4)) * time.Minute
			first = false
		}
		next := time.Now().Add(gap)
		setNextPing(next)
		fmt.Printf("⏳ 下一次采样：%s（%.0f 分钟后）\n", next.Format("15:04"), gap.Minutes())
		sleepUntil(next)

		now := time.Now()
		if !w.contains(now) {
			fmt.Printf("⏭️ %s 不在活动时段（%s），跳过\n", now.Format("15:04"), hoursArg)
			lastAt = now
			continue
		}

		gapMin := int(now.Sub(lastAt).Minutes())
		lastAt = now

		start := time.Now()
		answer, ok := ask(lastAnswer(loadPings()))
		p := Ping{
			At:         now.Format(time.RFC3339),
			Answer:     answer,
			Answered:   ok,
			ElapsedSec: int(time.Since(start).Seconds()),
			GapMin:     gapMin,
		}
		if err := appendPing(p); err != nil {
			fmt.Printf("❌ 写记录失败: %v\n", err)
			continue
		}
		if ok {
			fmt.Printf("✅ %s  %s（%d 秒答完，覆盖前 %d 分钟）\n",
				now.Format("15:04"), answer, p.ElapsedSec, gapMin)
		} else {
			fmt.Printf("⚠️ %s  未答（人可能不在）\n", now.Format("15:04"))
		}
	}
}

// ---------- 统计 ----------

type Bucket struct {
	Tag     string `json:"tag"`
	Count   int    `json:"count"`
	Minutes int    `json:"minutes"`
	Idle    bool   `json:"idle"`
}

type Stats struct {
	NextPing     string   `json:"next_ping"`  // 下次采样时间，用来确认采样器还活着
	StartedAt    string   `json:"started_at"` // 采样器启动时间
	Hours        string   `json:"hours"`      // 活动时段
	InWindow     bool     `json:"in_window"`  // 此刻在不在活动时段内
	Range        string   `json:"range"`
	Total        int      `json:"total"`         // ping 总数
	Answered     int      `json:"answered"`      // 答了的
	StudyMinutes int      `json:"study_minutes"` // 学习类估计时长
	IdleMinutes  int      `json:"idle_minutes"`  // 非学习类估计时长
	Switches     int      `json:"switches"`      // 相邻采样标签不同的次数
	Buckets      []Bucket `json:"buckets"`
	Recent       []Ping   `json:"recent"`
}

func idleTags() []string {
	if s := strings.TrimSpace(os.Getenv("PINGER_IDLE_TAGS")); s != "" {
		var out []string
		for _, t := range strings.Split(s, ",") {
			if t = strings.TrimSpace(t); t != "" {
				out = append(out, t)
			}
		}
		return out
	}
	return defaultIdleTags
}

func isIdle(answer string) bool {
	a := strings.ToLower(answer)
	for _, t := range idleTags() {
		if strings.Contains(a, strings.ToLower(t)) {
			return true
		}
	}
	return false
}

// normTag 把回答归一成统计用的标签：去空格、转小写。
// 展示时用第一次出现的原文，保留他自己的写法。
func normTag(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func computeStats(pings []Ping, rangeName string) Stats {
	now := time.Now()
	var since time.Time
	switch rangeName {
	case "week":
		since = now.AddDate(0, 0, -7)
	default:
		rangeName = "today"
		since = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	}

	st := Stats{Range: rangeName}
	counts := map[string]*Bucket{}
	var order []string
	prevTag := ""

	for _, p := range pings {
		t, err := time.Parse(time.RFC3339, p.At)
		if err != nil || t.Before(since) {
			continue
		}
		st.Total++
		if !p.Answered {
			continue
		}
		st.Answered++

		// 每次采样代表它覆盖的那段时间。gap 缺失（老记录）就退回均值。
		mins := p.GapMin
		if mins <= 0 || mins > maxGapMin {
			mins = int(meanMin)
		}

		key := normTag(p.Answer)
		if b, ok := counts[key]; ok {
			b.Count++
			b.Minutes += mins
		} else {
			counts[key] = &Bucket{Tag: p.Answer, Count: 1, Minutes: mins, Idle: isIdle(p.Answer)}
			order = append(order, key)
		}
		if isIdle(p.Answer) {
			st.IdleMinutes += mins
		} else {
			st.StudyMinutes += mins
		}
		if prevTag != "" && prevTag != key {
			st.Switches++
		}
		prevTag = key

		st.Recent = append(st.Recent, p)
	}

	for _, k := range order {
		st.Buckets = append(st.Buckets, *counts[k])
	}
	sort.Slice(st.Buckets, func(i, j int) bool { return st.Buckets[i].Minutes > st.Buckets[j].Minutes })

	// Recent 只回最近 40 条，倒序（新的在前）
	for i, j := 0, len(st.Recent)-1; i < j; i, j = i+1, j-1 {
		st.Recent[i], st.Recent[j] = st.Recent[j], st.Recent[i]
	}
	if len(st.Recent) > 40 {
		st.Recent = st.Recent[:40]
	}
	return st
}

// ---------- web ----------

func cors(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		h(w, r)
	}
}

func handleStats(w http.ResponseWriter, r *http.Request) {
	st := computeStats(loadPings(), r.URL.Query().Get("range"))
	if n := getNextPing(); !n.IsZero() {
		st.NextPing = n.Format(time.RFC3339)
	}
	st.StartedAt = startedAt.Format(time.RFC3339)
	st.Hours = hoursArg
	st.InWindow = activeWindow.contains(time.Now())
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(st)
}

// ---------- 预检 ----------

func checkDeps() error {
	if _, err := exec.LookPath("zenity"); err != nil {
		return fmt.Errorf("找不到 zenity（弹框靠它）：sudo apt install zenity")
	}
	return nil
}

// acquireLock 拿数据目录里的排他锁，保证同一时间只有一个采样器在跑。
// 没有这道锁的话，手动跑一个 + systemd 再跑一个 = 双倍弹框、两边抢同一个
// jsonl 写、还会撞端口（已经因此崩过 299 次）。锁随进程退出自动释放。
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

func main() {
	flag.StringVar(&dataDir, "data", "", "数据目录（默认 ~/.local/share/study_pinger，或 PINGER_DATA_DIR）")
	flag.StringVar(&port, "port", defaultPort, "统计页面端口")
	flag.Float64Var(&meanMin, "mean", 0, "平均采样间隔/分钟（默认 45，或 PINGER_MEAN_MIN）")
	flag.StringVar(&hoursArg, "hours", "", "活动时段，时段外不打扰（默认 09:00-23:00，或 PINGER_HOURS）")
	flag.BoolVar(&pingNow, "now", false, "启动时立刻弹一次（用来试效果）")
	flag.Parse()

	if dataDir == "" {
		if d := strings.TrimSpace(os.Getenv("PINGER_DATA_DIR")); d != "" {
			dataDir = d
		} else {
			dataDir = defaultDataDir
		}
	}
	if meanMin == 0 {
		meanMin = 45
		if s := strings.TrimSpace(os.Getenv("PINGER_MEAN_MIN")); s != "" {
			if v, err := strconv.ParseFloat(s, 64); err == nil && v > 0 {
				meanMin = v
			}
		}
	}
	if hoursArg == "" {
		// 23:00 截止太早了——他 23:09 还在学，那个时段的采样会被白白跳过
		hoursArg = "09:00-24:00"
		if s := strings.TrimSpace(os.Getenv("PINGER_HOURS")); s != "" {
			hoursArg = s
		}
	}

	if err := checkDeps(); err != nil {
		log.Fatalf("❌ %v", err)
	}
	w, err := parseHours(hoursArg)
	if err != nil {
		log.Fatalf("❌ %v", err)
	}
	activeWindow = w
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		log.Fatalf("❌ 建不了数据目录 %s: %v", dataDir, err)
	}

	// 已经有一个在跑就安静退出。注意是 exit 0 而不是失败——
	// 退成失败会让 systemd 的 Restart=on-failure 一直重启，正是之前崩 299 次的成因。
	lock, err := acquireLock()
	if err != nil {
		fmt.Println("⏭️ 已经有一个采样器在跑了，这个就不启动了")
		fmt.Printf("   统计页面: http://localhost:%s\n", port)
		return
	}
	defer lock.Close()

	fmt.Println("⏱ 学习采样器已启动")
	fmt.Printf("   平均间隔: %.0f 分钟（随机 %d~%d 分钟，无法预判）\n", meanMin, minGapMin, maxGapMin)
	fmt.Printf("   活动时段: %s\n", hoursArg)
	fmt.Printf("   数据文件: %s\n", dataFile())
	fmt.Printf("   统计页面: http://localhost:%s\n", port)
	fmt.Println("   💡 弹框会预填上次的答案——还在干同一件事直接回车就行")

	if pingNow {
		go func() {
			answer, ok := ask(lastAnswer(loadPings()))
			p := Ping{At: time.Now().Format(time.RFC3339), Answer: answer, Answered: ok, GapMin: int(meanMin)}
			_ = appendPing(p)
			if ok {
				fmt.Printf("✅ %s  %s\n", time.Now().Format("15:04"), answer)
			} else {
				fmt.Println("⚠️ 未答")
			}
		}()
	}
	go pingLoop(w)

	http.HandleFunc("/", cors(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	}))
	http.HandleFunc("/api/stats", cors(handleStats))

	// 采样才是核心功能，web 只是拿来看数据的。端口起不来就只警告，
	// 采样循环照跑——之前这里是 log.Fatalf，附属功能把主功能一起杀了。
	ln, err := net.Listen("tcp4", "127.0.0.1:"+port)
	if err != nil {
		fmt.Printf("⚠️ 端口 %s 用不了（%v），统计页面开不了，但采样照常进行\n", port, err)
		select {} // 守住采样 goroutine
	}
	if err := http.Serve(ln, nil); err != nil {
		fmt.Printf("⚠️ 统计页面挂了: %v；采样照常进行\n", err)
		select {}
	}
}
