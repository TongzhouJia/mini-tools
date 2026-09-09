// audio_recorder —— 把电脑正在放的声音录下来，录完自动转成文字。
//
// 跟网站无关：录的是声卡输出的那一路（monitor），谁在放都录得到，
// 加密的、没有下载按钮的、只能在浏览器里播的，一律通吃。代价是 1 倍速实时。
package main

import (
	"bufio"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed index.html
var assets embed.FS

// 状态机：闲着 → 录音中 → 转码中 → 转文字中 → 闲着
const (
	stIdle         = "idle"
	stRecording    = "recording"
	stProcessing   = "processing"
	stTranscribing = "transcribing"
)

// Rec 是一次录音的全部信息，落盘成 <name>.json。
type Rec struct {
	Name     string  `json:"name"`     // 20260909-175012_课程名，也是所有产物的文件名前缀
	Title    string  `json:"title"`    // 用户填的标题，可以为空
	Source   string  `json:"source"`   // 录的哪个音源
	Started  int64   `json:"started"`  // Unix 秒
	Duration float64 `json:"duration"` // 秒
	Err      string  `json:"err,omitempty"`

	// 下面这些是列表时现算的，不落盘
	HasAudio bool   `json:"hasAudio"`
	HasText  bool   `json:"hasText"`
	HasSrt   bool   `json:"hasSrt"`
	Text     string `json:"text,omitempty"`
}

// session 是「正在进行中的那一次」，闲着的时候是 nil。
type session struct {
	rec  Rec
	cmd  *exec.Cmd
	raw  string // 录音落地的 wav
	stop chan struct{}

	transcribe bool
	mail       bool
	autoStop   int     // 静音多少秒自动停，0 = 不自动
	silenceDB  float64 // 低于这个响度算静音

	// 实时状态，读写都要拿 mu
	elapsed    float64
	level      float64 // 当前响度 LUFS，-120 是绝对安静
	silentFor  float64
	heardSound bool // 还没听到过声音之前不许自动停，不然他还没点播放就停了
	stopping   bool
}

var (
	mu      sync.Mutex
	state   = stIdle
	stage   = "闲着"
	cur     *session
	lastErr string

	dataDir  string
	mailTo   string
	keepWav  bool
	perfMode bool
)

func main() {
	defaultData := filepath.Join(os.Getenv("HOME"), ".local", "share", "audio_recorder")
	if v := os.Getenv("AUDIO_RECORDER_DATA_DIR"); v != "" {
		defaultData = v
	}
	port := flag.Int("port", 8091, "监听端口")
	dir := flag.String("data", defaultData, "录音和转写产物放哪")
	lan := flag.Bool("lan", false, "监听 0.0.0.0（手机也能开着看进度）")
	to := flag.String("to", "", "发邮件的收件人，留空 = 发给自己")
	keep := flag.Bool("keep-wav", false, "保留原始 wav（默认转完 mp3 就删，wav 是 mp3 的 8 倍大）")
	perf := flag.Bool("perf", true, "转文字前自动切性能模式（tuned-adm），转完切回来")
	flag.Usage = usage
	flag.Parse()

	dataDir, mailTo, keepWav, perfMode = *dir, *to, *keep, *perf
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		log.Fatalf("建不了数据目录 %s：%v", dataDir, err)
	}
	if err := checkDeps(); err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/api/sources", handleSources)
	mux.HandleFunc("/api/start", handleStart)
	mux.HandleFunc("/api/stop", handleStop)
	mux.HandleFunc("/api/events", handleEvents)
	mux.HandleFunc("/api/list", handleList)
	mux.HandleFunc("/api/audio", handleAudio)
	mux.HandleFunc("/api/file", handleFile)
	mux.HandleFunc("/api/retranscribe", handleRetranscribe)
	mux.HandleFunc("/api/mail", handleMail)
	mux.HandleFunc("/api/delete", handleDelete)

	host := "127.0.0.1"
	if *lan {
		host = "0.0.0.0"
	}
	addr := fmt.Sprintf("%s:%d", host, *port)
	fmt.Printf("audio_recorder 起来了：http://localhost:%d\n", *port)
	fmt.Printf("录音和文字都放在：%s\n", dataDir)
	log.Fatal(http.ListenAndServe(addr, logged(mux)))
}

