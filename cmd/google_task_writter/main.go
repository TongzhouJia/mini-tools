package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/googleapi" // 新增：用于捕获和识别 403 错误
	"google.golang.org/api/option"
	"google.golang.org/api/tasks/v1"
)

// 获取 OAuth2 客户端
func getClient(config *oauth2.Config) *http.Client {
	tokFile := "token.json"
	tok, err := tokenFromFile(tokFile)
	if err != nil {
		tok = getTokenFromWeb(config)
		saveToken(tokFile, tok)
	}
	return config.Client(context.Background(), tok)
}

// 从 Web 获取 Token
func getTokenFromWeb(config *oauth2.Config) *oauth2.Token {
	authURL := config.AuthCodeURL("state-token", oauth2.AccessTypeOffline)
	fmt.Printf("请在浏览器中打开以下链接授权，然后将获取到的授权码输入到这里: \n%v\n", authURL)
	fmt.Print("输入授权码: ")

	var authCode string
	if _, err := fmt.Scan(&authCode); err != nil {
		log.Fatalf("无法读取授权码: %v", err)
	}

	tok, err := config.Exchange(context.TODO(), authCode)
	if err != nil {
		log.Fatalf("无法获取 Token: %v", err)
	}
	return tok
}

func tokenFromFile(file string) (*oauth2.Token, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := f.Close(); err != nil {
			log.Printf("关闭文件失败 %s: %v", file, err)
		}
	}()
	tok := &oauth2.Token{}
	err = json.NewDecoder(f).Decode(tok)
	return tok, err
}

func saveToken(path string, token *oauth2.Token) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		log.Fatalf("无法缓存 OAuth token: %v", err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			log.Printf("关闭 token 文件失败: %v", err)
		}
	}()
	if err := json.NewEncoder(f).Encode(token); err != nil {
		log.Fatalf("编码 token 到文件失败: %v", err)
	}
}

// 解析行：第一个空格前为单词，后面为释义
func parseLine(line string) (word string, meaning string) {
	line = strings.TrimSpace(line)
	// 遇到空行或者以逗号开头的行直接跳过
	if line == "" || strings.HasPrefix(line, ",") {
		return "", ""
	}
	parts := strings.SplitN(line, " ", 2)
	if len(parts) == 2 {
		return parts[0], strings.TrimSpace(parts[1])
	}
	return parts[0], ""
}

// 新增：专门处理 403 频率限制的重试函数
func insertTaskWithRetry(srv *tasks.Service, listId string, task *tasks.Task) error {
	maxRetries := 5
	waitDuration := 2 * time.Second

	for i := 0; i < maxRetries; i++ {
		_, err := srv.Tasks.Insert(listId, task).Do()
		if err == nil {
			return nil // 成功
		}

		// 检查是否是频率超限
		var gErr *googleapi.Error
		if errors.As(err, &gErr) && (gErr.Code == 403 || gErr.Code == 429) {
			fmt.Printf("  ⚠️ 触发限制，等待 %v 后进行第 %d 次重试...\n", waitDuration, i+1)
			time.Sleep(waitDuration)
			waitDuration *= 2 // 退避时间翻倍
			continue
		}
		return err // 非频率限制的错误直接返回
	}
	return fmt.Errorf("在重试 %d 次后依然失败", maxRetries)
}

func main() {
	ctx := context.Background()

	// 1. 加载 Google API 凭据（默认取仓库里的 data/google_task_writer/credentials.json）
	credPath := strings.TrimSpace(os.Getenv("GOOGLE_TASKS_CREDENTIALS"))
	if credPath == "" {
		credPath = filepath.Join("data", "google_task_writer", "credentials.json")
	}
	b, err := os.ReadFile(credPath)
	if err != nil {
		log.Fatalf("无法读取 %s: %v（可用 GOOGLE_TASKS_CREDENTIALS 指定路径）", credPath, err)
	}

	config, err := google.ConfigFromJSON(b, tasks.TasksScope)
	if err != nil {
		log.Fatalf("无法解析 credentials.json: %v", err)
	}
	client := getClient(config)

	srv, err := tasks.NewService(ctx, option.WithHTTPClient(client))
	if err != nil {
		log.Fatalf("无法创建 Tasks Service: %v", err)
	}

	// 2. 本地文件路径
	// 原来写死在 macOS 的 Dropbox 里（~/Public/Drop Box/学外语）；那棵目录树 2026-07-30
	// 从 Mac 拷进了仓库的 data/学外语（data/ 在 .gitignore 里，不进版本控制）。
	// 需要时可用 LINGO_BASE_DIR 指向别处。
	lingoBase := strings.TrimSpace(os.Getenv("LINGO_BASE_DIR"))
	if lingoBase == "" {
		lingoBase = filepath.Join("data", "学外语")
	}
	baseDir := filepath.Join(lingoBase, "alphabet_order_word")

	files, err := os.ReadDir(baseDir)
	if err != nil {
		log.Fatalf("无法读取目录 %s: %v", baseDir, err)
	}

	// 3. 遍历每个字母文件
	for _, fileInfo := range files {
		if fileInfo.IsDir() || filepath.Ext(fileInfo.Name()) != ".txt" {
			continue
		}

		// 获取列表名称：比如 a.txt -> A
		letter := strings.ToUpper(strings.TrimSuffix(fileInfo.Name(), ".txt"))
		fmt.Printf("\n--- 正在为字母 %s 创建新列表 ---\n", letter)

		// 创建对应的 Task List
		taskList, err := srv.Tasklists.Insert(&tasks.TaskList{
			Title: letter,
		}).Do()
		if err != nil {
			log.Printf("❌ 创建列表 %s 失败: %v\n", letter, err)
			continue
		}
		fmt.Printf("列表 '%s' 创建完毕 (ID: %s)\n", letter, taskList.Id)

		// 读取文件内容
		filePath := filepath.Join(baseDir, fileInfo.Name())
		file, err := os.Open(filePath)
		if err != nil {
			log.Printf("❌ 无法打开文件 %s: %v\n", filePath, err)
			continue
		}

		scanner := bufio.NewScanner(file)

		for scanner.Scan() {
			line := scanner.Text()
			word, meaning := parseLine(line)

			if word == "" {
				continue
			}

			// 插入提醒事项
			task := &tasks.Task{
				Title: word,
				Notes: meaning,
			}

			// 修改：替换为带重试机制的函数
			err := insertTaskWithRetry(srv, taskList.Id, task)
			if err != nil {
				log.Printf("  ❌ [放弃] 单词 '%s': %v\n", word, err)
			} else {
				fmt.Printf("  ✔ 已添加: %s\n", word)
			}

			// 修改：把“每 5 个单词休息 1 秒”改为“每个单词固定休息 500 毫秒”
			time.Sleep(500 * time.Millisecond)
		}
		if err := file.Close(); err != nil {
			log.Printf("关闭文件 %s 失败: %v", filePath, err)
		}
	}

	fmt.Println("\n🎉 全部完成！你的 Google Tasks 现在已按字母分类排列。")
}
