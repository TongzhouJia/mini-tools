// vocab_task —— 粘一段单词表，直接写进 Google Tasks。
//
// 干什么用：手机上的单词列表是他的输入口，但一条条手打太慢。
// 这个页面接受两种粘贴格式，自动认，转成任务：
//
//	日语：日语,假名,中文[,音频]   →  标题「日语,中文」，细节里放假名
//	英语：English,Chinese         →  标题「English」，细节里放中文释义
//
// 表头行（日文/假名/中文/音频、English/Chinese、单词/中文释义）自动跳过，
// 带不带引号都行，行尾多余的空列（那个「音频」占位）会被扔掉。
//
// 写任务是外调 ~/.local/bin/gtasks，token 刷新和重试都归它管。
package main

import (
	"bytes"
	"embed"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

//go:embed index.html
var assets embed.FS

const defaultList = "外语得学啊"

const usage = `vocab_task —— 粘单词表，一键写进 Google Tasks

用法：
  vocab_task              起在 :8089，浏览器打开 http://localhost:8089
  vocab_task -lan         同一个 Wi-Fi 下手机也能开
  vocab_task -list "To do"  换默认写进哪个列表（默认「` + defaultList + `」）

网页里怎么用：
  1. 把单词表整段粘进框里（两种格式随便混，逐行认）
  2. 下面自动出预览：标题 / 细节，已经在列表里的会标「重复」并自动取消勾选
  3. 点「写进去」，一条条推，实时出进度

认得的格式：
  日语,假名,中文[,音频]   →  标题「日语,中文」，细节放假名
  English,Chinese         →  标题「English」，细节放中文释义
  只有一列                →  整行当标题
  表头行自动跳过；分隔符是英文逗号，整行没逗号时也认 Tab

别的：
  写进去的顺序是倒着推的，这样任务列表从上往下读跟粘贴顺序一致
  实际写任务靠 gtasks（-gtasks 改路径），凭据见 ~/.config/gtasks/token.json

参数：
`

var gtasksBin string

func main() {
	port := flag.String("port", "8089", "监听端口")
	lan := flag.Bool("lan", false, "监听 0.0.0.0，手机也能开（默认只有本机能开）")
	list := flag.String("list", defaultList, "默认写进哪个任务列表")
	bin := flag.String("gtasks", "", "gtasks 命令的路径（默认自动找）")
	flag.CommandLine.SetOutput(os.Stdout)
	flag.Usage = func() { fmt.Print(usage); flag.PrintDefaults() }
	flag.Parse()

	gtasksBin = findGtasks(*bin)
	if gtasksBin == "" {
		log.Fatal("找不到 gtasks 命令，用 -gtasks 指一下路径")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/api/lists", logged("/api/lists", handleLists))
	mux.HandleFunc("/api/parse", logged("/api/parse", handleParse(*list)))
	mux.HandleFunc("/api/push", logged("/api/push", handlePush(*list)))

	host := "127.0.0.1"
	if *lan {
		host = "0.0.0.0"
	}
	addr := host + ":" + *port
	fmt.Printf("vocab_task 起来了：http://127.0.0.1:%s   默认列表「%s」\n", *port, *list)
	if *lan {
		fmt.Printf("手机上开：http://%s:%s\n", localIP(), *port)
	}
	log.Fatal(http.ListenAndServe(addr, mux))
}

func findGtasks(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if p, err := exec.LookPath("gtasks"); err == nil {
		return p
	}
	p := filepath.Join(os.Getenv("HOME"), ".local", "bin", "gtasks")
	if st, err := os.Stat(p); err == nil && !st.IsDir() {
		return p
	}
	return ""
}

func localIP() string {
	out, err := exec.Command("hostname", "-I").Output()
	if err != nil {
		return "127.0.0.1"
	}
	if f := strings.Fields(string(out)); len(f) > 0 {
		return f[0]
	}
	return "127.0.0.1"
}

// ---------- 解析 ----------

// Row 是预览里的一行。
type Row struct {
	Line  int    `json:"line"`
	Title string `json:"title"`
	Notes string `json:"notes"`
	Kind  string `json:"kind"` // "日语" | "英语" | "单列"
	Dup   bool   `json:"dup"`
	Raw   string `json:"raw"`
}

var headerWords = map[string]bool{
	"日文": true, "日语": true, "假名": true, "中文": true, "音频": true,
	"english": true, "chinese": true, "单词": true, "中文释义": true,
	"英文": true, "英语": true, "释义": true, "读音": true, "留空": true,
	"word": true, "meaning": true, "kana": true, "audio": true,
}

func splitFields(line string) []string {
	if !strings.Contains(line, ",") && strings.Contains(line, "\t") {
		line = strings.ReplaceAll(line, "\t", ",")
	}
	r := csv.NewReader(strings.NewReader(line))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	rec, err := r.Read()
	if err != nil {
		// 引号没配对之类，退化成傻切
		rec = strings.Split(line, ",")
	}
	for i := range rec {
		rec[i] = strings.TrimSpace(strings.Trim(strings.TrimSpace(rec[i]), `"`))
	}
	// 扔掉行尾的空列（「音频」那个占位）
	for len(rec) > 0 && rec[len(rec)-1] == "" {
		rec = rec[:len(rec)-1]
	}
	return rec
}

func isHeader(rec []string) bool {
	if len(rec) == 0 {
		return true
	}
	for _, f := range rec {
		if f == "" {
			continue
		}
		if !headerWords[strings.ToLower(f)] {
			return false
		}
	}
	return true
}

func parseText(text string) []Row {
	text = strings.TrimPrefix(text, "\ufeff")
	var rows []Row
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(strings.TrimSpace(line), ",")
		if line == "" {
			continue
		}
		rec := splitFields(line)
		if isHeader(rec) {
			continue
		}
		row := Row{Line: i + 1, Raw: line}
		switch {
		case len(rec) >= 3:
			row.Kind = "日语"
			row.Title = rec[0] + "," + rec[2]
			row.Notes = rec[1]
		case len(rec) == 2:
			row.Kind = "英语"
			row.Title = rec[0]
			row.Notes = rec[1]
		default:
			row.Kind = "单列"
			row.Title = rec[0]
		}
		if row.Title == "" {
			continue
		}
		rows = append(rows, row)
	}
	return rows
}

// ---------- gtasks ----------

type taskList struct {
	Name string `json:"name"`
	ID   string `json:"id"`
}

func listNames() ([]taskList, error) {
	out, err := exec.Command(gtasksBin, "lists").Output()
	if err != nil {
		return nil, fmt.Errorf("gtasks lists 挂了：%w", err)
	}
	var res []taskList
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimSpace(strings.TrimPrefix(line, "📋"))
		if line == "" {
			continue
		}
		i := strings.LastIndex(line, "(")
		if i < 0 || !strings.HasSuffix(line, ")") {
			continue
		}
		res = append(res, taskList{
			Name: strings.TrimSpace(line[:i]),
			ID:   line[i+1 : len(line)-1],
		})
	}
	return res, nil
}

