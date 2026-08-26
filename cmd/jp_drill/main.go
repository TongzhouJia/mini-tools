// jp_drill —— 日语单词自测器。
//
// 解决的问题：对着单词表「看」，看的是「日语→中文」，而且中文就印在旁边。
// 这叫「再认」，几乎不产生长期记忆——你以为记住了，其实只是看着眼熟。
// 真正管用的是「提取」：只给中文，逼自己把日语从脑子里捞出来，捞不出来才算不会。
//
// 所以这个工具默认反着来：显示中文，你心里默答，按空格翻面，自己判会不会。
// 判「不会」的留在本轮继续转，判「会」的要连续两次才毕业——一次蒙对不算。
//
// 第一遍跑的意义是「砍表」：43 个词里通常有一大半是早就会的 N5 词，
// 它们混在表里稀释复习时间，还制造「我连这个都记不住」的挫败感。跑一轮就筛干净了。
//
//	jp_drill -f ~/Desktop/记不住🤡.csv        中文→日语（默认，练提取）
//	jp_drill -f xxx.csv -mode jp2zh          日语→中文（只练认脸，热身用）
//	jp_drill -f xxx.csv -prune               跑完把「还没毕业」的写回一个新 csv
//
// CSV 列：文,假名,中文,音频（音频列可空，不用）。
package main

import (
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type word struct {
	Kanji string // 文
	Kana  string // 假名
	Zh    string // 中文
}

// 跨会话的战绩，key 用「文」。
type record struct {
	Seen    int       `json:"seen"`     // 一共问过几次
	Wrong   int       `json:"wrong"`    // 一共答错几次
	Streak  int       `json:"streak"`   // 当前连对
	LastHit time.Time `json:"last_hit"` // 最近一次答对
}

var stateDir = func() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "jp_drill_data"
	}
	return filepath.Join(home, ".local", "share", "jp_drill")
}()

const graduate = 2 // 连对几次算本轮毕业

func main() {
	var (
		file   string
		mode   string
		prune  bool
		shuffl bool
	)
	flag.StringVar(&file, "f", "", "词表 csv（列：文,假名,中文,音频）")
	flag.StringVar(&mode, "mode", "zh2jp", "zh2jp=看中文说日语（练提取）；jp2zh=看日语说中文（只练认脸）")
	flag.BoolVar(&prune, "prune", false, "跑完把没毕业的词写回 <原名>-还没记住.csv")
	flag.BoolVar(&shuffl, "shuffle", true, "打乱顺序（关掉就按原顺序，会靠位置记忆作弊）")
	flag.Parse()

	if file == "" {
		fmt.Fprintln(os.Stderr, "要给个词表：jp_drill -f ~/Desktop/xxx.csv")
		os.Exit(2)
	}
	words, err := loadCSV(file)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读词表失败：", err)
		os.Exit(1)
	}
	if len(words) == 0 {
		fmt.Fprintln(os.Stderr, "词表是空的")
		os.Exit(1)
	}

	st := loadState()
	defer saveState(st)

	restore, err := rawMode()
	if err != nil {
		fmt.Fprintln(os.Stderr, "进不了 raw 模式（换个终端试试）：", err)
		os.Exit(1)
	}
	defer restore()

	fmt.Printf("\n共 %d 个词，模式 %s。\n", len(words), mode)
	fmt.Println("看到提示先在心里答，再按 [空格] 翻面；翻面后 [空格]=记住了  [n]=没记住  [q]=退出\n")

	// 本轮队列：没毕业的一直转回来。
	queue := append([]word(nil), words...)
	if shuffl {
		rand.Shuffle(len(queue), func(i, j int) { queue[i], queue[j] = queue[j], queue[i] })
	}
	roundStreak := map[string]int{} // 本次会话的连对，跟跨会话的分开
	firstTry := map[string]bool{}   // 第一遍就答对的
	asked := map[string]bool{}

	round := 1
	for len(queue) > 0 {
		fmt.Printf("── 第 %d 轮 · 还剩 %d 个 ──\n\n", round, len(queue))
		var next []word
		for _, w := range queue {
			ok, quit := ask(w, mode)
			if quit {
				report(words, roundStreak, firstTry, st)
				if prune {
					writePrune(file, words, roundStreak)
				}
				return
			}
			r := st[w.Kanji]
			r.Seen++
			if ok {
				r.Streak++
				r.LastHit = time.Now()
				roundStreak[w.Kanji]++
				if !asked[w.Kanji] {
					firstTry[w.Kanji] = true
				}
			} else {
				r.Streak = 0
				r.Wrong++
				roundStreak[w.Kanji] = 0
			}
			st[w.Kanji] = r
			asked[w.Kanji] = true

			if roundStreak[w.Kanji] < graduate {
				next = append(next, w)
			}
		}
		queue = next
		if shuffl {
			rand.Shuffle(len(queue), func(i, j int) { queue[i], queue[j] = queue[j], queue[i] })
		}
		round++
	}
	report(words, roundStreak, firstTry, st)
	if prune {
		writePrune(file, words, roundStreak)
	}
}

