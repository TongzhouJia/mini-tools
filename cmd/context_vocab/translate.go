package main

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// 跟 pdf_reader / tts_downloader 一个路子：REST + ?key=，不引 GCP SDK。
// Key 从环境变量或仓库根目录的 .env 读（.env 在 .gitignore 里，别提交）。
var translateKey string

const (
	translateAPI = "https://translation.googleapis.com/language/translate/v2"
	maxTextLen   = 3000
)

// loadEnv 手动读 .env：KEY=VALUE，# 开头是注释。
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

func initTranslate(envPath string) {
	loadEnv(envPath)
	translateKey = strings.TrimSpace(os.Getenv("GOOGLE_TRANSLATE_API_KEY"))
}

func cacheFile(text string) string {
	sum := sha1.Sum([]byte(text + "|" + transTo))
	return filepath.Join(dataDir, "cache", "translate", hex.EncodeToString(sum[:])+".json")
}

type translateResp struct {
	Text string `json:"text"`
	From string `json:"from"`
}

// translateText 打一次 Cloud Translation，结果按 sha1 存盘。
// 同一个词圈第二遍不会重复烧配额。
func translateText(text string) (translateResp, error) {
	var out translateResp
	text = strings.TrimSpace(text)
	if text == "" {
		return out, fmt.Errorf("没给要翻译的文字")
	}
	if len(text) > maxTextLen {
		return out, fmt.Errorf("太长了（%d 字节，上限 %d）", len(text), maxTextLen)
	}
	if translateKey == "" {
		return out, fmt.Errorf("没配 GOOGLE_TRANSLATE_API_KEY（放进仓库根目录的 .env）")
	}

	ck := cacheFile(text)
	if b, err := os.ReadFile(ck); err == nil {
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
		if os.MkdirAll(filepath.Dir(ck), 0o755) == nil { // 缓存写不进去不算错
			os.WriteFile(ck, b, 0o644)
		}
	}
	return out, nil
}

func handleTranslate(w http.ResponseWriter, r *http.Request) {
	out, err := translateText(r.URL.Query().Get("text"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, out)
}

// squash 把一坨报错压成一行，塞进日志/错误信息里好看
func squash(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
