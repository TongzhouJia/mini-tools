package main

// 每天中午的单词日报：昨天新收了哪些词 + 今天该复习哪些词，纯文字发到邮箱。
//
// 复习节奏按艾宾浩斯那套递增间隔来，从「第一次收这个词」的那天起算：
// 第 1 天就是「昨天新增」那一栏（所以间隔表里不重复放 1），之后 2/4/7/15/30/60 天各回顾一次。
// 改节奏：-review-days 2,4,7 或环境变量 VOCAB_REVIEW_DAYS。
//
// 词是按「词」聚合的，不是按句子：同一个词在多个句子里出现过，只报最近那条例句。

import (
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

const defaultReviewDays = "2,4,7,15,30,60"

// card 是日报里的一个词条。
type card struct {
	Word     string
	Zh       string
	Sentence string
	First    time.Time // 第一次收这个词的日子，复习进度从这天算
	Latest   time.Time // 最近一次在哪句里见到它

	zhAt     time.Time // 现在这个释义是哪条给的
	sentAt   time.Time // 现在这个例句是哪条给的
	realSent bool      // 这例句是真句子，还是「没写例句、拿词顶上」的那种
}

func parseReviewDays(s string) []int {
	var out []int
	seen := map[int]bool{}
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil || n <= 0 || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}

func parseTime(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(s))
	if err != nil {
		return time.Time{}, false
	}
	return t.Local(), true
}

// dayDiff 算两个时刻差几个自然日（按本地日历，不是 24 小时整除）。
func dayDiff(from, to time.Time) int {
	a := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.Local)
	b := time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, time.Local)
	return int(b.Sub(a).Hours() / 24)
}

// collectCards 把所有句子摊平成一个个词。同一个词只出一张卡：
// First 取最早（复习进度按它算），释义取最近一条非空的，
// 例句**优先带上下文的真句子**——手机上只记了个孤词的那条不该把旧例句冲掉。
func collectCards() []card {
	mu.Lock()
	defer mu.Unlock()

	byWord := map[string]*card{}
	for _, e := range entries {
		t, ok := parseTime(e.CreatedAt)
		if !ok {
			continue
		}
		for _, m := range e.Marks {
			w := m.Word()
			if w == "" {
				continue
			}
			key := strings.ToLower(w)
			c := byWord[key]
			if c == nil {
				c = &card{Word: w, First: t, Latest: t}
				byWord[key] = c
			}

			if t.Before(c.First) {
				c.First = t
			}
			if t.After(c.Latest) {
				c.Latest = t
			}

			if zh := strings.TrimSpace(m.Zh); zh != "" && !t.Before(c.zhAt) {
				c.Zh, c.zhAt, c.Word = zh, t, w
			}

			sent := strings.TrimSpace(e.Sentence)
			real := sent != "" && !strings.EqualFold(sent, w)
			better := real && !c.realSent // 真句子永远赢过孤词，哪怕它更旧
			if c.Sentence == "" || better || (real == c.realSent && !t.Before(c.sentAt)) {
				c.Sentence, c.sentAt, c.realSent = sent, t, real
			}
		}
	}

	out := make([]card, 0, len(byWord))
	for _, c := range byWord {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].First.Equal(out[j].First) {
			return out[i].First.Before(out[j].First)
		}
		return strings.ToLower(out[i].Word) < strings.ToLower(out[j].Word)
	})
	return out
}

func mdate(t time.Time) string { return fmt.Sprintf("%d/%d", int(t.Month()), t.Day()) }

// line 一个词占两行：词 + 释义，例句另起一行缩进。没例句（只写了词）就只占一行。
func line(i int, c card) string {
	head := fmt.Sprintf("%2d. %s", i, c.Word)
	if c.Zh != "" {
		head += "  " + c.Zh
	}
	if s := strings.TrimSpace(c.Sentence); s != "" && !strings.EqualFold(s, c.Word) {
		head += "\n      " + strings.ReplaceAll(s, "\n", " ")
	}
	return head
}

// buildDigest 拼出一封信。now 一般就是现在，测试时可以塞别的日子进去。
// send=false 表示昨天一个新词都没记——那天不发信（他明说的，别拿空信烦他）。
func buildDigest(now time.Time, reviewDays []int) (subject, body string, send bool) {
	cards := collectCards()

	var fresh []card        // 昨天新收的
	due := map[int][]card{} // 间隔天数 -> 今天该复习的词
	inReview := map[int]bool{}
	for _, d := range reviewDays {
		inReview[d] = true
	}

	for _, c := range cards {
		d := dayDiff(c.First, now)
		switch {
		case d == 1:
			fresh = append(fresh, c)
		case inReview[d]:
			due[d] = append(due[d], c)
		}
	}
	if len(fresh) == 0 {
		return "", "", false
	}

	dueCount := 0
	for _, cs := range due {
		dueCount += len(cs)
	}
	subject = fmt.Sprintf("单词 %s · 新收 %d · 复习 %d", mdate(now), len(fresh), dueCount)

	var b strings.Builder
	fmt.Fprintf(&b, "昨天（%s）新收 %d 个\n", mdate(now.AddDate(0, 0, -1)), len(fresh))
	for i, c := range fresh {
		b.WriteString(line(i+1, c) + "\n")
	}

	if dueCount > 0 {
		fmt.Fprintf(&b, "\n今天该复习 %d 个\n", dueCount)
		for _, d := range reviewDays {
			cs := due[d]
			if len(cs) == 0 {
				continue
			}
			fmt.Fprintf(&b, "\n【%d 天前 · %s】\n", d, mdate(now.AddDate(0, 0, -d)))
			for i, c := range cs {
				b.WriteString(line(i+1, c) + "\n")
			}
		}
	}

	fmt.Fprintf(&b, "\n%s\n", webURL)
	return subject, b.String(), true
}

// sendMail 外调 gmail-send（默认就发给他自己）。正文走 stdin，长了也不会撑爆命令行。
func sendMail(subject, body string) error {
	if _, err := exec.LookPath("gmail-send"); err != nil {
		return fmt.Errorf("找不到 gmail-send 命令（应该在 ~/.local/bin/gmail-send）：%w", err)
	}
	cmd := exec.Command("gmail-send", subject)
	cmd.Stdin = strings.NewReader(body)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gmail-send 失败：%v（%s）", err, strings.TrimSpace(string(out)))
	}
	return nil
}
