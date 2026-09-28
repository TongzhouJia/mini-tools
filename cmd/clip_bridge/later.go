// 「明天发邮件」：点完 3–5 小时后随机发一封，第二天早上 5–6 点再发一封。
// 电脑关着 / 睡着错过了点，开机后补发；第二封永远排在第一封实际发出之后的那个早上。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	mrand "math/rand/v2"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Job 是一次「明天发邮件」。点的时候把内容拍个快照（文件是硬链接），
// 之后在中转站里删掉这条也不影响邮件。
type Job struct {
	ID      string   `json:"id"`
	ItemID  string   `json:"itemId"`
	Type    string   `json:"type"` // 跟 Item 一样："text" | "file"
	Text    string   `json:"text,omitempty"`
	Name    string   `json:"name,omitempty"`
	Size    int64    `json:"size,omitempty"`
	Clicked int64    `json:"clicked"` // 点按钮的时间，Unix 秒
	At      [2]int64 `json:"at"`      // 两封的预定时间，Unix 秒
	Sent    int      `json:"sent"`    // 已经发出去几封
	Next    int64    `json:"next"`    // 下次尝试的时间：平时就是 At[Sent]，失败了往后推
	Fails   int      `json:"fails,omitempty"`
	Err     string   `json:"err,omitempty"`
}

const (
	nightEnd  = 6                // 第一封落在 0–6 点（多半睡着），第二封挪到再下一个早上
	lateAfter = 30 * time.Minute // 第一封晚发超过这么久（关机、睡眠、断网），第二封按实际发出时间重排
	warnFails = 3                // 连着失败这么多次，页面上才冒红字
)

var (
	laterMu sync.Mutex
	jobs    []Job
)

func jobsPath() string     { return filepath.Join(dataDir, "later.json") }
func jobDir(j Job) string  { return filepath.Join(dataDir, "later", j.ID) }
func jobFile(j Job) string { return filepath.Join(jobDir(j), safeName(j.Name)) }

func loadJobs() error {
	b, err := os.ReadFile(jobsPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &jobs); err != nil {
		// 坏了就挪开重来，别让整个服务起不来；原文件留着能手工救
		os.Rename(jobsPath(), jobsPath()+".bad")
		log.Printf("later.json 读不了，已挪成 later.json.bad：%v", err)
		jobs = nil
	}
	return nil
}

// 调用前必须持有 laterMu。
func saveJobs() error {
	b, err := json.MarshalIndent(jobs, "", "  ")
	if err != nil {
		return err
	}
	tmp := jobsPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, jobsPath())
}

// morningAfter 返回 t 之后第一个 5:minute。t 本身在半夜（0–6 点）就再往后推一天：
// 那封是醒来才看到的，第二封要是同一个早上到，两封一起躺在收件箱里等于白发。
func morningAfter(t time.Time, minute int) time.Time {
	c := time.Date(t.Year(), t.Month(), t.Day(), 5, minute, 0, 0, t.Location())
	if t.Hour() < nightEnd {
		c = c.AddDate(0, 0, 1)
	}
	for !c.After(t) {
		c = c.AddDate(0, 0, 1)
	}
	return c
}

func when(u int64) string { return time.Unix(u, 0).Format("1/2 15:04") }

func describe(j Job) string {
	if j.Sent == 0 {
		return fmt.Sprintf("%s 和 %s 各发一封", when(j.At[0]), when(j.At[1]))
	}
	return fmt.Sprintf("第一封已发，%s 再发一封", when(j.At[1]))
}

func handleLater(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, 405, "只收 POST")
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		fail(w, 400, "解析不了请求："+err.Error())
		return
	}
	it, ok := findItem(req.ID)
	if !ok {
		fail(w, 404, "没这条")
		return
	}

	laterMu.Lock()
	defer laterMu.Unlock()
	for _, j := range jobs {
		if j.ItemID == it.ID { // 手滑点两下别排两套
			writeJSON(w, map[string]string{"ok": "已经排过了：" + describe(j)})
			return
		}
	}
	if it.Type == "file" && it.Size > gmailAttachLimit {
		fail(w, 400, fmt.Sprintf("文件 %s 有 %s，超过 Gmail 的 25MB 附件上限，发不了", it.Name, humanSize(it.Size)))
		return
	}

	now := time.Now()
	first := now.Add(3*time.Hour + time.Duration(mrand.Int64N(int64(2*time.Hour))))
	second := morningAfter(first, mrand.IntN(60))
	j := Job{
		ID: newID(), ItemID: it.ID, Type: it.Type, Text: it.Text, Name: it.Name, Size: it.Size,
		Clicked: now.Unix(), At: [2]int64{first.Unix(), second.Unix()}, Next: first.Unix(),
	}
	if it.Type == "file" {
		if err := snapshot(filePath(it), jobFile(j)); err != nil {
			os.RemoveAll(jobDir(j))
			fail(w, 500, "存不下快照："+err.Error())
			return
		}
	}
	jobs = append(jobs, j)
	if err := saveJobs(); err != nil {
		jobs = jobs[:len(jobs)-1]
		os.RemoveAll(jobDir(j))
		fail(w, 500, "存不下："+err.Error())
		return
	}
	log.Printf("明天发邮件：%s 排在 %s", label(j), describe(j))
	writeJSON(w, map[string]string{"ok": "排好了：" + describe(j)})
}