func usage() {
	fmt.Fprint(os.Stderr, `audio_recorder —— 录电脑正在放的声音，录完自动转成文字

干什么：
  录的是声卡输出那一路（PipeWire 的 monitor），也就是「你耳朵听到的东西」。
  所以跟网站、播放器、有没有加密全都无关——能放出声就能录。
  录完自动做响度标准化，再交给 audio_transcriber 转成文字。

怎么调：
  audio_recorder              起网页，默认 http://localhost:8091
  audio_recorder -lan         同时监听 0.0.0.0，手机上也能看进度
  audio_recorder -port 9000   换端口

  网页上的流程：选音源（默认已经填好扬声器的 monitor）→ 填个标题 →
  点「开始录音」→ 切到浏览器点播放 → 放完了它自己停（默认静音 60 秒算放完）。

产物（都在 ~/.local/share/audio_recorder/，网页上有下载按钮，不用去翻目录）：
  <名字>.mp3    响度拉齐过的音频，16k 单声道
  <名字>.txt    整篇纯文本
  <名字>.srt    带时间轴的字幕
  <名字>.json   这次录音的元信息（时长、音源、开始时间）

依赖：
  ffmpeg          录音 + 响度标准化
  audio_transcriber   转文字（whisper.cpp，有独显走 CUDA）
  gmail-send      可选，只有勾了「转完发邮件」才用
  tuned-adm       可选，转文字前自动切性能模式，转完切回来

有哪些坑：
  1. monitor 录的是音量调节之后的声音。系统音量拧到很小，录出来就是很小，
     whisper 会漏字。所以录之前把系统音量调到正常大小（工具会做响度标准化，
     但那是补救，不是补天）。
  2. 系统提示音、终端响铃、别的窗口的声音都会一起录进去，因为它们走的是同一个
     输出。要干净就录之前先把别的声音源关掉。
  3. 只能 1 倍速。开二倍速播放确实也能录、whisper 也认，但识别质量会掉，
     长课程不建议。
  4. 「静音自动停」在听到第一声之前不生效——不然你还没来得及点播放它就停了。
  5. 一次只能录一路。正在录的时候再点开始会被拒绝。

环境变量：
  AUDIO_RECORDER_DATA_DIR   产物目录（等价于 -data）

参数：
`)
	flag.PrintDefaults()
}

func checkDeps() error {
	var missing []string
	for _, b := range []string{"ffmpeg", "audio_transcriber"} {
		if _, err := exec.LookPath(b); err != nil {
			missing = append(missing, b)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("少了这些命令，装上再跑：%s", strings.Join(missing, "、"))
	}
	return nil
}

// ---------- 音源 ----------

type source struct {
	Name    string `json:"name"`
	Desc    string `json:"desc"`
	Monitor bool   `json:"monitor"` // 是不是「录电脑输出」的那种口
}

var srcRE = regexp.MustCompile(`^\s*\*?\s*(\S+)\s+\[(.*?)\]`)

// listSources 问 ffmpeg 要 pulse 侧看得见的输入口。
// 名字带 .monitor 的是扬声器输出的回环，其余是麦克风一类的真输入。
func listSources() []source {
	out, err := exec.Command("ffmpeg", "-hide_banner", "-sources", "pulse").CombinedOutput()
	if err != nil && len(out) == 0 {
		return nil
	}
	var list []source
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, "[") || strings.HasPrefix(strings.TrimSpace(line), "Auto-detected") {
			continue
		}
		m := srcRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		list = append(list, source{Name: m[1], Desc: m[2], Monitor: strings.HasSuffix(m[1], ".monitor")})
	}
	// monitor 排前面，因为九成九是要录电脑输出
	sort.SliceStable(list, func(i, j int) bool { return list[i].Monitor && !list[j].Monitor })
	return list
}

// ---------- 录音 ----------

// ebur128 每 100ms 往 stderr 打一行：t: 1.19979  TARGET:-23 LUFS  M: -22.6 ...
// M 是瞬时响度，绝对安静时是 -120 左右。拿它当电平表和静音判据。
var meterRE = regexp.MustCompile(`t:\s*([0-9.]+).*?M:\s*(-?[0-9.]+)`)

func handleStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Source     string  `json:"source"`
		Title      string  `json:"title"`
		AutoStop   int     `json:"autoStop"`
		SilenceDB  float64 `json:"silenceDB"`
		Transcribe bool    `json:"transcribe"`
		Mail       bool    `json:"mail"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpErr(w, 400, "请求读不懂：%v", err)
		return
	}
	if req.Source == "" {
		httpErr(w, 400, "没选音源")
		return
	}
	if req.SilenceDB == 0 {
		req.SilenceDB = -50
	}

	mu.Lock()
	defer mu.Unlock()
	if state != stIdle {
		httpErr(w, 409, "现在是「%s」，先等它完事", stage)
		return
	}

	name := time.Now().Format("20060102-150405")
	if t := sanitize(req.Title); t != "" {
		name += "_" + t
	}
	s := &session{
		rec: Rec{Name: name, Title: strings.TrimSpace(req.Title), Source: req.Source,
			Started: time.Now().Unix()},
		raw:        filepath.Join(dataDir, name+".wav"),
		stop:       make(chan struct{}),
		transcribe: req.Transcribe,
		mail:       req.Mail,
		autoStop:   req.AutoStop,
		silenceDB:  req.SilenceDB,
		level:      -120,
	}

	// -af ebur128 是直通滤镜，音频照写不误，顺带把响度打到 stderr
	s.cmd = exec.Command("ffmpeg",
		"-hide_banner", "-nostats", "-nostdin",
		"-f", "pulse", "-i", req.Source,
		"-ac", "1", "-ar", "16000",
		"-af", "ebur128=peak=none",
		"-y", s.raw)
	stderr, err := s.cmd.StderrPipe()
	if err != nil {
		httpErr(w, 500, "接不上 ffmpeg 的输出：%v", err)
		return
	}
	if err := s.cmd.Start(); err != nil {
		httpErr(w, 500, "ffmpeg 起不来：%v", err)
		return
	}

	cur, state, stage, lastErr = s, stRecording, "录音中", ""
	go readMeter(s, stderr)
	go watchdog(s)
	log.Printf("开始录音 %s，音源 %s", name, req.Source)
	writeJSON(w, map[string]any{"ok": true, "name": name})
}

// readMeter 一行行读 ffmpeg 的 stderr，把时长和响度刷进 session。
// ffmpeg 自己的报错也走这里，留最后几行好在页面上说人话。
func readMeter(s *session, stderr io.ReadCloser) {
	sc := bufio.NewScanner(stderr)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	var tail []string
	for sc.Scan() {
		line := sc.Text()
		m := meterRE.FindStringSubmatch(line)
		if m == nil {
			if t := strings.TrimSpace(line); t != "" {
				tail = append(tail, t)
				if len(tail) > 6 {
					tail = tail[1:]
				}
			}
			continue
		}
		t, _ := strconv.ParseFloat(m[1], 64)
		lv, _ := strconv.ParseFloat(m[2], 64)

		mu.Lock()
		s.elapsed = t
		s.level = lv
		if lv > s.silenceDB {
			s.heardSound = true
			s.silentFor = 0
		} else if s.heardSound {
			s.silentFor += 0.1 // ebur128 每 100ms 一行
		}
		mu.Unlock()
	}
	// ffmpeg 退了。正常停是我们自己发的信号，异常退出得把话留下。
	err := s.cmd.Wait()
	mu.Lock()
	quitByUs := s.stopping
	if err != nil && !quitByUs {
		s.rec.Err = "ffmpeg 挂了：" + strings.Join(tail, " / ")
	}
	mu.Unlock()
	close(s.stop)

	if !quitByUs {
		// 自己死的，没人会去跑后续，这里补上
		finish(s)
	}
}

// watchdog 负责「静音够久就自动停」。
func watchdog(s *session) {
	tk := time.NewTicker(time.Second)
	defer tk.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-tk.C:
			mu.Lock()
			hit := s.autoStop > 0 && s.heardSound && s.silentFor >= float64(s.autoStop) && !s.stopping
			mu.Unlock()
			if hit {
				log.Printf("静音超过 %d 秒，自动停", s.autoStop)
				stopRecording()
				return
			}
		}
	}
}

func handleStop(w http.ResponseWriter, r *http.Request) {
	if err := stopRecording(); err != nil {
		httpErr(w, 409, "%v", err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// stopRecording 给 ffmpeg 发 SIGINT 让它把文件收尾写好，然后走后处理。
func stopRecording() error {
	mu.Lock()
	s := cur
	if s == nil || state != stRecording || s.stopping {
		mu.Unlock()
		return errors.New("现在没在录音")
	}
	s.stopping = true
	s.rec.Duration = s.elapsed
	stage = "收尾中"
	mu.Unlock()

	_ = s.cmd.Process.Signal(syscall.SIGINT)
	select {
	case <-s.stop:
	case <-time.After(15 * time.Second):
		_ = s.cmd.Process.Kill()
		<-s.stop
	}
	go finish(s)
	return nil
}

// ---------- 后处理：响度标准化 → 转文字 → 发邮件 ----------

func finish(s *session) {
	defer func() {
		mu.Lock()
		if s.rec.Err != "" {
			lastErr = s.rec.Err
		}
		saveRec(s.rec)
		cur, state, stage = nil, stIdle, "闲着"
		mu.Unlock()
	}()

	if fi, err := os.Stat(s.raw); err != nil || fi.Size() < 4096 {
		setErr(s, "没录到东西（文件是空的）。多半是音源选错了，或者那一路根本没出声")
		return
	}

	// 1. 转 mp3 并把响度拉齐。monitor 录的是音量调节之后的声音，
	//    系统音量小的时候录出来会很轻，不拉一把 whisper 要漏字。
	mp3 := filepath.Join(dataDir, s.rec.Name+".mp3")
	setStage(stProcessing, "转码和响度标准化")
	out, err := exec.Command("ffmpeg", "-hide_banner", "-nostats", "-loglevel", "error",
		"-i", s.raw,
		"-af", "loudnorm=I=-16:LRA=11:TP=-1.5",
		"-ar", "16000", "-ac", "1", "-b:a", "64k",
		"-y", mp3).CombinedOutput()
	if err != nil {
		setErr(s, fmt.Sprintf("转码失败：%v %s", err, strings.TrimSpace(string(out))))
		return
	}
	if !keepWav {
		_ = os.Remove(s.raw)
	}
	if s.rec.Duration == 0 {
		s.rec.Duration = probeDuration(mp3)
	}
	if !s.transcribe {
		return
	}

	// 2. 转文字。交给已经调好的 audio_transcriber（whisper.cpp + CUDA + VAD）
	setStage(stTranscribing, "转文字中（whisper）")
	restore := perfBoost()
	tOut, tErr := exec.Command("audio_transcriber", "-f", mp3).CombinedOutput()
	restore()
	if tErr != nil {
		setErr(s, fmt.Sprintf("转文字失败：%v %s", tErr, tailLines(string(tOut), 4)))
		return
	}
	txt := filepath.Join(dataDir, s.rec.Name+".txt")
	body, rErr := os.ReadFile(txt)
	if rErr != nil {
		setErr(s, "转写跑完了但没找到 .txt，whisper 那边可能空转了")
		return
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		setErr(s, "转出来是空的。八成是录进去的声音太轻，或者录的是没有人声的那一路")
		return
	}
	log.Printf("%s 转写完成，%d 字", s.rec.Name, len([]rune(string(body))))

	// 3. 发邮件
	if s.mail {
		setStage(stTranscribing, "发邮件")
		subject := "录音转写：" + firstNonEmpty(s.rec.Title, s.rec.Name)
		args := []string{subject, "-f", txt}
		if mailTo != "" {
			args = append(args, "-t", mailTo)
		}
		if o, e := exec.Command("gmail-send", args...).CombinedOutput(); e != nil {
			setErr(s, fmt.Sprintf("转写好了，但邮件没发出去：%v %s", e, tailLines(string(o), 3)))
		}
	}
}

// perfBoost 把 CPU 切到性能模式，返回一个切回去的函数。
// tuned-adm 不在或者没权限就当无事发生，不该因为这个耽误转写。
func perfBoost() func() {
	if !perfMode {
		return func() {}
	}
	if _, err := exec.LookPath("tuned-adm"); err != nil {
		return func() {}
	}
	prev, err := exec.Command("tuned-adm", "active").Output()
	if err != nil {
		return func() {}
	}
	old := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(prev)), "Current active profile:"))
	if old == "" || old == "throughput-performance" {
		return func() {}
	}
	if err := exec.Command("tuned-adm", "profile", "throughput-performance").Run(); err != nil {
		return func() {}
	}
	log.Printf("性能模式：%s → throughput-performance", old)
	return func() {
		_ = exec.Command("tuned-adm", "profile", old).Run()
		log.Printf("性能模式切回 %s", old)
	}
}

func setStage(st, msg string) {
	mu.Lock()
	state, stage = st, msg
	mu.Unlock()
}

func setErr(s *session, msg string) {
	mu.Lock()
	s.rec.Err = msg
	mu.Unlock()
	log.Printf("出错：%s", msg)
}

// ---------- 状态推送 ----------

func handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		httpErr(w, 500, "这个连接不支持流式推送")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	tk := time.NewTicker(250 * time.Millisecond)
	defer tk.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tk.C:
			mu.Lock()
			st := map[string]any{"state": state, "stage": stage, "err": lastErr}
			if cur != nil {
				st["name"] = cur.rec.Name
				st["title"] = cur.rec.Title
				st["elapsed"] = cur.elapsed
				st["level"] = cur.level
				st["silentFor"] = cur.silentFor
				st["autoStop"] = cur.autoStop
				st["heard"] = cur.heardSound
			}
			mu.Unlock()
			b, _ := json.Marshal(st)
			fmt.Fprintf(w, "event: status\ndata: %s\n\n", b)
			fl.Flush()
		}
	}
}

// ---------- 历史 ----------

func recPath(name string) string { return filepath.Join(dataDir, name+".json") }

func saveRec(r Rec) {
	b, _ := json.MarshalIndent(r, "", "  ")
	if err := os.WriteFile(recPath(r.Name), append(b, '\n'), 0o644); err != nil {
		log.Printf("元信息写不进去：%v", err)
	}
}

func loadRecs() []Rec {
	entries, _ := os.ReadDir(dataDir)
	var list []Rec
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dataDir, e.Name()))
		if err != nil {
			continue
		}
		var r Rec
		if json.Unmarshal(b, &r) != nil || r.Name == "" {
			continue
		}
		r.HasAudio = exists(filepath.Join(dataDir, r.Name+".mp3"))
		r.HasSrt = exists(filepath.Join(dataDir, r.Name+".srt"))
		if body, err := os.ReadFile(filepath.Join(dataDir, r.Name+".txt")); err == nil {
			r.HasText, r.Text = true, string(body)
		}
		list = append(list, r)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Started > list[j].Started })
	return list
}

func handleList(w http.ResponseWriter, r *http.Request) { writeJSON(w, loadRecs()) }

// safeName 挡住 ../ 这类路径穿越，只认真实存在的那条记录。
func safeName(r *http.Request) (string, bool) {
	n := r.URL.Query().Get("name")
	if n == "" {
		n = r.FormValue("name")
	}
	if n == "" || strings.ContainsAny(n, "/\\") || strings.Contains(n, "..") {
		return "", false
	}
	if !exists(recPath(n)) {
		return "", false
	}
	return n, true
}

func handleAudio(w http.ResponseWriter, r *http.Request) {
	n, ok := safeName(r)
	if !ok {
		httpErr(w, 404, "没这条记录")
		return
	}
	p := filepath.Join(dataDir, n+".mp3")
	f, err := os.Open(p)
	if err != nil {
		httpErr(w, 404, "音频不在了")
		return
	}
	defer f.Close()
	fi, _ := f.Stat()
	w.Header().Set("Content-Type", "audio/mpeg")
	// 一定要 ServeContent：自己 Write 不带 Content-Length，浏览器拖进度条就废
	http.ServeContent(w, r, filepath.Base(p), fi.ModTime(), f)
}

func handleFile(w http.ResponseWriter, r *http.Request) {
	n, ok := safeName(r)
	if !ok {
		httpErr(w, 404, "没这条记录")
		return
	}
	ext := r.URL.Query().Get("ext")
	if ext != "txt" && ext != "srt" && ext != "mp3" {
		httpErr(w, 400, "只给 txt / srt / mp3")
		return
	}
	p := filepath.Join(dataDir, n+"."+ext)
	f, err := os.Open(p)
	if err != nil {
		httpErr(w, 404, "文件不在了")
		return
	}
	defer f.Close()
	fi, _ := f.Stat()
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename*=UTF-8''%s", pathEscape(n+"."+ext)))
	http.ServeContent(w, r, filepath.Base(p), fi.ModTime(), f)
}

func handleRetranscribe(w http.ResponseWriter, r *http.Request) {
	n, ok := safeName(r)
	if !ok {
		httpErr(w, 404, "没这条记录")
		return
	}
	mu.Lock()
	if state != stIdle {
		mu.Unlock()
		httpErr(w, 409, "现在是「%s」，先等它完事", stage)
		return
	}
	mu.Unlock()
	mp3 := filepath.Join(dataDir, n+".mp3")
	if !exists(mp3) {
		httpErr(w, 404, "音频不在了，重转不了")
		return
	}
	// audio_transcriber 见到同名 .txt 会跳过，重转就得先挪开
	_ = os.Remove(filepath.Join(dataDir, n+".txt"))
	go func() {
		setStage(stTranscribing, "重新转文字："+n)
		restore := perfBoost()
		out, err := exec.Command("audio_transcriber", "-f", mp3).CombinedOutput()
		restore()
		mu.Lock()
		if err != nil {
			lastErr = fmt.Sprintf("重转失败：%v %s", err, tailLines(string(out), 4))
		} else {
			lastErr = ""
		}
		state, stage = stIdle, "闲着"
		mu.Unlock()
	}()
	writeJSON(w, map[string]any{"ok": true})
}

func handleMail(w http.ResponseWriter, r *http.Request) {
	n, ok := safeName(r)
	if !ok {
		httpErr(w, 404, "没这条记录")
		return
	}
	txt := filepath.Join(dataDir, n+".txt")
	if !exists(txt) {
		httpErr(w, 404, "还没有文字稿，发不了")
		return
	}
	args := []string{"录音转写：" + n, "-f", txt}
	if mailTo != "" {
		args = append(args, "-t", mailTo)
	}
	if out, err := exec.Command("gmail-send", args...).CombinedOutput(); err != nil {
		httpErr(w, 500, "发不出去：%v %s", err, tailLines(string(out), 3))
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func handleDelete(w http.ResponseWriter, r *http.Request) {
	n, ok := safeName(r)
	if !ok {
		httpErr(w, 404, "没这条记录")
		return
	}
	for _, ext := range []string{".json", ".mp3", ".wav", ".txt", ".srt"} {
		_ = os.Remove(filepath.Join(dataDir, n+ext))
	}
	log.Printf("删了 %s", n)
	writeJSON(w, map[string]any{"ok": true})
}

func handleSources(w http.ResponseWriter, r *http.Request) { writeJSON(w, listSources()) }

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	b, _ := assets.ReadFile("index.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(b)
}

// ---------- 零碎 ----------

// logged 打状态码，浏览器不说的终端会说。
// 注意 Flush 必须透传，不然 SSE 那个 http.Flusher 断言直接失败。
type logWriter struct {
	http.ResponseWriter
	code int
}

func (l *logWriter) WriteHeader(c int) { l.code = c; l.ResponseWriter.WriteHeader(c) }
func (l *logWriter) Flush() {
	if f, ok := l.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func logged(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lw := &logWriter{ResponseWriter: w, code: 200}
		h.ServeHTTP(lw, r)
		if lw.code >= 400 {
			log.Printf("%d %s %s", lw.code, r.Method, r.URL.Path)
		}
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

func httpErr(w http.ResponseWriter, code int, format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " / ")
}

// sanitize 把标题洗成能当文件名的样子，中文照留。
func sanitize(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' ||
			r == '"' || r == '<' || r == '>' || r == '|' || r < 32:
			// 丢掉
		case r == ' ':
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len([]rune(out)) > 40 {
		out = string([]rune(out)[:40])
	}
	return out
}

func pathEscape(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func probeDuration(p string) float64 {
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration",
		"-of", "default=nw=1:nk=1", p).Output()
	if err != nil {
		return 0
	}
	d, _ := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	return d
}
