// tts —— 本地语音合成：Qwen3-TTS 1.7B 跑在本机的 4060 上，不联网、不花钱、不限量。
//
// 网页上能干三件事：
//
//	念    选一个声音念一段字，中日英韩等 10 种语言；长文按句切开，边合成边念
//	克隆  传一段录音，存成一个新声音，之后拿它念任何字
//	描述  用一句话描述一个声音（「低沉沙哑的中年男人」），试听满意就存下来
//
// 模型真正跑在一个 Python 子进程里（worker.py，嵌在二进制里），有活才起，
// 闲 -idle 这么久就杀掉：它装着模型时一直占 4~5G 显存，不放掉的话 llm 就起不来了。
// 一个子进程只装一种模型，念预设音色和念自己的声音用的是两个模型，来回换要等十几秒。
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"
)

//go:embed index.html
var indexHTML []byte

//go:embed worker.py
var workerPy []byte

const (
	defaultPort = "8089"
	// 网页会按句切好再发，一次几十到一百多字；这个上限只挡住整篇直接怼进来
	maxText = 1000
	// 参考录音只留前这么多秒：5~15 秒最合适，太长了每一句都要多算
	maxRefSecs = 20
)

var (
	dataDir      string
	modelsDir    string
	pythonBin    string
	whisperModel string
	idleAfter    time.Duration
	gpu          = &worker{state: "off"}
)

var modelDirs = map[string]string{
	"custom": "Qwen3-TTS-12Hz-1.7B-CustomVoice",
	"clone":  "Qwen3-TTS-12Hz-1.7B-Base",
	"design": "Qwen3-TTS-12Hz-1.7B-VoiceDesign",
}

var modelNames = map[string]string{"custom": "预设音色", "clone": "克隆", "design": "描述造声音"}

type preset struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Desc string `json:"desc"`
	Lang string `json:"lang"`
}

// 模型自带的 9 个音色。每个都能念 10 种语言，母语那门最地道。
// 排第一的是默认。他嫌 Vivian「语气逆天、用力过猛」：同一句各生成 8 遍量音高起伏，
// Vivian 5 遍跨度超过两个八度，Serena、Ryan 8 遍都稳，所以默认换成 Serena（2026-09-28）。
var presets = []preset{
	{"serena", "Serena", "年轻女声，温柔、平稳", "中文"},
	{"uncle_fu", "Uncle Fu", "大叔，低沉醇厚", "中文"},
	{"vivian", "Vivian", "年轻女声，明亮、有点飒，起伏大", "中文"},
	{"dylan", "Dylan", "北京话男声", "中文"},
	{"eric", "Eric", "四川话男声", "中文"},
	{"ryan", "Ryan", "男声，节奏感强、平稳", "英语"},
	{"aiden", "Aiden", "美式男声，阳光", "英语"},
	{"ono_anna", "小野安娜", "女声，俏皮", "日语"},
	{"sohee", "Sohee", "女声，温暖", "韩语"},
}

var languages = map[string]bool{
	"auto": true, "chinese": true, "english": true, "japanese": true, "korean": true, "german": true,
	"french": true, "russian": true, "portuguese": true, "spanish": true, "italian": true,
}

// 页面上的语言 → 两字母代码，跟 whisper 报的对得上
var langCodes = map[string]string{
	"chinese": "zh", "english": "en", "japanese": "ja", "korean": "ko", "german": "de",
	"french": "fr", "russian": "ru", "portuguese": "pt", "spanish": "es", "italian": "it",
}

// 自己加的声音：voices/<id>/voice.json + ref.wav
type myVoice struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	From     string `json:"from"`               // upload / design
	Text     string `json:"text"`               // 参考录音里说的话；空 = 只用声纹，像的程度差一截
	Lang     string `json:"lang,omitempty"`     // 参考录音是哪种话（zh/ja/en…）
	Instruct string `json:"instruct,omitempty"` // 描述出来的声音，当初那句描述
	Created  string `json:"created"`
}

var idRe = regexp.MustCompile(`^[0-9a-z-]{6,40}$`)