// ask 出一道题，返回（答对了吗，要退出吗）。
func ask(w word, mode string) (bool, bool) {
	front, back := w.Zh, w.Kanji+"　"+w.Kana
	if w.Kanji == w.Kana {
		back = w.Kana
	}
	if mode == "jp2zh" {
		front, back = w.Kanji, w.Kana+"　"+w.Zh
	}

	fmt.Printf("  \033[1;36m%s\033[0m", front)
	for {
		k := readKey()
		if k == 'q' {
			fmt.Println("\n")
			return false, true
		}
		if k == ' ' || k == '\r' || k == '\n' {
			break
		}
	}
	fmt.Printf("\r\033[2K  \033[1;36m%s\033[0m  →  \033[1;33m%s\033[0m", front, back)
	fmt.Print("\n     [空格]记住了  [n]没记住  [q]退出  ")
	for {
		switch k := readKey(); k {
		case ' ', '\r', '\n', 'y', 'j':
			fmt.Print("\r\033[2K     \033[32m✓ 记住了\033[0m\n\n")
			return true, false
		case 'n', 'f':
			fmt.Print("\r\033[2K     \033[31m✗ 没记住，等下再来\033[0m\n\n")
			return false, false
		case 'q':
			fmt.Println("\n")
			return false, true
		}
	}
}

func report(words []word, roundStreak map[string]int, firstTry map[string]bool, st map[string]record) {
	var clean, stuck []word
	for _, w := range words {
		if firstTry[w.Kanji] {
			clean = append(clean, w)
		}
		if roundStreak[w.Kanji] < graduate {
			stuck = append(stuck, w)
		}
	}
	fmt.Printf("\n════ 结果 ════\n")
	fmt.Printf("第一遍就答对：%d / %d —— 这些本来就会，从表里删掉，别再占复习时间。\n", len(clean), len(words))
	if len(clean) > 0 {
		fmt.Println("  " + join(clean))
	}
	fmt.Printf("还没拿下：%d 个 —— 这才是你真正要背的量。\n", len(stuck))
	if len(stuck) > 0 {
		fmt.Println("  " + join(stuck))
	}

	// 跨会话累计错得最多的，排前面。
	type kv struct {
		k string
		r record
	}
	var all []kv
	for k, r := range st {
		if r.Wrong > 0 {
			all = append(all, kv{k, r})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].r.Wrong > all[j].r.Wrong })
	if len(all) > 0 {
		n := min(8, len(all))
		fmt.Printf("\n历史累计最难的 %d 个（错次数）：", n)
		for _, e := range all[:n] {
			fmt.Printf(" %s(%d)", e.k, e.r.Wrong)
		}
		fmt.Println()
	}
	fmt.Println()
}

func join(ws []word) string {
	var s []string
	for _, w := range ws {
		s = append(s, w.Kanji)
	}
	return strings.Join(s, " ")
}

func writePrune(src string, words []word, roundStreak map[string]int) {
	ext := filepath.Ext(src)
	out := strings.TrimSuffix(src, ext) + "-还没记住" + ext
	f, err := os.Create(out)
	if err != nil {
		fmt.Fprintln(os.Stderr, "写不出来：", err)
		return
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	w.Write([]string{"文", "假名", "中文", "音频"})
	n := 0
	for _, x := range words {
		if roundStreak[x.Kanji] < graduate {
			w.Write([]string{x.Kanji, x.Kana, x.Zh, ""})
			n++
		}
	}
	fmt.Printf("没毕业的 %d 个已写到：%s\n\n", n, out)
}

func loadCSV(path string) ([]word, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	rows, err := r.ReadAll()
	if err != nil {
		return nil, err
	}
	var out []word
	for i, row := range rows {
		if len(row) < 3 {
			continue
		}
		if i == 0 && (row[0] == "文" || row[0] == "単語" || row[0] == "word") {
			continue // 表头
		}
		out = append(out, word{Kanji: row[0], Kana: row[1], Zh: row[2]})
	}
	return out, nil
}

func loadState() map[string]record {
	st := map[string]record{}
	b, err := os.ReadFile(filepath.Join(stateDir, "state.json"))
	if err != nil {
		return st
	}
	json.Unmarshal(b, &st)
	return st
}

func saveState(st map[string]record) {
	os.MkdirAll(stateDir, 0o755)
	b, _ := json.MarshalIndent(st, "", "  ")
	os.WriteFile(filepath.Join(stateDir, "state.json"), b, 0o644)
}

// rawMode 借 stty 进单键模式，省掉每次都要敲回车。返回恢复函数。
func rawMode() (func(), error) {
	saved, err := exec.Command("stty", "-F", "/dev/tty", "-g").Output()
	if err != nil {
		return nil, err
	}
	if err := exec.Command("stty", "-F", "/dev/tty", "cbreak", "-echo").Run(); err != nil {
		return nil, err
	}
	return func() {
		exec.Command("stty", "-F", "/dev/tty", strings.TrimSpace(string(saved))).Run()
		fmt.Print("\033[0m")
	}, nil
}

func readKey() byte {
	var b [1]byte
	tty, err := os.Open("/dev/tty")
	if err != nil {
		return 'q'
	}
	defer tty.Close()
	if _, err := tty.Read(b[:]); err != nil {
		return 'q'
	}
	return b[0]
}
