package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ── 把单词表和 mp3/srt 配上对 ──────────────────────────────────────────

func attachMedia(d *Deck, mediaDir string) {
	mp3s, _ := filepath.Glob(filepath.Join(mediaDir, "*.mp3"))
	for _, ep := range d.Eps {
		want := fold(ep.Title)
		for _, m := range mp3s {
			stem := strings.TrimSuffix(filepath.Base(m), ".mp3")
			// 音频名前面挂日期、后面可能多挂年份，所以两个方向都试
			got := fold(stem)
			if !strings.Contains(got, want) && !strings.HasPrefix(want, got) {
				continue
			}
			srt := strings.TrimSuffix(m, ".mp3") + ".srt"
			if _, err := os.Stat(srt); err != nil {
				continue
			}
			ep.MP3 = m
			ep.Cues = parseSRT(srt)
			buildIndex(ep)
			break
		}
	}
}

var timing = regexp.MustCompile(`(\d\d):(\d\d):(\d\d)[,.](\d\d\d)\s*-->\s*(\d\d):(\d\d):(\d\d)[,.](\d\d\d)`)

func parseSRT(path string) []Cue {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var cues []Cue
	var cur *Cue
	for _, line := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
		if m := timing.FindStringSubmatch(line); m != nil {
			cues = append(cues, Cue{Start: secs(m[1:5]), End: secs(m[5:9])})
			cur = &cues[len(cues)-1]
			continue
		}
		t := strings.TrimSpace(line)
		if t == "" {
			// 空行 = 一块结束。必须在这儿断开，否则下一块开头那行序号
			// 会被当成台词拼到上一条尾巴上（"…of the coral hosts 1983 more…"）
			cur = nil
			continue
		}
		if cur == nil {
			continue // 序号行，或者时间轴还没出现
		}
		if cur.Text != "" {
			cur.Text += " "
		}
		cur.Text += t
	}
	return cues
}

func secs(p []string) float64 {
	h, _ := strconv.Atoi(p[0])
	m, _ := strconv.Atoi(p[1])
	s, _ := strconv.Atoi(p[2])
	ms, _ := strconv.Atoi(p[3])
	return float64(h*3600+m*60+s) + float64(ms)/1000
}

func buildIndex(ep *Episode) {
	ep.index = map[string][]int{}
	ep.flat = make([]string, len(ep.Cues))
	for i, c := range ep.Cues {
		f := flatten(c.Text)
		ep.flat[i] = f
		for _, tok := range strings.Fields(f) {
			if len(tok) < 2 {
				continue
			}
			if n := ep.index[tok]; len(n) == 0 || n[len(n)-1] != i {
				ep.index[tok] = append(ep.index[tok], i)
			}
		}
	}
}

// ── 给一张卡找原句 ────────────────────────────────────────────────────

// forms 拿词表里的原形去凑转录稿里可能出现的形态。
// 不做真词形还原 —— 找不到就没音频，这张卡退回认脸模式，代价可以接受。
func forms(w string) []string {
	out := []string{w}
	add := func(ss ...string) { out = append(out, ss...) }
	switch {
	case strings.HasSuffix(w, "e"):
		stem := w[:len(w)-1]
		add(w+"s", w+"d", stem+"ing")
	case strings.HasSuffix(w, "y"):
		stem := w[:len(w)-1]
		add(w+"s", w+"ing", stem+"ies", stem+"ied")
	case strings.HasSuffix(w, "s"), strings.HasSuffix(w, "x"), strings.HasSuffix(w, "ch"), strings.HasSuffix(w, "sh"):
		add(w+"es", w+"ed", w+"ing")
	default:
		add(w+"s", w+"ed", w+"ing", w+w[len(w)-1:]+"ing", w+w[len(w)-1:]+"ed")
	}
	return out
}