const usage = `tts —— 本地语音合成（Qwen3-TTS 1.7B，跑在本机显卡上，不联网）

用法：
  tts                      起在 :8089，浏览器打开 http://localhost:8089
  tts -idle 30m            显卡进程闲 30 分钟才退（默认 10 分钟）

网页里怎么用：
  粘一段字，选声音和语言，点「朗读」（Ctrl+Enter）。长文按句切开，第一句好了就开始念，
  念完能下载整段 wav。预设音色可以加一句「语气」：开心一点 / 小声说 / 很生气
  「加一个声音」：传一段 5~15 秒的录音克隆，或者用一句话描述一个声音、试听满意再存

显卡和显存：
  模型在 Python 子进程里跑，有活才起，闲 -idle 就退出，显存（4~5G）跟着放掉
  第一次念、换了声音种类（预设 <-> 自己的）、或者闲置之后，要先等十几秒装模型
  llm 占着显卡时装不下，页面上会报「显存不够」

东西在哪：
  程序和 Python 环境   ~/.local/share/tts/（venv/、worker.py）
  自己加的声音         ~/.local/share/tts/voices/<id>/（ref.wav + voice.json）
  模型                 ~/.local/share/models/qwen3-tts/ 下三个目录，共 12G：
                       CustomVoice（预设音色）/ Base（克隆）/ VoiceDesign（描述造声音）
                       speech_tokenizer 三家是同一个文件，硬链接共用

依赖：
  ffmpeg（转参考录音）；whisper-cli + ~/ggml-large-v3.bin（识别参考录音说了什么，可缺）
  Python 环境重装：uv venv --python 3.12 ~/.local/share/tts/venv
                  uv pip install --python ~/.local/share/tts/venv/bin/python faster-qwen3-tts \
                    transformers==5.15.1 huggingface-hub==1.28.0 tokenizers==0.22.2
  transformers 必须钉在 5.15.1：5.17 改了 RoPE 配置，装模型报 'MimiConfig' has no attribute 'rope_theta'

参数：
`

func main() {
	flag.CommandLine.SetOutput(os.Stdout)
	flag.Usage = func() {
		fmt.Print(usage)
		flag.PrintDefaults()
	}
	port := flag.String("port", defaultPort, "监听端口")
	lan := flag.Bool("lan", false, "监听 0.0.0.0，同一个 Wi-Fi 下手机也能开（默认只有本机能开）")
	flag.StringVar(&dataDir, "data", envOr("TTS_DATA_DIR", filepath.Join(home(), ".local", "share", "tts")), "数据目录（Python 环境、自己加的声音）")
	flag.StringVar(&modelsDir, "models", filepath.Join(home(), ".local", "share", "models", "qwen3-tts"), "三个模型所在的目录")
	flag.StringVar(&pythonBin, "python", "", "用哪个 python 跑模型（默认 <数据目录>/venv/bin/python）")
	flag.StringVar(&whisperModel, "whisper-model", envOr("WHISPER_MODEL", filepath.Join(home(), "ggml-large-v3.bin")), "识别参考录音用的 whisper 模型")
	flag.DurationVar(&idleAfter, "idle", 10*time.Minute, "显卡进程闲多久就退出、放掉显存")
	flag.Parse()
	if pythonBin == "" {
		pythonBin = filepath.Join(dataDir, "venv", "bin", "python")
	}
	for _, d := range []string{"voices", "tmp"} {
		if err := os.MkdirAll(filepath.Join(dataDir, d), 0o755); err != nil {
			log.Fatalf("建不了数据目录：%v", err)
		}
	}
	cleanTmp()

	http.HandleFunc("/", handleIndex)
	http.HandleFunc("/api/status", handleStatus)
	http.HandleFunc("/api/voices", logged(handleVoices))
	http.HandleFunc("/api/speak", logged(handleSpeak))
	http.HandleFunc("/api/design", logged(handleDesign))
	http.HandleFunc("/api/design/save", logged(handleSaveDesign))
	go reapIdle()

	fmt.Printf("本地语音合成起来了：http://localhost:%s\n", *port)
	var have []string
	for _, k := range []string{"custom", "clone", "design"} {
		state := "没下"
		if _, err := os.Stat(filepath.Join(modelsDir, modelDirs[k], "model.safetensors")); err == nil {
			state = "有"
		}
		have = append(have, modelNames[k]+" "+state)
	}
	fmt.Printf("   模型：%s（%s）\n", modelsDir, strings.Join(have, "，"))
	if _, err := os.Stat(pythonBin); err != nil {
		fmt.Printf("   找不到 %s，念不了（重装方法见 tts -h）\n", pythonBin)
	}
	fmt.Printf("   显卡进程有活才起，闲 %s 退出放掉显存\n", idleAfter)

	host := "127.0.0.1"
	if *lan {
		host = "0.0.0.0"
	}
	if err := http.ListenAndServe(host+":"+*port, nil); err != nil {
		log.Fatalf("起不来（端口被占了？换 -port）：%v", err)
	}
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(indexHTML)
}

