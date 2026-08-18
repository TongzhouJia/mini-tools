package main

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// 两个 Key 都走 REST + ?key=，跟 pdf_reader / tts_downloader 一个路子，不引 GCP SDK。
// 从环境变量或仓库根目录的 .env 读（.env 在 .gitignore 里，别提交）。
var (
	ttsKey       string
	translateKey string
)

const (
	ttsAPI       = "https://texttospeech.googleapis.com/v1/text:synthesize"
	voicesAPI    = "https://texttospeech.googleapis.com/v1/voices"
	translateAPI = "https://translation.googleapis.com/language/translate/v2"
	langCode     = "ja-JP"
	// Google 单次上限 5000 字节，留点余量。一段点读单元本来也不该这么长
	maxTextLen = 4500
)

// loadEnv 手动读 .env，格式就是 KEY=VALUE，# 开头当注释
func loadEnv(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
}

func initGCP(envPath string) {
	loadEnv(envPath)
	ttsKey = strings.TrimSpace(os.Getenv("GOOGLE_TTS_API_KEY"))
	translateKey = strings.TrimSpace(os.Getenv("GOOGLE_TRANSLATE_API_KEY"))
}

func cacheFile(kind, key, ext string) string {
	sum := sha1.Sum([]byte(key))
	return filepath.Join(dataDir, "cache", kind, hex.EncodeToString(sum[:])+ext)
}

func writeCache(path string, b []byte) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return // 缓存写不进去不算错，下次再打一次 API 就是了
	}
	os.WriteFile(path, b, 0o644)
}

func clampText(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("这段是空的")
	}
	if len(s) > maxTextLen {
		return "", fmt.Errorf("这段太长了（%d 字节，上限 %d），拆细一点", len(s), maxTextLen)
	}
	return s, nil
}

func clampRate(s string) float64 {
	r, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || r <= 0 {
		return defaultRate
	}
	if r < 0.25 {
		return 0.25
	}
	if r > 2.0 {
		return 2.0
	}
	return r
}

func pickVoice(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return defaultVoice
	}
	// 嗓子名字跟语种绑死，别的语种的名字配 ja-JP 会被 API 打回来
	if !strings.HasPrefix(s, langCode+"-") {
		return defaultVoice
	}
	return s
}

// ── 合成 ──────────────────────────────────────────────────────────────

