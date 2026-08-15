package main

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Mark 是句子里被圈中的一个词或词组。
//
// Start/End 是前端 JS 的字符串下标（UTF-16 code unit），Go 这边只负责原样存取、
// 绝不拿它去切 Sentence —— Go 的下标是字节，两边语义不一样，混用了迟早切出乱码。
// 需要词本身的时候用 Text，那是前端切好一起传过来的。
type Mark struct {
	Text  string `json:"text"`  // 句子里原样的样子，比如 "running"
	Lemma string `json:"lemma"` // 收进单词本的词形，比如 "run"；留空就用 Text
	Zh    string `json:"zh"`    // 中文释义，可以自动填也可以手改
	Start int    `json:"start"`
	End   int    `json:"end"`

	// TaskID 是这个词在 Google Tasks「单词积累」里对应的那条任务。
	// 非空 = 两边已经对上了：既不用再推一遍，导入时也不会再收一遍（防来回打架）。
	TaskID string `json:"task_id,omitempty"`
}

// Word 返回这条 mark 该以什么形式进单词本
func (m Mark) Word() string {
	if s := strings.TrimSpace(m.Lemma); s != "" {
		return s
	}
	return strings.TrimSpace(m.Text)
}

type Entry struct {
	ID        string `json:"id"`
	Sentence  string `json:"sentence"`
	Trans     string `json:"trans"`  // 整句的意思，可留空
	Note      string `json:"note"`   // 自己的备注
	Source    string `json:"source"` // 这句从哪来的（书名/网址随便写）
	Marks     []Mark `json:"marks"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`

	// TaskID 只有从 Google Tasks 导进来的才有，用来认出「这条已经导过了」。
	TaskID string `json:"task_id,omitempty"`
}

var (
	mu      sync.Mutex
	entries []Entry // 内存里就是全量，量级几千条，够用了
)

func entriesPath() string { return filepath.Join(dataDir, "entries.jsonl") }

func countEntries() int {
	mu.Lock()
	defer mu.Unlock()
	return len(entries)
}

// loadStore 读 JSONL。坏行跳过并警告，不让一行烂数据挡住整个单词本。
func loadStore() error {
	f, err := os.Open(entriesPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // 长句子 + 备注，默认 64K 可能不够
	bad := 0
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			bad++
			continue
		}
		entries = append(entries, e)
	}
	if bad > 0 {
		fmt.Printf("⚠️  有 %d 行读不懂，已跳过（文件没动，可以自己去 %s 看）\n", bad, entriesPath())
	}
	return sc.Err()
}