// GET /api/status —— 页面等第一句的时候轮询，好分清「在装模型」还是「在合成」
func handleStatus(w http.ResponseWriter, r *http.Request) {
	state, kind := gpu.status()
	writeJSON(w, map[string]string{"state": state, "kind": kind})
}

// /api/voices：GET 列出所有声音；POST 传录音存成新声音；DELETE ?id= 删掉一个
func handleVoices(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, map[string]any{"presets": presets, "mine": listVoices()})
	case http.MethodPost:
		v, err := addUploadedVoice(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, v)
	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		if !idRe.MatchString(id) {
			http.Error(w, "没有这个声音", http.StatusBadRequest)
			return
		}
		if err := os.RemoveAll(filepath.Join(dataDir, "voices", id)); err != nil {
			http.Error(w, "删不掉："+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	default:
		http.Error(w, "不支持", http.StatusMethodNotAllowed)
	}
}

// POST /api/speak {voice: "p:vivian" | "m:<id>", text, language, instruct} → wav
func handleSpeak(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "要用 POST", http.StatusMethodNotAllowed)
		return
	}
	var in struct{ Voice, Text, Language, Instruct string }
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
		http.Error(w, "请求看不懂："+err.Error(), http.StatusBadRequest)
		return
	}
	text, err := checkText(in.Text)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	lang := langOr(in.Language)
	kind, req, err := voiceRequest(in.Voice, lang, text)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req["text"] = text
	req["language"] = lang
	if kind == "custom" && strings.TrimSpace(in.Instruct) != "" {
		req["instruct"] = strings.TrimSpace(in.Instruct)
	}
	out := filepath.Join(dataDir, "tmp", "speak-"+randHex(6)+".wav")
	req["out"] = out
	defer os.Remove(out)

	msg, err := gpu.call(r.Context(), kind, req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("   %s：%v 秒音频，用了 %v 秒 —— %s", in.Voice, msg["secs"], msg["took"], short(text))
	serveWAV(w, r, out)
}