func existingTitles(list string) (map[string]bool, error) {
	out, err := exec.Command(gtasksBin, "ls", list, "--json").Output()
	if err != nil {
		return nil, fmt.Errorf("读不了列表「%s」：%w", list, err)
	}
	var tasks []struct {
		Title string `json:"title"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &tasks); err != nil {
		return nil, fmt.Errorf("列表返回的不是 JSON：%w", err)
	}
	seen := map[string]bool{}
	for _, t := range tasks {
		seen[strings.ToLower(strings.TrimSpace(t.Title))] = true
	}
	return seen, nil
}

func addTask(list, title, notes string) error {
	args := []string{"add", title, "--list", list}
	if notes != "" {
		args = append(args, "--notes", notes)
	}
	cmd := exec.Command(gtasksBin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
}

// ---------- HTTP ----------

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	b, err := assets.ReadFile("index.html")
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(b)
}

func handleLists(w http.ResponseWriter, r *http.Request) {
	ls, err := listNames()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]any{"lists": ls})
}

func handleParse(defList string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text string `json:"text"`
			List string `json:"list"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "请求体不是 JSON："+err.Error(), 400)
			return
		}
		list := body.List
		if list == "" {
			list = defList
		}
		rows := parseText(body.Text)
		warn := ""
		if seen, err := existingTitles(list); err != nil {
			warn = "查不了列表里已有的词（去重先不做了）：" + err.Error()
		} else {
			inBatch := map[string]bool{}
			for i := range rows {
				k := strings.ToLower(strings.TrimSpace(rows[i].Title))
				if seen[k] || inBatch[k] {
					rows[i].Dup = true
				}
				inBatch[k] = true
			}
		}
		if rows == nil {
			rows = []Row{}
		}
		writeJSON(w, map[string]any{"rows": rows, "list": list, "warn": warn})
	}
}

func handlePush(defList string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			List  string `json:"list"`
			Items []Row  `json:"items"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "请求体不是 JSON："+err.Error(), 400)
			return
		}
		list := body.List
		if list == "" {
			list = defList
		}
		w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
		w.Header().Set("X-Accel-Buffering", "no")
		flusher, _ := w.(http.Flusher)
		enc := json.NewEncoder(w)
		send := func(v any) {
			enc.Encode(v)
			if flusher != nil {
				flusher.Flush()
			}
		}

		n := len(body.Items)
		ok, fail := 0, 0
		// 倒着推：Tasks 新任务插在最上面，倒着写完列表顺序才跟粘贴顺序一致
		for i := n - 1; i >= 0; i-- {
			it := body.Items[i]
			err := addTask(list, it.Title, it.Notes)
			if err != nil {
				fail++
				log.Printf("写失败 %q：%v", it.Title, err)
				send(map[string]any{"i": i, "title": it.Title, "ok": false, "err": err.Error()})
			} else {
				ok++
				send(map[string]any{"i": i, "title": it.Title, "ok": true})
			}
		}
		send(map[string]any{"done": true, "ok": ok, "fail": fail, "list": list})
		log.Printf("写完：成功 %d，失败 %d，列表「%s」", ok, fail, list)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

// logged 打一行状态码，浏览器不说的终端说。
func logged(name string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		lw := &logWriter{ResponseWriter: w, code: 200}
		h(lw, r)
		log.Printf("%s %s -> %d", r.Method, name, lw.code)
	}
}

type logWriter struct {
	http.ResponseWriter
	code int
}

func (l *logWriter) WriteHeader(c int) { l.code = c; l.ResponseWriter.WriteHeader(c) }

// Flush 必须透传，否则流式进度直接废掉。
func (l *logWriter) Flush() {
	if f, ok := l.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