// snapshot 优先硬链接（不占空间），跨分区链不了再老实拷一份。
func snapshot(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if os.Link(src, dst) == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func label(j Job) string {
	if j.Type == "file" {
		return j.Name
	}
	return firstLine(j.Text, 30)
}

// laterLoop 每分钟看一眼有没有到点的。启动时先跑一次，关机错过的就是这里补上的。
func laterLoop(mailTo string) {
	for {
		laterTick(mailTo, time.Now())
		time.Sleep(time.Minute)
	}
}

func laterTick(mailTo string, now time.Time) {
	laterMu.Lock()
	var due []Job
	for _, j := range jobs {
		if j.Next <= now.Unix() {
			due = append(due, j)
		}
	}
	laterMu.Unlock()

	for _, j := range due {
		if j.Type == "file" {
			if _, err := os.Stat(jobFile(j)); err != nil {
				log.Printf("明天发邮件：%s 的快照没了，这条作废：%v", label(j), err)
				dropJob(j.ID)
				continue
			}
		}
		err := sendLater(j, mailTo)
		laterMu.Lock()
		for i := range jobs {
			if jobs[i].ID == j.ID && afterSend(&jobs[i], err, time.Now()) {
				os.RemoveAll(jobDir(jobs[i]))
				jobs = append(jobs[:i], jobs[i+1:]...)
				break
			}
		}
		if err := saveJobs(); err != nil {
			log.Printf("明天发邮件：存不下 later.json：%v", err)
		}
		laterMu.Unlock()
	}
}

// afterSend 记下一封发没发成，返回 true 表示两封都发完了。
func afterSend(j *Job, err error, now time.Time) bool {
	if err != nil {
		j.Fails++
		j.Err = err.Error()
		j.Next = now.Add(backoff(j.Fails)).Unix()
		log.Printf("明天发邮件：%s 第 %d 封没发出去（连着第 %d 次）：%v", label(*j), j.Sent+1, j.Fails, err)
		return false
	}
	j.Sent++
	j.Fails, j.Err = 0, ""
	log.Printf("明天发邮件：%s 第 %d/2 封已发", label(*j), j.Sent)
	if j.Sent >= 2 {
		return true
	}
	if now.Sub(time.Unix(j.At[0], 0)) > lateAfter {
		j.At[1] = morningAfter(now, time.Unix(j.At[1], 0).Minute()).Unix()
	}
	j.Next = j.At[1]
	return false
}

// backoff：2、4、8、16 分钟，之后每 30 分钟试一次。开机那会儿网络还没起来很正常。
func backoff(fails int) time.Duration {
	if fails > 4 {
		return 30 * time.Minute
	}
	return time.Duration(1<<fails) * time.Minute
}

func dropJob(id string) {
	laterMu.Lock()
	defer laterMu.Unlock()
	for i := range jobs {
		if jobs[i].ID == id {
			os.RemoveAll(jobDir(jobs[i]))
			jobs = append(jobs[:i], jobs[i+1:]...)
			break
		}
	}
	saveJobs()
}

func sendLater(j Job, mailTo string) error {
	bin, err := exec.LookPath("gmail-send")
	if err != nil {
		return fmt.Errorf("找不到 gmail-send")
	}
	n := j.Sent + 1
	note := fmt.Sprintf("中转站「明天发邮件」第 %d 遍（共 2 遍），%s 点的。", n, when(j.Clicked))
	var subject, body string
	var args []string
	if j.Type == "text" {
		subject = fmt.Sprintf("复习 %d/2：%s", n, firstLine(j.Text, 60))
		body = j.Text + "\n\n——\n" + note
	} else {
		subject = fmt.Sprintf("复习 %d/2：%s", n, j.Name)
		body = fmt.Sprintf("%s\n文件名：%s\n大小：%s", note, j.Name, humanSize(j.Size))
		args = append(args, "-a", jobFile(j))
	}
	if mailTo != "" {
		args = append(args, "-t", mailTo)
	}
	// 主题和正文放在 -- 后面：文字以「-」开头时 argparse 会把它当成选项
	args = append(args, "--", subject, body)

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	if err != nil {
		msg := strings.TrimPrefix(strings.TrimSpace(string(out)), "❌ ")
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
}

// laterWarning 给页面用：有连着失败好几次的就冒一行红字，不然定时邮件静悄悄地没了没人知道。
func laterWarning() string {
	laterMu.Lock()
	defer laterMu.Unlock()
	n, last := 0, ""
	for _, j := range jobs {
		if j.Fails >= warnFails {
			n++
			last = j.Err
		}
	}
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("「明天发邮件」有 %d 封一直发不出去，还在重试：%s", n, last)
}