// POST /api/design {instruct, text, language} → 试听的 wav，响应头 X-Design-Id 留着存的时候用
func handleDesign(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "要用 POST", http.StatusMethodNotAllowed)
		return
	}
	var in struct{ Instruct, Text, Language string }
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
		http.Error(w, "请求看不懂："+err.Error(), http.StatusBadRequest)
		return
	}
	instruct := strings.TrimSpace(in.Instruct)
	if instruct == "" {
		http.Error(w, "先写一句描述：想要什么样的声音", http.StatusBadRequest)
		return
	}
	text, err := checkText(in.Text)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id := newID()
	out := filepath.Join(dataDir, "tmp", "design-"+id+".wav")
	_, err = gpu.call(r.Context(), "design", map[string]any{
		"text": text, "language": langOr(in.Language), "instruct": instruct, "out": out,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	meta, _ := json.Marshal(myVoice{From: "design", Text: text, Lang: targetLang(langOr(in.Language), text), Instruct: instruct})
	os.WriteFile(filepath.Join(dataDir, "tmp", "design-"+id+".json"), meta, 0o644)
	w.Header().Set("X-Design-Id", id)
	serveWAV(w, r, out)
}

// POST /api/design/save {id, name} —— 把试听满意的那段存成一个声音。
// 描述造出来的声音每念一次都会变，所以存的是这段试听录音本身，之后按它克隆，声音就定住了。
func handleSaveDesign(w http.ResponseWriter, r *http.Request) {
	var in struct{ ID, Name string }
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in); err != nil || !idRe.MatchString(in.ID) {
		http.Error(w, "请求看不懂", http.StatusBadRequest)
		return
	}
	tmpWav := filepath.Join(dataDir, "tmp", "design-"+in.ID+".wav")
	b, err := os.ReadFile(filepath.Join(dataDir, "tmp", "design-"+in.ID+".json"))
	if err != nil {
		http.Error(w, "这段试听找不到了（隔太久被清掉了？再试听一次）", http.StatusBadRequest)
		return
	}
	var v myVoice
	json.Unmarshal(b, &v)
	v.ID, v.Name, v.Created = newID(), strings.TrimSpace(in.Name), time.Now().Format("2006-01-02 15:04")
	if v.Name == "" {
		v.Name = short(v.Instruct)
	}
	dir := filepath.Join(dataDir, "voices", v.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := os.Rename(tmpWav, filepath.Join(dir, "ref.wav")); err != nil {
		os.RemoveAll(dir)
		http.Error(w, "存不了："+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := saveVoice(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	os.Remove(filepath.Join(dataDir, "tmp", "design-"+in.ID+".json"))
	log.Printf("   存了一个描述出来的声音「%s」：%s", v.Name, v.Instruct)
	writeJSON(w, v)
}

// ── 声音 ──────────────────────────────────────────────────────────────

// voiceRequest 把页面上选的声音翻译成「用哪个模型 + 给 worker 的参数」。
//
// 克隆有两种喂法：带参考录音的原文（ICL，像得多）/ 只给声纹。实测带原文念别的语言会把
// 参考录音的口音整个串过来——日语录音克隆的声音念中文，whisper 听成了「这堂煞在通心餐厅吗」；
// 只给声纹就字正腔圆。所以语言一致才带原文，不一致只给声纹。
func voiceRequest(voice, lang, text string) (string, map[string]any, error) {
	if id, ok := strings.CutPrefix(voice, "p:"); ok {
		for _, p := range presets {
			if p.ID == id {
				return "custom", map[string]any{"speaker": id}, nil
			}
		}
	}
	if id, ok := strings.CutPrefix(voice, "m:"); ok && idRe.MatchString(id) {
		v, err := loadVoice(id)
		if err == nil {
			refText := v.Text
			refLang := v.Lang
			if refLang == "" {
				refLang = guessLang(v.Text)
			}
			if refLang != targetLang(lang, text) {
				refText = ""
			}
			return "clone", map[string]any{
				"ref_audio": filepath.Join(dataDir, "voices", id, "ref.wav"),
				"ref_text":  refText,
			}, nil
		}
	}
	return "", nil, fmt.Errorf("不认识这个声音：%s（刷新一下页面）", voice)
}

// targetLang 这次要念成哪种话：页面上选了就听页面的，「自动」就看字
func targetLang(lang, text string) string {
	if c, ok := langCodes[lang]; ok {
		return c
	}
	return guessLang(text)
}

// guessLang 看字猜语言：有假名是日语，有谚文是韩语，剩下有汉字的算中文（纯汉字的日语词会猜错，
// 那种时候页面上选「日语」就行），西里尔字母算俄语，其余字母算英语
func guessLang(s string) string {
	var han, latin, cyr bool
	for _, r := range s {
		switch {
		case unicode.In(r, unicode.Hiragana, unicode.Katakana):
			return "ja"
		case unicode.Is(unicode.Hangul, r):
			return "ko"
		case unicode.Is(unicode.Han, r):
			han = true
		case unicode.Is(unicode.Cyrillic, r):
			cyr = true
		case unicode.IsLetter(r):
			latin = true
		}
	}
	switch {
	case han:
		return "zh"
	case cyr:
		return "ru"
	case latin:
		return "en"
	}
	return ""
}

func listVoices() []myVoice {
	out := []myVoice{}
	entries, _ := os.ReadDir(filepath.Join(dataDir, "voices"))
	for _, e := range entries {
		if v, err := loadVoice(e.Name()); err == nil {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID }) // 新加的在前
	return out
}

func loadVoice(id string) (myVoice, error) {
	var v myVoice
	b, err := os.ReadFile(filepath.Join(dataDir, "voices", id, "voice.json"))
	if err != nil {
		return v, err
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return v, err
	}
	if _, err := os.Stat(filepath.Join(dataDir, "voices", id, "ref.wav")); err != nil {
		return v, err
	}
	v.ID = id
	return v, nil
}

func saveVoice(v myVoice) error {
	b, _ := json.MarshalIndent(v, "", "  ")
	return os.WriteFile(filepath.Join(dataDir, "voices", v.ID, "voice.json"), b, 0o644)
}

// addUploadedVoice 收下页面传来的录音：转成 24k 单声道 wav、只留前 20 秒，
// 没填「录音里说的话」就让 whisper 听一遍写出来——有原文克隆得像得多。
func addUploadedVoice(r *http.Request) (myVoice, error) {
	var v myVoice
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		return v, fmt.Errorf("录音没传上来：%v", err)
	}
	f, hdr, err := r.FormFile("audio")
	if err != nil {
		return v, errors.New("先选一段录音")
	}
	defer f.Close()

	v = myVoice{
		ID:      newID(),
		Name:    strings.TrimSpace(r.FormValue("name")),
		From:    "upload",
		Text:    strings.TrimSpace(r.FormValue("text")),
		Created: time.Now().Format("2006-01-02 15:04"),
	}
	if v.Name == "" {
		v.Name = strings.TrimSuffix(hdr.Filename, filepath.Ext(hdr.Filename))
	}
	dir := filepath.Join(dataDir, "voices", v.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return v, err
	}
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(dir)
		}
	}()

	raw := filepath.Join(dir, "upload"+strings.ToLower(filepath.Ext(hdr.Filename)))
	dst, err := os.Create(raw)
	if err != nil {
		return v, err
	}
	_, err = io.Copy(dst, f)
	dst.Close()
	if err != nil {
		return v, fmt.Errorf("录音存不下来：%v", err)
	}
	ref := filepath.Join(dir, "ref.wav")
	cmd := exec.Command("ffmpeg", "-y", "-v", "error", "-i", raw, "-t", fmt.Sprint(maxRefSecs),
		"-vn", "-ac", "1", "-ar", "24000", "-sample_fmt", "s16", ref)
	if b, err := cmd.CombinedOutput(); err != nil {
		return v, fmt.Errorf("这个文件转不成音频：%s", squash(b))
	}
	os.Remove(raw)
	if fi, err := os.Stat(ref); err != nil || fi.Size() < 44+2*24000*2 {
		return v, errors.New("录音太短了，至少要两三秒（5~15 秒最好）")
	}

	if v.Text == "" {
		v.Text, v.Lang = transcribe(ref)
	} else {
		v.Lang = guessLang(v.Text)
	}
	if err := saveVoice(v); err != nil {
		return v, err
	}
	ok = true
	log.Printf("   存了一个克隆的声音「%s」，原文：%s", v.Name, short(v.Text))
	return v, nil
}

var detectedRe = regexp.MustCompile(`auto-detected language: ([a-z]+)`)

// transcribe 用本机的 whisper 听一遍参考录音，返回说的话和语言（zh/ja/en…）。
// whisper large-v3 也要 3G 多显存，先把合成进程停掉腾地方。
// 听不出来就返回空串——那样克隆只用声纹，照样能用。
func transcribe(ref string) (string, string) {
	if _, err := exec.LookPath("whisper-cli"); err != nil {
		return "", ""
	}
	if _, err := os.Stat(whisperModel); err != nil {
		return "", ""
	}
	gpu.mu.Lock()
	gpu.stop("腾显存给 whisper 听参考录音")
	defer gpu.mu.Unlock()

	wav16 := ref + ".16k.wav"
	defer os.Remove(wav16)
	if err := exec.Command("ffmpeg", "-y", "-v", "error", "-i", ref, "-ar", "16000", "-ac", "1", wav16).Run(); err != nil {
		return "", ""
	}
	var stderr bytes.Buffer
	cmd := exec.Command("whisper-cli", "-m", whisperModel, "-f", wav16, "-l", "auto", "-nt", "-np")
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		log.Printf("   whisper 没听出来：%v %s", err, squash(stderr.Bytes()))
		return "", ""
	}
	lang := ""
	if m := detectedRe.FindSubmatch(stderr.Bytes()); m != nil {
		lang = string(m[1])
	}
	var lines []string
	cjk := false
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
			for _, c := range l {
				if unicode.In(c, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul) {
					cjk = true
				}
			}
		}
	}
	text := strings.Join(lines, " ")
	if cjk {
		text = strings.Join(lines, "")
	}
	if lang == "" {
		lang = guessLang(text)
	}
	return text, lang
}