// synth 返回这段话的 MP3 缓存路径，没有就打一次 API 再落盘。
// 缓存 key 带上嗓子和语速 —— 换了嗓子/语速就是另一份音频。
func synth(text, voice string, rate float64) (string, error) {
	if ttsKey == "" {
		return "", fmt.Errorf("没配 GOOGLE_TTS_API_KEY（放进仓库根目录的 .env）")
	}
	text, err := clampText(text)
	if err != nil {
		return "", err
	}

	ck := cacheFile("tts", fmt.Sprintf("%s|%s|%.2f", text, voice, rate), ".mp3")
	if fi, err := os.Stat(ck); err == nil && fi.Size() > 0 {
		return ck, nil
	}

	reqBody, _ := json.Marshal(map[string]any{
		"input": map[string]string{"text": text},
		"voice": map[string]string{"languageCode": langCode, "name": voice},
		"audioConfig": map[string]any{
			"audioEncoding": "MP3",
			"speakingRate":  rate,
		},
	})
	resp, err := http.Post(ttsAPI+"?key="+url.QueryEscape(ttsKey),
		"application/json", strings.NewReader(string(reqBody)))
	if err != nil {
		return "", fmt.Errorf("合成请求失败（网络？）：%w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("TTS API 返回 %s：%s", resp.Status, squash(body))
	}

	var parsed struct {
		AudioContent string `json:"audioContent"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.AudioContent == "" {
		return "", fmt.Errorf("TTS 结果看不懂：%s", squash(body))
	}
	mp3, err := base64.StdEncoding.DecodeString(parsed.AudioContent)
	if err != nil {
		return "", fmt.Errorf("音频解码失败：%w", err)
	}
	writeCache(ck, mp3)
	return ck, nil
}

// cachedPath 只查缓存在不在，不打 API。前端靠它给「已合成」的段做标记
func cachedPath(text, voice string, rate float64) (string, bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", false
	}
	ck := cacheFile("tts", fmt.Sprintf("%s|%s|%.2f", text, voice, rate), ".mp3")
	if fi, err := os.Stat(ck); err == nil && fi.Size() > 0 {
		return ck, true
	}
	return ck, false
}

func handleTTS(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if _, err := clampText(q.Get("text")); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest) // 是这段话的问题，不是上游的
		return
	}
	path, err := synth(q.Get("text"), pickVoice(q.Get("voice")), clampRate(q.Get("rate")))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	serveMP3(w, r, path)
}

// serveMP3 一律用 http.ServeContent 吐音频，别自己 w.Write。
//
// 自己写的话 Go 会走 Transfer-Encoding: chunked、不带 Content-Length，
// 浏览器给媒体发的 Range 请求也只回 200 不回 206 —— <audio> 碰上这种响应直接
// 判定「no supported source found」，报错里完全看不出根因（pdf_reader 上踩过）。
func serveMP3(w http.ResponseWriter, r *http.Request, path string) {
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "音频缓存读不到："+err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		http.Error(w, "音频缓存看不了："+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "audio/mpeg")
	http.ServeContent(w, r, filepath.Base(path), fi.ModTime(), f)
}

// ── 嗓子列表 ──────────────────────────────────────────────────────────

type voiceInfo struct {
	Name   string `json:"name"`
	Gender string `json:"gender"`
	Family string `json:"family"` // Chirp3-HD / Neural2 / Wavenet / Standard
}

// handleVoices 转发一次 voices 接口。写死一串名字迟早会跟线上对不上，
// 不如让 Google 自己报，前端下拉直接用返回的。
func handleVoices(w http.ResponseWriter, r *http.Request) {
	if ttsKey == "" {
		http.Error(w, "没配 GOOGLE_TTS_API_KEY", http.StatusServiceUnavailable)
		return
	}
	resp, err := http.Get(voicesAPI + "?languageCode=" + langCode + "&key=" + url.QueryEscape(ttsKey))
	if err != nil {
		http.Error(w, "查嗓子失败（网络？）："+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		http.Error(w, "voices API 返回 "+resp.Status+"："+squash(body), http.StatusBadGateway)
		return
	}
	var parsed struct {
		Voices []struct {
			Name       string `json:"name"`
			SsmlGender string `json:"ssmlGender"`
		} `json:"voices"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		http.Error(w, "voices 结果看不懂："+squash(body), http.StatusBadGateway)
		return
	}
	out := make([]voiceInfo, 0, len(parsed.Voices))
	for _, v := range parsed.Voices {
		parts := strings.Split(v.Name, "-") // ja-JP-Chirp3-HD-Achernar
		family := "Standard"
		if len(parts) > 3 {
			family = strings.Join(parts[2:len(parts)-1], "-")
		}
		out = append(out, voiceInfo{Name: v.Name, Gender: v.SsmlGender, Family: family})
	}
	writeJSON(w, out)
}

// handleCached 回一串「已经有音频」的段序号，前端拿它给这些段画实线下划线。
// 只查文件在不在，不打 API。
func handleCached(w http.ResponseWriter, r *http.Request) {
	d, err := loadDoc(r.URL.Query().Get("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	voice := pickVoice(r.URL.Query().Get("voice"))
	rate := clampRate(r.URL.Query().Get("rate"))
	out := []int{}
	for i, seg := range d.Segments {
		if _, hit := cachedPath(seg.Text, voice, rate); hit {
			out = append(out, i)
		}
	}
	writeJSON(w, out)
}

// ── 批量预合成 ────────────────────────────────────────────────────────

// handlePrewarm 把一篇文章的所有段一次性合成好，用 SSE 推进度。
// 划完段先点一下这个，之后点读就是纯本地播放，一点就响。
func handlePrewarm(w http.ResponseWriter, r *http.Request) {
	d, err := loadDoc(r.URL.Query().Get("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "这个连接不支持流式返回", http.StatusInternalServerError)
		return
	}
	voice := pickVoice(r.URL.Query().Get("voice"))
	rate := clampRate(r.URL.Query().Get("rate"))

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	var done, skipped, failed int
	for i, seg := range d.Segments {
		if _, hit := cachedPath(seg.Text, voice, rate); hit {
			skipped++
			sendEvent(w, flusher, "progress", map[string]any{
				"i": i + 1, "n": len(d.Segments), "state": "已有", "text": preview(seg.Text),
			})
			continue
		}
		if _, err := synth(seg.Text, voice, rate); err != nil {
			failed++
			sendEvent(w, flusher, "progress", map[string]any{
				"i": i + 1, "n": len(d.Segments), "state": "失败", "text": preview(seg.Text), "err": err.Error(),
			})
			continue
		}
		done++
		sendEvent(w, flusher, "progress", map[string]any{
			"i": i + 1, "n": len(d.Segments), "state": "已合成", "text": preview(seg.Text),
		})
	}
	sendEvent(w, flusher, "done", map[string]any{
		"done": done, "skipped": skipped, "failed": failed, "n": len(d.Segments),
	})
}

func sendEvent(w http.ResponseWriter, flusher http.Flusher, eventType string, data any) {
	b, err := json.Marshal(data)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, b)
	flusher.Flush()
}

func preview(s string) string {
	rs := []rune(strings.Join(strings.Fields(s), " "))
	if len(rs) > 18 {
		return string(rs[:18]) + "…"
	}
	return string(rs)
}

// ── 翻译 ──────────────────────────────────────────────────────────────

// handleTranslate 打一次 Cloud Translation，结果按 sha1 存盘，同一句不重复烧配额
func handleTranslate(w http.ResponseWriter, r *http.Request) {
	if translateKey == "" {
		http.Error(w, "没配 GOOGLE_TRANSLATE_API_KEY（放进仓库根目录的 .env）", http.StatusServiceUnavailable)
		return
	}
	text, err := clampText(r.URL.Query().Get("text"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ck := cacheFile("translate", text+"|"+transTo, ".json")
	if b, err := os.ReadFile(ck); err == nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Write(b)
		return
	}

	form := url.Values{"q": {text}, "target": {transTo}, "source": {"ja"}, "format": {"text"}}
	resp, err := http.PostForm(translateAPI+"?key="+url.QueryEscape(translateKey), form)
	if err != nil {
		http.Error(w, "翻译请求失败（网络？）："+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		http.Error(w, "翻译 API 返回 "+resp.Status+"："+squash(body), http.StatusBadGateway)
		return
	}
	var parsed struct {
		Data struct {
			Translations []struct {
				TranslatedText string `json:"translatedText"`
			} `json:"translations"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || len(parsed.Data.Translations) == 0 {
		http.Error(w, "翻译结果看不懂："+squash(body), http.StatusBadGateway)
		return
	}

	out, _ := json.Marshal(map[string]string{"text": parsed.Data.Translations[0].TranslatedText})
	writeCache(ck, out)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Write(out)
}

// squash 把 API 的报错压成一行塞进 HTTP 错误里，太长的截断
func squash(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
