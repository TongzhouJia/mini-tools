package main

import (
	"encoding/csv"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// ── 一集播客 ──────────────────────────────────────────────────────────
//
// 单词表叫「07_The Challenger Expedition_单词表.csv」，
// 音频却叫「2022-12-22 The Challenger Expedition 1872-1876.mp3」——
// 前面带日期、后面可能多个年份、Erdős 的 ő 在 csv 里被写成了 o。
// 所以配对不能按文件名相等，只能把两边都折平了再前缀匹配（见 fold）。

type Cue struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Text  string  `json:"text"`
}

type Episode struct {
	Code  string `json:"code"`  // 单词表前缀的两位数，同时当 /audio/ 的 URL
	Title string `json:"title"`
	MP3   string `json:"-"`
	Cues  []Cue  `json:"-"`
	index map[string][]int // 词 → 出现在哪几条字幕，避免 2000 词 × 1500 条硬扫
	flat  []string         // 每条字幕折平成「小写 + 只留字母数字空格」，给短语做子串匹配
}

// ── 一张卡 ────────────────────────────────────────────────────────────

type Hit struct {
	Ep    string  `json:"ep"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Text  string  `json:"text"`
}

type Card struct {
	ID   string   `json:"id"`   // 折平后的词，同时是存档主键 —— 别改它，改了进度就对不上了
	Word string   `json:"word"` // csv 里的原样，带 (pl. nuclei) 这种尾巴
	Def  string   `json:"def"`
	POS  string   `json:"pos"`
	Eps  []string `json:"eps"`
	Tier string   `json:"tier"` // core / extra
	Mode string   `json:"mode"` // listen / produce / recognize
	Hits []Hit    `json:"hits"`
}

type Deck struct {
	Cards []*Card             `json:"cards"`
	Eps   map[string]*Episode `json:"eps"`
}

// ── 折平 ──────────────────────────────────────────────────────────────

// fold 把标题/词折成只剩小写字母数字，顺手把 ő 这类带音符的字母拆掉音符。
// NFD 之后组合音符是独立的 Mn 类字符，滤掉就剩裸字母。
func fold(s string) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(strings.ToLower(s)) {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// flatten 保留词的边界，给短语做子串匹配用
func flatten(s string) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(strings.ToLower(s)) {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			b.WriteRune(' ')
		}
	}
	return " " + strings.Join(strings.Fields(b.String()), " ") + " "
}

var parenTail = regexp.MustCompile(`\s*[（(].*?[)）]\s*`)

// normWord 去掉 csv 里的括号注释：「nucleus (pl. nuclei)」→「nucleus」
func normWord(w string) string {
	w = parenTail.ReplaceAllString(w, " ")
	w = strings.TrimSpace(strings.ToLower(w))
	// 「émigré / emigre」这种给了俩拼法，取前一个
	if i := strings.Index(w, " / "); i >= 0 {
		w = w[:i]
	}
	return strings.Join(strings.Fields(w), " ")
}

var posHead = regexp.MustCompile(`^\s*((?:[a-z]+\.\s*/?\s*)+)`)

func posOf(def string) string {
	m := posHead.FindStringSubmatch(def)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// ── 读单词表 ──────────────────────────────────────────────────────────

var csvName = regexp.MustCompile(`^(\d+)_(.+)_单词表\.csv$`)

func loadDeck(srcDir, mediaDir string) (*Deck, error) {
	files, err := filepath.Glob(filepath.Join(srcDir, "*单词表.csv"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)

	d := &Deck{Eps: map[string]*Episode{}}
	byID := map[string]*Card{}
	var order []string

	for _, f := range files {
		m := csvName.FindStringSubmatch(filepath.Base(f))
		if m == nil {
			continue
		}
		ep := &Episode{Code: m[1], Title: m[2]}
		d.Eps[ep.Code] = ep

		rows, err := readCSV(f)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			id := normWord(r[0])
			if id == "" {
				continue
			}
			c, ok := byID[id]
			if !ok {
				c = &Card{ID: id, Word: strings.TrimSpace(r[0]), Def: strings.TrimSpace(r[1]), POS: posOf(r[1])}
				byID[id] = c
				order = append(order, id)
			}
			// 同一个词在别集里又出现 —— 只记一笔出处，释义以头一次为准
			if !contains(c.Eps, ep.Code) {
				c.Eps = append(c.Eps, ep.Code)
			}
		}
	}

	for _, id := range order {
		c := byID[id]
		// 分层：跨集复现的，加上全部短语 —— 剩下 2/3 是只在本集出现过一次的
		// 学科名词（helium / lipid / endosymbiont），概念早就有，不值得占复习位
		if len(c.Eps) >= 2 || strings.HasPrefix(c.POS, "phr") {
			c.Tier = "core"
		} else {
			c.Tier = "extra"
		}
		d.Cards = append(d.Cards, c)
	}

	attachMedia(d, mediaDir)
	for _, c := range d.Cards {
		c.Hits = findHits(d, c)
		switch {
		case strings.HasPrefix(c.POS, "phr"):
			c.Mode = "produce" // 成语性短语的中文释义几乎是唯一解，反着考才有信息量
		case len(c.Hits) > 0:
			c.Mode = "listen" // 有原句就听原句：练的正好是听懂播客那个技能
		default:
			c.Mode = "recognize"
		}
	}
	return d, nil
}

func readCSV(path string) ([][]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	r := csv.NewReader(strings.NewReader(strings.TrimPrefix(string(b), "\ufeff")))
	r.FieldsPerRecord = -1
	rows, err := r.ReadAll()
	if err != nil {
		return nil, err
	}
	var out [][]string
	for i, row := range rows {
		if i == 0 || len(row) < 2 || strings.TrimSpace(row[0]) == "" {
			continue
		}
		out = append(out, row)
	}
	return out, nil
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