var stop = map[string]bool{
	"a": true, "an": true, "the": true, "to": true, "of": true, "in": true, "on": true,
	"at": true, "by": true, "for": true, "up": true, "out": true, "off": true, "as": true,
	"is": true, "it": true, "be": true, "no": true, "so": true, "and": true, "or": true,
	"one": true, "ones": true, "sb": true, "sth": true, "s": true, "with": true, "from": true,
}

const (
	maxHits = 3
	maxSolo = 4 // 独苗兜底词最多允许在本集出现几次
)

func findHits(d *Deck, c *Card) []Hit {
	var hits []Hit
	seen := map[string]bool{}
	for _, code := range c.Eps {
		ep := d.Eps[code]
		if ep == nil || ep.index == nil {
			continue
		}
		for _, i := range cueCandidates(ep, c.ID) {
			key := code + ":" + strconv.Itoa(i)
			if seen[key] {
				continue
			}
			seen[key] = true
			hits = append(hits, clip(ep, i))
			if len(hits) >= maxHits {
				return hits
			}
		}
	}
	return hits
}

func cueCandidates(ep *Episode, phrase string) []int {
	words := strings.Fields(phrase)
	if len(words) == 1 {
		var out []int
		for _, f := range forms(words[0]) {
			out = append(out, ep.index[f]...)
		}
		sort.Ints(out)
		return dedupe(out)
	}

	// 多词条目：先按字面整串找，找不到就退到串里最罕见的那个实词。
	// 「at one's disposal」转录稿里说的是 at his disposal，字面必然落空，
	// 但 disposal 全集出现不了几次，落到它头上八九不离十。
	need := strings.TrimSpace(flatten(phrase))
	var out []int
	for i, f := range ep.flat {
		if strings.Contains(f, " "+need+" ") {
			out = append(out, i)
		}
	}
	if len(out) > 0 {
		return out
	}

	// 兜底：短语字面没命中时，靠串里的实词去定位。
	// 实词也要过一遍词形（转录稿说的是 alluded to / figured out），
	// 而且有两个以上实词时必须落在同一条字幕里 ——
	// 只认单个词的话，「periodic table」会挂到「periodic cycle」上，
	// 「in one's own right」会挂到「he got it right first time」上。
	var sets [][]int
	for _, w := range words {
		if stop[w] || len(w) < 5 {
			continue
		}
		var all []int
		for _, f := range forms(w) {
			all = append(all, ep.index[f]...)
		}
		sort.Ints(all)
		if all = dedupe(all); len(all) > 0 {
			sets = append(sets, all)
		}
	}
	switch {
	case len(sets) == 0:
		return nil
	case len(sets) >= 2:
		got := sets[0]
		for _, x := range sets[1:] {
			got = intersect(got, x)
		}
		return got
	}
	// 只有一个实词撑着，那它得够罕见才说明得了问题
	if len(sets[0]) > maxSolo {
		return nil
	}
	return sets[0]
}

func intersect(a, b []int) []int {
	m := map[int]bool{}
	for _, x := range b {
		m[x] = true
	}
	var out []int
	for _, x := range a {
		if m[x] {
			out = append(out, x)
		}
	}
	return out
}

func dedupe(xs []int) []int {
	var out []int
	for i, x := range xs {
		if i == 0 || xs[i-1] != x {
			out = append(out, x)
		}
	}
	return out
}

// clip 把一条字幕切成能听的片段。
// whisper 切出来的一条常常只有一两秒、半句话就断了，
// 短于 2.5 秒就把下一条也接上，不然听见的是个残句，回想不出任何东西。
func clip(ep *Episode, i int) Hit {
	c := ep.Cues[i]
	start, end, text := c.Start, c.End, c.Text
	for j := i + 1; j < len(ep.Cues) && end-start < 2.5; j++ {
		end = ep.Cues[j].End
		text += " " + ep.Cues[j].Text
	}
	if start > 0.25 {
		start -= 0.25
	}
	return Hit{Ep: ep.Code, Start: start, End: end + 0.25, Text: strings.TrimSpace(text)}
}
