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
	"strings"
	"unicode"
)

// 两个 Key 都走 REST + ?key=，跟 tts_downloader 一个路子，不用引 GCP SDK。
// 从环境变量或仓库根目录的 .env 读（.env 已在 .gitignore 里，别提交）。
var (
	translateKey string
	ttsKey       string
)

const (
	translateAPI = "https://translation.googleapis.com/language/translate/v2"
	ttsAPI       = "https://texttospeech.googleapis.com/v1/text:synthesize"
	maxTextLen   = 3000 // 选太多一次翻译/朗读没意义，也怕烧配额
)

// loadEnv 手动读 .env，格式就是 KEY=VALUE，# 开头当注释。
// 跟 vocabulary_comparison 里那份一个意思，这里只认需要的两个 Key。
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
	translateKey = strings.TrimSpace(os.Getenv("GOOGLE_TRANSLATE_API_KEY"))
	ttsKey = strings.TrimSpace(os.Getenv("GOOGLE_TTS_API_KEY"))
}

// hasCJK 用来猜该拿哪种嗓子念。选中的是中日韩就用普通话，否则按 -voice 走。
// 没做语种检测——那要多打一次 API，为了念一个词不值当。
func hasCJK(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
			unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
			return true
		}
	}
	return false
}

func cacheFile(kind, key, ext string) string {
	sum := sha1.Sum([]byte(key))
	return filepath.Join(dataDir, "cache", kind, hex.EncodeToString(sum[:])+ext)
}

func readCache(path string) []byte {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return b
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
		return "", fmt.Errorf("没选中任何文字")
	}
	if len(s) > maxTextLen {
		return "", fmt.Errorf("选中的太长了（%d 字节，上限 %d）", len(s), maxTextLen)
	}
	return s, nil
}

// ── 翻译 ──────────────────────────────────────────────────────────────

type translateResp struct {
	Text string `json:"text"`
	From string `json:"from"`
}

// translateText 打一次 Cloud Translation，结果按 sha1 存盘。
// 存单词本时也走这里，所以同一个词翻两遍不会重复烧配额。
func translateText(text string) (translateResp, error) {
	var out translateResp
	if translateKey == "" {
		return out, fmt.Errorf("没配 GOOGLE_TRANSLATE_API_KEY（放进仓库根目录的 .env）")
	}
	text, err := clampText(text)
	if err != nil {
		return out, err
	}

	ck := cacheFile("translate", text+"|"+transTo, ".json")
	if b := readCache(ck); b != nil {
		if json.Unmarshal(b, &out) == nil {
			return out, nil
		}
	}

	form := url.Values{"q": {text}, "target": {transTo}, "format": {"text"}}
	resp, err := http.PostForm(translateAPI+"?key="+url.QueryEscape(translateKey), form)
	if err != nil {
		return out, fmt.Errorf("翻译请求失败（网络？）：%w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("翻译 API 返回 %s：%s", resp.Status, squash(body))
	}

	var parsed struct {
		Data struct {
			Translations []struct {
				TranslatedText         string `json:"translatedText"`
				DetectedSourceLanguage string `json:"detectedSourceLanguage"`
			} `json:"translations"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || len(parsed.Data.Translations) == 0 {
		return out, fmt.Errorf("翻译结果看不懂：%s", squash(body))
	}

	out = translateResp{
		Text: parsed.Data.Translations[0].TranslatedText,
		From: parsed.Data.Translations[0].DetectedSourceLanguage,
	}
	if b, err := json.Marshal(out); err == nil {
		writeCache(ck, b)
	}
	return out, nil
}

func handleTranslate(w http.ResponseWriter, r *http.Request) {
	out, err := translateText(r.URL.Query().Get("text"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(out)
}

// ── 朗读 ──────────────────────────────────────────────────────────────

// handleTTS 直接吐 MP3，前端 new Audio(url) 就能放。
// 合成过的按 sha1 存盘，同一个词念第二遍不再花钱。
func handleTTS(w http.ResponseWriter, r *http.Request) {
	if ttsKey == "" {
		http.Error(w, "没配 GOOGLE_TTS_API_KEY（放进仓库根目录的 .env）", http.StatusServiceUnavailable)
		return
	}
	text, err := clampText(r.URL.Query().Get("text"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// 嗓子名字是跟语言绑死的，选中中日韩时不能把 en-AU 的名字带过去，
	// 只给 languageCode 让 Google 自己挑
	lang, name := voiceLang, voiceName
	if hasCJK(text) {
		lang, name = "cmn-CN", ""
	}

	ck := cacheFile("tts", text+"|"+lang+"|"+name, ".mp3")
	if _, err := os.Stat(ck); err == nil {
		serveMP3(w, r, ck)
		return
	}

	voice := map[string]string{"languageCode": lang}
	if name != "" {
		voice["name"] = name
	}
	reqBody, _ := json.Marshal(map[string]any{
		"input":       map[string]string{"text": text},
		"voice":       voice,
		"audioConfig": map[string]string{"audioEncoding": "MP3"},
	})
	resp, err := http.Post(ttsAPI+"?key="+url.QueryEscape(ttsKey),
		"application/json", strings.NewReader(string(reqBody)))
	if err != nil {
		http.Error(w, "朗读请求失败（网络？）："+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		http.Error(w, "TTS API 返回 "+resp.Status+"："+squash(body), http.StatusBadGateway)
		return
	}

	var parsed struct {
		AudioContent string `json:"audioContent"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.AudioContent == "" {
		http.Error(w, "TTS 结果看不懂："+squash(body), http.StatusBadGateway)
		return
	}
	mp3, err := base64.StdEncoding.DecodeString(parsed.AudioContent)
	if err != nil {
		http.Error(w, "音频解码失败："+err.Error(), http.StatusBadGateway)
		return
	}

	writeCache(ck, mp3)
	serveMP3(w, r, ck)
}

// serveMP3 一律用 http.ServeContent 吐音频，别自己 w.Write。
//
// 自己写的话 Go 会用 Transfer-Encoding: chunked、不带 Content-Length，而且
// 浏览器给媒体发的 Range 请求也只会得到 200 而不是 206 —— <audio> 碰上这种响应
// 就直接判定「no supported source found」，报错里完全看不出是这个原因。
// ServeContent 会把 Content-Length、Range/206、Last-Modified 都处理好。
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

// squash 把 API 的报错压成一行塞进 HTTP 错误里，太长的截断
func squash(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
