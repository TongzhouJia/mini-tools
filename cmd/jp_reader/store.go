package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Segment 是一个点读单元 —— 用户自己拖选出来的一段。
//
// Start/End 是**前端 JS 的 UTF-16 下标**，Go 这边只存不碰。
// Go 的字符串下标是字节，日语一个假名 3 字节，混用必出乱码
// （context_vocab 上踩过这个坑，那边靠 utf16.Encode 换算）。
// 这里的做法更省事：切文本的活全在前端干，Text 由前端切好一起发过来，
// 服务端只拿 Text 去合成语音，永远不用 Start/End 去切 Doc.Text。
type Segment struct {
	Start int    `json:"start"`
	End   int    `json:"end"`
	Text  string `json:"text"`
	Note  string `json:"note,omitempty"` // 中文译文，按 t 键才有
}

type Doc struct {
	ID       string    `json:"id"`
	Title    string    `json:"title"`
	Text     string    `json:"text"`
	Segments []Segment `json:"segments"`
	Voice    string    `json:"voice,omitempty"`
	Rate     float64   `json:"rate,omitempty"`
	Created  time.Time `json:"created"`
	Updated  time.Time `json:"updated"`
}

// docBrief 是侧边栏列表用的摘要，不带全文（文章可能很长）
type docBrief struct {
	ID      string    `json:"id"`
	Title   string    `json:"title"`
	Chars   int       `json:"chars"`
	Segs    int       `json:"segs"`
	Updated time.Time `json:"updated"`
}

func docsDir() string { return filepath.Join(dataDir, "docs") }

// newID 生成文件名安全的 id：时间戳 + 4 位随机，同一秒建两篇也不会撞
func newID() string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 4)
	for i := range b {
		b[i] = letters[rand.Intn(len(letters))]
	}
	return time.Now().Format("20060102-150405") + "-" + string(b)
}

// safeID 挡路径穿越 —— id 是从 query 来的，直接拼进路径会被 ../ 带出去
func safeID(id string) error {
	if id == "" {
		return fmt.Errorf("缺 id")
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return fmt.Errorf("id 不合法：%s", id)
		}
	}
	return nil
}

func docPath(id string) string { return filepath.Join(docsDir(), id+".json") }

func loadDoc(id string) (*Doc, error) {
	if err := safeID(id); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(docPath(id))
	if err != nil {
		return nil, fmt.Errorf("读不到这篇文章：%w", err)
	}
	var d Doc
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, fmt.Errorf("这篇文章的存档坏了：%w", err)
	}
	return &d, nil
}

// saveDoc 先写临时文件再 rename。
// 用户的正文可能是辛苦攒的一整篇，写一半崩了就没了，多这一步不亏。
func saveDoc(d *Doc) error {
	if err := safeID(d.ID); err != nil {
		return err
	}
	if err := os.MkdirAll(docsDir(), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	tmp := docPath(d.ID) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, docPath(d.ID))
}

func listDocs() []docBrief {
	out := []docBrief{}
	entries, err := os.ReadDir(docsDir())
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		d, err := loadDoc(strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			continue // 单篇坏了不该让整个列表打不开
		}
		out = append(out, docBrief{
			ID:      d.ID,
			Title:   d.Title,
			Chars:   len([]rune(d.Text)),
			Segs:    len(d.Segments),
			Updated: d.Updated,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out
}

// ── 路由 ──────────────────────────────────────────────────────────────

func handleDocs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, listDocs())
}

func handleDoc(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		d, err := loadDoc(r.URL.Query().Get("id"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		writeJSON(w, d)

	case http.MethodPost:
		var in Doc
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "请求体看不懂："+err.Error(), http.StatusBadRequest)
			return
		}
		now := time.Now()
		if in.ID == "" {
			in.ID = newID()
			in.Created = now
		} else {
			if err := safeID(in.ID); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			// 保住原始创建时间；文件不在就当新建
			if old, err := loadDoc(in.ID); err == nil {
				in.Created = old.Created
			} else {
				in.Created = now
			}
		}
		in.Updated = now
		if strings.TrimSpace(in.Title) == "" {
			in.Title = autoTitle(in.Text)
		}
		if in.Segments == nil {
			in.Segments = []Segment{}
		}
		if err := saveDoc(&in); err != nil {
			http.Error(w, "存不进去："+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, &in)

	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		if err := safeID(id); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := os.Remove(docPath(id)); err != nil {
			http.Error(w, "删不掉："+err.Error(), http.StatusInternalServerError)
			return
		}
		// 音频缓存是按文本 sha1 存的、跟文章无关，故意不跟着删：
		// 同一句话在别的文章里还能直接命中
		writeJSON(w, map[string]bool{"ok": true})

	default:
		http.Error(w, "方法不对", http.StatusMethodNotAllowed)
	}
}

// autoTitle 没填标题时拿正文开头顶上
func autoTitle(text string) string {
	t := strings.TrimSpace(text)
	if i := strings.IndexAny(t, "\n。！？"); i > 0 {
		t = t[:i]
	}
	rs := []rune(strings.TrimSpace(t))
	if len(rs) > 24 {
		rs = rs[:24]
	}
	if len(rs) == 0 {
		return "未命名"
	}
	return string(rs)
}