// ── 显卡进程 ──────────────────────────────────────────────────────────

type worker struct {
	mu   sync.Mutex // 显卡一次只干一件事；整个请求期间都拿着
	kind string
	cmd  *exec.Cmd
	in   io.WriteCloser
	out  *bufio.Reader
	errc chan struct{} // stderr 读完（进程退了）就关
	last time.Time
	seq  int

	smu   sync.Mutex // 只护下面三个，/api/status 不用排在正在跑的合成后面
	state string     // off / loading / ready / busy
	sKind string
	tail  []string // 子进程 stderr 最后几行，出错时带给页面
}

func (w *worker) setState(s string) {
	w.smu.Lock()
	w.state, w.sKind = s, w.kind
	w.smu.Unlock()
}

func (w *worker) status() (string, string) {
	w.smu.Lock()
	defer w.smu.Unlock()
	return w.state, w.sKind
}

// call 让显卡进程干一件活。要的模型跟装着的不一样就换一个进程——
// 一个进程只装一种模型，换的时候显存放得最干净。
func (w *worker) call(ctx context.Context, kind string, req map[string]any) (map[string]any, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if ctx.Err() != nil { // 排队的时候页面已经点了停止
		return nil, errors.New("已取消")
	}
	if w.cmd != nil && w.kind != kind {
		w.stop("换成" + modelNames[kind])
	}
	if w.cmd == nil {
		if err := w.start(kind); err != nil {
			return nil, err
		}
	}
	w.setState("busy")
	defer func() {
		w.last = time.Now()
		if w.cmd != nil {
			w.setState("ready")
		}
	}()

	w.seq++
	req["id"] = w.seq
	b, _ := json.Marshal(req)
	if _, err := w.in.Write(append(b, '\n')); err != nil {
		return nil, w.died()
	}
	for {
		msg, err := w.read()
		if err != nil {
			return nil, w.died()
		}
		if id, _ := msg["id"].(float64); int(id) != w.seq {
			continue
		}
		if ok, _ := msg["ok"].(bool); !ok {
			return nil, fmt.Errorf("%v", msg["error"])
		}
		return msg, nil
	}
}

