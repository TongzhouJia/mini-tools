package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 单词本：一天一个文件，day01.csv / day02.csv……跟 data/学外语/daily_english_word/
// 那批 dayNN.txt 一个路子。每个文件两列：英文,中文，带表头。
//
// 这个格式跟 vocabulary_comparison 读 vocab.csv 的方式是对得上的
// （跳过首行表头，之后按逗号切，第 0 列单词第 1 列释义），所以攒完可以直接喂给它。
const (
	wordsDirName = "单词"
	daysMapName  = ".days.json" // 日期 -> 第几天，不然重启后不知道今天是 day 几
)

var wordsHeader = []string{"英文", "中文"}

var wordsMu sync.Mutex

func wordsDir() string { return filepath.Join(dataDir, wordsDirName) }

// dayFileFor 返回某个日期该写哪个文件，没有就新分配一个天数。
// 天数按「已经攒过多少天」递增，不是日期差 —— 中间断几天不会跳号。
func dayFileFor(date string) (string, int, error) {
	if err := os.MkdirAll(wordsDir(), 0o755); err != nil {
		return "", 0, err
	}
	mapPath := filepath.Join(wordsDir(), daysMapName)

	m := map[string]int{}
	if b, err := os.ReadFile(mapPath); err == nil {
		json.Unmarshal(b, &m) // 坏了就当空的重来，大不了重新编号
	}
	if n, ok := m[date]; ok {
		return filepath.Join(wordsDir(), fmt.Sprintf("day%02d.csv", n)), n, nil
	}

	next := 1
	for _, n := range m {
		if n >= next {
			next = n + 1
		}
	}
	m[date] = next
	if b, err := json.MarshalIndent(m, "", "  "); err == nil {
		os.WriteFile(mapPath, b, 0o644)
	}
	return filepath.Join(wordsDir(), fmt.Sprintf("day%02d.csv", next)), next, nil
}

type wordEntry struct {
	En  string `json:"en"`
	Zh  string `json:"zh"`
	Day int    `json:"day"`
}

// readDayFile 读一个 dayNN.csv，跳过表头
func readDayFile(path string, day int) ([]wordEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	rd := csv.NewReader(f)
	rd.FieldsPerRecord = -1
	rows, err := rd.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("%s 读不动（手改坏了？）：%w", filepath.Base(path), err)
	}

	var out []wordEntry
	for i, row := range rows {
		if i == 0 || len(row) == 0 {
			continue
		}
		en := strings.TrimSpace(row[0])
		if en == "" {
			continue
		}
		zh := ""
		if len(row) > 1 {
			zh = strings.TrimSpace(row[1])
		}
		out = append(out, wordEntry{En: en, Zh: zh, Day: day})
	}
	return out, nil
}

// allWords 把所有 dayNN.csv 读出来，按天数排好
func allWords() ([]wordEntry, error) {
	entries, err := os.ReadDir(wordsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return []wordEntry{}, nil
		}
		return nil, err
	}

	var out []wordEntry
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "day") || !strings.HasSuffix(name, ".csv") {
			continue
		}
		day, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "day"), ".csv"))
		if err != nil {
			continue
		}
		got, err := readDayFile(filepath.Join(wordsDir(), name), day)
		if err != nil {
			return nil, err
		}
		out = append(out, got...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Day < out[j].Day })
	if out == nil {
		out = []wordEntry{}
	}
	return out, nil
}

func handleWords(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		wordsMu.Lock()
		items, err := allWords()
		wordsMu.Unlock()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(items)

	case http.MethodPost:
		var body struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "请求体不对："+err.Error(), http.StatusBadRequest)
			return
		}
		en := strings.TrimSpace(body.Text)
		if en == "" {
			http.Error(w, "没选中任何文字", http.StatusBadRequest)
			return
		}

		// 翻译挂了也照存，中文列留空，手工补就是了 —— 别因为翻译失败把单词丢了
		zh, warn := "", ""
		if tr, err := translateText(en); err != nil {
			warn = "翻译没成功：" + err.Error()
		} else {
			zh = tr.Text
		}

		res, err := addWord(en, zh)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		res["warn"] = warn
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(res)

	default:
		http.Error(w, "只收 GET / POST", http.StatusMethodNotAllowed)
	}
}

// addWord 把单词写进今天那个文件。已经攒过的（哪天攒的都算）就不重复写。
func addWord(en, zh string) (map[string]any, error) {
	wordsMu.Lock()
	defer wordsMu.Unlock()

	existing, err := allWords()
	if err != nil {
		return nil, err
	}
	for _, e := range existing {
		if strings.EqualFold(e.En, en) {
			return map[string]any{
				"ok": true, "dup": true, "day": e.Day, "en": e.En, "zh": e.Zh,
				"total": len(existing),
			}, nil
		}
	}

	path, day, err := dayFileFor(time.Now().Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	_, statErr := os.Stat(path)

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	cw := csv.NewWriter(f)
	if os.IsNotExist(statErr) {
		// 头一次写：先来个 BOM，不然 Excel 打开中文是乱码
		if _, err := f.WriteString("\uFEFF"); err != nil {
			return nil, err
		}
		if err := cw.Write(wordsHeader); err != nil {
			return nil, err
		}
	}
	if err := cw.Write([]string{en, zh}); err != nil {
		return nil, err
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return nil, err
	}

	return map[string]any{
		"ok": true, "dup": false, "day": day, "en": en, "zh": zh,
		"file": filepath.Base(path), "total": len(existing) + 1,
	}, nil
}