// flush 整份重写 + rename，避免写一半断电留个半条 JSON。
// 调用方必须已经拿着 mu。
func flush() error {
	tmp := entriesPath() + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	for _, e := range entries {
		if err := enc.Encode(e); err != nil {
			f.Close()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, entriesPath())
}

// saveEntry 没 ID 就是新增，有 ID 就是覆盖那一条。
func saveEntry(e Entry) (Entry, error) {
	e.Sentence = strings.TrimSpace(e.Sentence)
	if e.Sentence == "" {
		return Entry{}, fmt.Errorf("句子是空的")
	}
	if len(e.Marks) == 0 {
		return Entry{}, fmt.Errorf("一个词都没圈——点句子里的词把它标上")
	}
	now := time.Now().Format(time.RFC3339)
	e.UpdatedAt = now

	mu.Lock()
	defer mu.Unlock()

	if e.ID == "" {
		e.ID = fmt.Sprintf("%d", time.Now().UnixNano())
		e.CreatedAt = now
		entries = append(entries, e)
	} else {
		found := false
		for i, old := range entries {
			if old.ID == e.ID {
				e.CreatedAt = old.CreatedAt
				e = keepTaskIDs(old, e)
				entries[i] = e
				found = true
				break
			}
		}
		if !found {
			e.CreatedAt = now
			entries = append(entries, e)
		}
	}
	if err := flush(); err != nil {
		return Entry{}, fmt.Errorf("存盘失败：%w", err)
	}
	return e, nil
}

// importEntries 批量落盘（从 Google Tasks 导过来的那批）：ID 已经在了就更新，
// 否则新增。一次锁、一次 flush，几百条也只写一遍盘。
//
// ⚠️ 已存在的条目一律**保留原来的 created_at**：那是「第一次收这个词」的日子，
// 复习节奏全靠它算。他回头去手机上改个错别字，Tasks 的 updated 会变，
// 但复习进度不能跟着被重置。
func importEntries(in []Entry) (added, updated, same int, err error) {
	if len(in) == 0 {
		return 0, 0, 0, nil
	}

	mu.Lock()
	defer mu.Unlock()

	idx := make(map[string]int, len(entries))
	for i, e := range entries {
		idx[e.ID] = i
	}

	now := time.Now().Format(time.RFC3339)
	dirty := false

	for _, e := range in {
		i, exists := idx[e.ID]
		if !exists {
			e.UpdatedAt = now
			if e.CreatedAt == "" {
				e.CreatedAt = now
			}
			idx[e.ID] = len(entries)
			entries = append(entries, e)
			added++
			dirty = true
			continue
		}

		old := entries[i]
		e.CreatedAt = old.CreatedAt
		e.Trans = old.Trans // 整句翻译和备注是他在网页里补的，Tasks 那边没有，别覆盖掉
		e.Note = old.Note
		if sameContent(old, e) {
			same++
			continue
		}
		e.UpdatedAt = now
		entries[i] = e
		updated++
		dirty = true
	}

	if dirty {
		if err := flush(); err != nil {
			return added, updated, same, fmt.Errorf("存盘失败：%w", err)
		}
	}
	return added, updated, same, nil
}

// sameContent 比句子和词，不比时间戳——否则每次导入都判定成「变了」，白写一遍盘。
func sameContent(a, b Entry) bool {
	if a.Sentence != b.Sentence || a.Source != b.Source || len(a.Marks) != len(b.Marks) {
		return false
	}
	for i := range a.Marks {
		if a.Marks[i] != b.Marks[i] {
			return false
		}
	}
	return true
}

// keepTaskIDs 把旧版本里「这个词已经同步到哪条任务」的对应关系接过来。
// 前端改完一条重新提交时 marks 里是不带 task_id 的，不接一下两边就断了，
// 下次导入会把同一个词再收一遍。
func keepTaskIDs(old, next Entry) Entry {
	if next.TaskID == "" {
		next.TaskID = old.TaskID
	}
	byWord := make(map[string]string, len(old.Marks))
	for _, m := range old.Marks {
		if m.TaskID != "" {
			byWord[strings.ToLower(m.Word())] = m.TaskID
		}
	}
	for i, m := range next.Marks {
		if m.TaskID == "" {
			next.Marks[i].TaskID = byWord[strings.ToLower(m.Word())]
		}
	}
	return next
}

// snapshotEntries 拷一份出来给要打网络的活用——别攥着锁去等 Google。
func snapshotEntries() []Entry {
	mu.Lock()
	defer mu.Unlock()
	out := make([]Entry, len(entries))
	copy(out, entries)
	return out
}

// syncedWords 是已经在 Google Tasks 里有对应任务的词（小写）。推之前拿它挡重复。
func syncedWords() map[string]bool {
	mu.Lock()
	defer mu.Unlock()
	out := map[string]bool{}
	for _, e := range entries {
		for _, m := range e.Marks {
			if m.TaskID != "" {
				out[strings.ToLower(m.Word())] = true
			}
		}
	}
	return out
}

// claimedTasks 是「任务 id -> 本地哪条记着它」。导入时拿它认出自己推上去的那些任务，
// 别再当成新词收一遍（推出去、又导回来 = 同一个词两条，来回打架）。
func claimedTasks() map[string]string {
	mu.Lock()
	defer mu.Unlock()
	out := map[string]string{}
	for _, e := range entries {
		if e.TaskID != "" {
			out[e.TaskID] = e.ID
		}
		for _, m := range e.Marks {
			if m.TaskID != "" {
				out[m.TaskID] = e.ID
			}
		}
	}
	return out
}

// setMarkTaskIDs 把刚推上去拿到的 task id 记回对应的词，一次锁一次 flush。
func setMarkTaskIDs(entryID string, ids map[string]string) error {
	if len(ids) == 0 {
		return nil
	}
	mu.Lock()
	defer mu.Unlock()

	for i, e := range entries {
		if e.ID != entryID {
			continue
		}
		for j, m := range e.Marks {
			if id, ok := ids[strings.ToLower(m.Word())]; ok && m.TaskID == "" {
				entries[i].Marks[j].TaskID = id
			}
		}
		return flush()
	}
	return fmt.Errorf("没这条：%s", entryID)
}

func deleteEntry(id string) error {
	if id == "" {
		return fmt.Errorf("没给 id")
	}
	mu.Lock()
	defer mu.Unlock()

	for i, e := range entries {
		if e.ID == id {
			entries = append(entries[:i], entries[i+1:]...)
			return flush()
		}
	}
	return fmt.Errorf("没这条：%s", id)
}

// listEntries 按新到旧返回；q 非空就在 句子/词/释义/备注/出处 里做大小写无关的包含匹配。
func listEntries(q string) []Entry {
	mu.Lock()
	defer mu.Unlock()

	q = strings.ToLower(strings.TrimSpace(q))
	out := make([]Entry, 0, len(entries))
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if q == "" || matches(e, q) {
			out = append(out, e)
		}
	}
	return out
}

func matches(e Entry, q string) bool {
	hay := []string{e.Sentence, e.Trans, e.Note, e.Source}
	for _, m := range e.Marks {
		hay = append(hay, m.Text, m.Lemma, m.Zh)
	}
	for _, s := range hay {
		if strings.Contains(strings.ToLower(s), q) {
			return true
		}
	}
	return false
}

// exportCSV 导出单词本。默认两列 英文,中文（跟 vocab.csv 通用），
// full=true 时多一列例句。同一个词只留最新那条释义。
func exportCSV(w io.Writer, full bool) error {
	mu.Lock()
	defer mu.Unlock()

	type row struct{ word, zh, sentence string }
	seen := map[string]int{}
	var rows []row

	for _, e := range entries { // 老 → 新，后面的覆盖前面的释义
		for _, m := range e.Marks {
			word := m.Word()
			if word == "" {
				continue
			}
			key := strings.ToLower(word)
			r := row{word: word, zh: strings.TrimSpace(m.Zh), sentence: e.Sentence}
			if i, ok := seen[key]; ok {
				rows[i] = r
				continue
			}
			seen[key] = len(rows)
			rows = append(rows, r)
		}
	}

	cw := csv.NewWriter(w)
	header := []string{"英文", "中文"}
	if full {
		header = append(header, "例句")
	}
	if err := cw.Write(header); err != nil {
		return err
	}
	for _, r := range rows {
		rec := []string{r.word, r.zh}
		if full {
			rec = append(rec, r.sentence)
		}
		if err := cw.Write(rec); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}