func (w *worker) start(kind string) error {
	script := filepath.Join(dataDir, "worker.py")
	if err := os.WriteFile(script, workerPy, 0o644); err != nil {
		return err
	}
	if _, err := os.Stat(pythonBin); err != nil {
		return fmt.Errorf("找不到 %s：Python 环境没装（tts -h 里有重装方法）", pythonBin)
	}
	cmd := exec.Command(pythonBin, script, modelsDir, kind)
	cmd.Env = append(os.Environ(), "HF_HUB_OFFLINE=1", "TRANSFORMERS_OFFLINE=1", "TOKENIZERS_PARALLELISM=false")
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL} // 这边死了它跟着死，别留个占显存的孤儿
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	errp, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("合成进程起不来：%v", err)
	}
	w.cmd, w.in, w.out, w.kind = cmd, in, bufio.NewReader(out), kind
	w.errc = make(chan struct{})
	w.smu.Lock()
	w.tail = nil
	w.smu.Unlock()
	go w.pump(errp, w.errc)
	w.setState("loading")
	log.Printf("装模型：%s……", modelNames[kind])

	t0 := time.Now()
	for {
		msg, err := w.read()
		if err != nil {
			return w.died()
		}
		switch msg["event"] {
		case "ready":
			log.Printf("模型装好了：%s（%.0f 秒）", modelNames[kind], time.Since(t0).Seconds())
			w.last = time.Now()
			w.setState("ready")
			return nil
		case "failed":
			w.stop("")
			return fmt.Errorf("模型装不上：%v", msg["error"])
		}
	}
}

