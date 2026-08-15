package main

// 从 Google Tasks 的「单词积累」列表把词导进来。
//
// 他在手机上随手往那个列表里加词，格式是「英文+中文」当标题、例句写在详细说明里：
//
//	Permissive宽容的
//	Demonstrate证明
//	  💬 这个单词造的句子Sample
//
// 导进来之后跟网页里手动收的词是同一份数据（entries.jsonl），
// 例句为空时就拿单词本身当句子，保证 UI / 导出 / 日报都不用特判。
//
// 认证不自己做，直接外调 ~/.local/bin/gtasks（那边已经有 refresh token +
// SSL 抽风重试 + 翻页）。任务不动不删——他手机上那份是他自己的账本。

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
	"unicode/utf16"
)

const defaultTasksList = "单词积累"

// gtask 只挑用得上的字段，其余原样丢掉。
type gtask struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Notes   string `json:"notes"`
	Updated string `json:"updated"` // RFC3339，他最后一次动这条的时间
	Status  string `json:"status"`
}

func fetchTasks(list string) ([]gtask, error) {
	if _, err := exec.LookPath("gtasks"); err != nil {
		return nil, fmt.Errorf("找不到 gtasks 命令（应该在 ~/.local/bin/gtasks）：%w", err)
	}
	cmd := exec.Command("gtasks", "ls", list, "--json")
	var errBuf strings.Builder
	cmd.Stderr = &errBuf
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(errBuf.String())
		if msg == "" {
			msg = strings.TrimSpace(string(out))
		}
		return nil, fmt.Errorf("gtasks 跑挂了：%v（%s）", err, msg)
	}
	var tasks []gtask
	if err := json.Unmarshal(out, &tasks); err != nil {
		return nil, fmt.Errorf("gtasks 的输出不是 JSON：%w", err)
	}
	return tasks, nil
}

// isCJK 从这个码点起就算中文/日文那一侧了（CJK 部首 0x2E80 往上，含假名和全角标点）。
func isCJK(r rune) bool { return r >= 0x2E80 }

// splitTitle 把「Permissive宽容的」「look up 查阅」「run - 跑」拆成 英文 / 中文。
// 界线是第一个 CJK 字符；中间的连接符（空格 - : ： = 逗号 竖线）两边都剃掉。
func splitTitle(title string) (en, zh string) {
	title = strings.TrimSpace(title)
	cut := -1
	for i, r := range title {
		if isCJK(r) {
			cut = i
			break
		}
	}
	if cut < 0 {
		return trimSep(title), ""
	}
	return trimSep(title[:cut]), trimSep(title[cut:])
}

func trimSep(s string) string {
	return strings.Trim(strings.TrimSpace(s), " \t-—:：=/|,，、.。·")
}

// utf16Len 是前端 JS 眼里的字符串长度（UTF-16 code unit 数）。
// Go 的下标是字节，两边混用必出乱码——marks 的 start/end 一律用这个算。
func utf16Len(s string) int { return len(utf16.Encode([]rune(s))) }

// locateWord 在句子里找这个词，返回前端口径的 [start, end)。找不到返回 0,0，
// 前端 highlight() 会跳过不合法的区间，词条本身照样显示。
func locateWord(sentence, word string) (int, int) {
	if sentence == "" || word == "" {
		return 0, 0
	}
	i := strings.Index(strings.ToLower(sentence), strings.ToLower(word))
	if i < 0 {
		return 0, 0
	}
	start := utf16Len(sentence[:i])
	return start, start + utf16Len(sentence[i:i+len(word)])
}

// taskToEntry 把一条任务变成单词本里的一条。返回 ok=false 表示这条不该收。
func taskToEntry(t gtask) (Entry, bool, string) {
	en, zh := splitTitle(t.Title)
	if en == "" {
		return Entry{}, false, fmt.Sprintf("「%s」没有英文部分", strings.TrimSpace(t.Title))
	}

	sentence := strings.TrimSpace(t.Notes)
	if sentence == "" {
		sentence = en // 没写例句就拿词顶上，别让空句子污染存储和 UI
	}
	start, end := locateWord(sentence, en)

	created := time.Now()
	if ts, err := time.Parse(time.RFC3339, t.Updated); err == nil {
		created = ts.Local()
	}

	return Entry{
		ID:        "gt-" + t.ID, // 拿任务 id 当主键，重复导入只会覆盖同一条
		TaskID:    t.ID,
		Sentence:  sentence,
		Marks:     []Mark{{Text: en, Zh: zh, Start: start, End: end}},
		Source:    "单词积累",
		CreatedAt: created.Format(time.RFC3339),
	}, true, ""
}

type importResult struct {
	Added   int      `json:"added"`
	Updated int      `json:"updated"`
	Same    int      `json:"same"`
	Skipped []string `json:"skipped"`
}

func (r importResult) String() string {
	s := fmt.Sprintf("新增 %d，更新 %d，没变 %d", r.Added, r.Updated, r.Same)
	if len(r.Skipped) > 0 {
		s += fmt.Sprintf("，跳过 %d", len(r.Skipped))
	}
	return s
}

// importTasks 拉一次列表，全量对齐到本地单词本。翻译只补空着的中文。
func importTasks(list string) (importResult, error) {
	var res importResult

	tasks, err := fetchTasks(list)
	if err != nil {
		return res, err
	}

	batch := make([]Entry, 0, len(tasks))
	for _, t := range tasks {
		e, ok, why := taskToEntry(t)
		if !ok {
			res.Skipped = append(res.Skipped, why)
			continue
		}
		// 标题只写了英文的，配了 Key 就顺手翻一个中文出来（有缓存，不会重复烧配额）
		if e.Marks[0].Zh == "" && translateKey != "" {
			if tr, err := translateText(e.Marks[0].Text); err == nil {
				e.Marks[0].Zh = tr.Text
			}
		}
		batch = append(batch, e)
	}

	res.Added, res.Updated, res.Same, err = importEntries(batch)
	return res, err
}