// read 读子进程回的下一行 JSON
func (w *worker) read() (map[string]any, error) {
	for {
		line, err := w.out.ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		var m map[string]any
		if json.Unmarshal(line, &m) == nil {
			return m, nil
		}
	}
}

// pump 把子进程的 stderr 原样转到终端（systemd 下就是 journal），顺手留最后几行
func (w *worker) pump(r io.Reader, done chan struct{}) {
	defer close(done)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		fmt.Fprintln(os.Stderr, "   [worker] "+line)
		w.smu.Lock()
		if w.tail = append(w.tail, line); len(w.tail) > 12 {
			w.tail = w.tail[len(w.tail)-12:]
		}
		w.smu.Unlock()
	}
}

// died 子进程半路没了：等它的 stderr 收完，拿最后几行当错误信息
func (w *worker) died() error {
	select {
	case <-w.errc:
	case <-time.After(2 * time.Second):
	}
	w.smu.Lock()
	tail := strings.Join(w.tail, "\n")
	w.smu.Unlock()
	w.stop("")
	if strings.Contains(strings.ToLower(tail), "out of memory") {
		return fmt.Errorf("显存不够（是不是 llm 之类的也在用显卡？）\n%s", tail)
	}
	return fmt.Errorf("合成进程挂了：\n%s", tail)
}

// stop 关掉 stdin 让子进程自己退，5 秒不退就杀。调用方要拿着 w.mu。
func (w *worker) stop(why string) {
	if w.cmd == nil {
		return
	}
	w.in.Close()
	done := make(chan struct{})
	go func() { w.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		w.cmd.Process.Kill()
		<-done
	}
	if why != "" {
		log.Printf("合成进程退了（%s），显存放掉了", why)
	}
	w.cmd, w.in, w.out, w.kind = nil, nil, nil, ""
	w.setState("off")
}

// reapIdle 闲太久就把子进程关掉，显存和独显一起放掉
func reapIdle() {
	for range time.Tick(30 * time.Second) {
		if !gpu.mu.TryLock() {
			continue
		}
		if gpu.cmd != nil && time.Since(gpu.last) > idleAfter {
			gpu.stop(fmt.Sprintf("闲了 %s", idleAfter))
		}
		gpu.mu.Unlock()
	}
}

// ── 小工具 ────────────────────────────────────────────────────────────

func checkText(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("没有要念的字")
	}
	if utf8.RuneCountInString(s) > maxText {
		return "", fmt.Errorf("一次最多 %d 字（网页会自动切好，直接调接口才会撞上）", maxText)
	}
	return s, nil
}

func langOr(l string) string {
	l = strings.ToLower(strings.TrimSpace(l))
	if languages[l] {
		return l
	}
	return "auto"
}

// serveWAV 读进内存再 http.ServeContent：带 Content-Length、认 Range，<audio> 才不闹脾气；
// 读进内存之后临时文件就能当场删
func serveWAV(w http.ResponseWriter, r *http.Request, path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		http.Error(w, "音频读不到："+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, "tts.wav", time.Now(), bytes.NewReader(b))
}

// cleanTmp 清掉一天前的临时文件（没存下来的试听、半路崩掉留下的 wav）
func cleanTmp() {
	dir := filepath.Join(dataDir, "tmp")
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > 24*time.Hour {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

func newID() string {
	return time.Now().Format("20060102-150405") + "-" + randHex(2)
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func short(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 30 {
		return string(r[:30]) + "…"
	}
	return s
}

func squash(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// logged 把 /api/* 的请求和返回码打到终端：浏览器那边的报错常常只剩一句废话，看这儿才知道真相。
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(c int) {
	w.code = c
	w.ResponseWriter.WriteHeader(c)
}

func logged(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, code: 200}
		t0 := time.Now()
		h(sw, r)
		mark := "ok "
		if sw.code >= 400 {
			mark = "ERR"
		}
		q, _ := url.QueryUnescape(r.URL.RawQuery)
		log.Printf("%s %s %s %d %s %s", mark, r.Method, r.URL.Path, sw.code, time.Since(t0).Round(time.Millisecond), q)
	}
}

func home() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return h
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
