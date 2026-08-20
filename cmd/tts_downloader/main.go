package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// TTSRequest 定义请求谷歌 TTS 接口的 JSON 结构
type TTSRequest struct {
	Input struct {
		Text string `json:"text"`
	} `json:"input"`
	Voice struct {
		LanguageCode string `json:"languageCode"`
		Name         string `json:"name"`
	} `json:"voice"`
	AudioConfig struct {
		AudioEncoding string `json:"audioEncoding"`
	} `json:"audioConfig"`
}

// TTSResponse 定义接收谷歌响应的 JSON 结构
type TTSResponse struct {
	AudioContent string `json:"audioContent"`
	Error        *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

const usage = `🔊 tts_downloader —— 用 Google TTS 把文本合成 mp3 存下来

用法：
  tts_downloader     交互式，按提示走

产物：data/tts_downloader/（相对当前目录！所以要在 ~/go-projects/mini-tools 下跑）
依赖：GOOGLE_TTS_API_KEY，从当前目录的 .env 读
`

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "-h", "--help", "help":
			fmt.Print(usage)
			return
		}
	}

	// 尝试手动读取 .env 文件（简单实现，不使用第三方库，避免依赖报错）
	workDir, _ := os.Getwd()
	envPath := filepath.Join(workDir, ".env")
	loadEnv(envPath)

	// 从环境变量读取 API 密钥，避免硬编码泄露
	apiKey := os.Getenv("GOOGLE_TTS_API_KEY")
	if apiKey == "" {
		fmt.Println("❌ 错误: 未找到 API 密钥！")
		fmt.Println("请在项目根目录创建 .env 文件，并写入：\nGOOGLE_TTS_API_KEY=\"你的密钥\"")
		return
	}

	// 我们使用 data/tts_downloader 目录来专门存放这个工具的文件
	toolDataDir := filepath.Join(workDir, "data", "tts_downloader")
	outputDir := filepath.Join(toolDataDir, "word_audios")
	wordFilePath := filepath.Join(toolDataDir, "word.txt")

	// 创建数据存放文件夹
	if err := os.MkdirAll(toolDataDir, os.ModePerm); err != nil {
		fmt.Printf("创建数据文件夹失败: %v\n", err)
		return
	}

	// 如果 word.txt 不存在，直接提示用户把文件放到正确位置
	if _, err := os.Stat(wordFilePath); os.IsNotExist(err) {
		fmt.Printf("❌ 错误: 找不到输入文件！\n")
		fmt.Printf("请把你的单词文本放在以下路径: %s\n", wordFilePath)
		return
	}

	// 创建存放 MP3 的文件夹
	if err := os.MkdirAll(outputDir, os.ModePerm); err != nil {
		fmt.Printf("创建音频输出文件夹失败: %v\n", err)
		return
	}

	// 2. 读取并清洗文本
	fmt.Println("正在清理文本，提取单词...")
	contentBytes, err := os.ReadFile(wordFilePath)
	if err != nil {
		fmt.Printf("读取失败 (%s): %v\n", wordFilePath, err)
		return
	}
	content := string(contentBytes)

	// 剔除 <source> 这种标签
	reSource := regexp.MustCompile(`<[^>]+>`)
	content = reSource.ReplaceAllString(content, "")

	// 按照逗号或换行符切分
	reSplit := regexp.MustCompile(`[,\n]+`)
	rawWords := reSplit.Split(content, -1)

	var validWords []string
	var invalidWords []string
	// 正则：只允许字母(a-z, A-Z)、空格(\s)和连字符(-)
	validWordRegex := regexp.MustCompile(`^[a-zA-Z\s\-]+$`)

	for _, w := range rawWords {
		w = strings.TrimSpace(w)
		if len(w) == 0 {
			continue
		}
		// 过滤掉单独的 A, B, C 这种大写字母表头
		if len(w) == 1 && w >= "A" && w <= "Z" {
			continue
		}

		// 判断是否是正经单词
		if !validWordRegex.MatchString(w) {
			invalidWords = append(invalidWords, w)
		} else {
			validWords = append(validWords, w)
		}
	}

	// 如果发现有问题的单词，列出并结束程序
	if len(invalidWords) > 0 {
		fmt.Printf("❌ 扫描到 %d 个不合法的单词（包含非法字符如 '/' 等）:\n", len(invalidWords))
		for _, w := range invalidWords {
			fmt.Printf("  - %s\n", w)
		}
		fmt.Println("请修正这些单词后再重新运行程序。")
		return
	}

	fmt.Printf("提取完毕！一共找到 %d 个有效单词。\n", len(validWords))

	wordsToProcess := validWords

	apiUrl := "https://texttospeech.googleapis.com/v1/text:synthesize?key=" + apiKey

	// 4. 并发请求合成语音
	fmt.Println("开始批量并发合成语音...")

	// 控制并发数的 worker 数量，如果被限流可以调小
	const maxWorkers = 5
	jobs := make(chan string, len(wordsToProcess))
	results := make(chan string, len(wordsToProcess))

	var wg sync.WaitGroup

	// 启动 worker
	for w := 1; w <= maxWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for word := range jobs {
				// 替换单词中的斜杠为下划线，防止破坏文件路径
				safeName := strings.ReplaceAll(word, "/", "_")
				safeName = strings.ReplaceAll(safeName, " ", "_")
				outputPath := filepath.Join(outputDir, safeName+".mp3")

				// 如果文件已经存在，跳过，方便断点续传
				if _, err := os.Stat(outputPath); err == nil {
					results <- fmt.Sprintf("✅ 跳过 (已存在): %s", word)
					continue
				}

				// 组装请求数据
				reqData := TTSRequest{}
				reqData.Input.Text = word
				reqData.Voice.LanguageCode = "en-AU"   // 修改为澳洲英语
				reqData.Voice.Name = "en-AU-Neural2-C" // 澳洲口音最贵的一档（女声，如需男声可改为 en-AU-Neural2-B）
				reqData.AudioConfig.AudioEncoding = "MP3"

				jsonData, _ := json.Marshal(reqData)

				// 发送 POST 请求
				resp, err := http.Post(apiUrl, "application/json", bytes.NewBuffer(jsonData))
				if err != nil {
					results <- fmt.Sprintf("❌ 网络请求失败 [%s]: %v", word, err)
					continue
				}

				// 解析返回结果
				body, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()

				var ttsResp TTSResponse
				if err := json.Unmarshal(body, &ttsResp); err != nil {
					results <- fmt.Sprintf("❌ 解析响应失败 [%s]: %v", word, err)
					continue
				}

				if ttsResp.Error != nil {
					results <- fmt.Sprintf("❌ 接口报错 [%s]: %s", word, ttsResp.Error.Message)
					continue
				}

				// 解码 Base64
				audioBytes, err := base64.StdEncoding.DecodeString(ttsResp.AudioContent)
				if err != nil {
					results <- fmt.Sprintf("❌ 解码音频失败 [%s]: %v", word, err)
					continue
				}

				// 保存文件
				if err := os.WriteFile(outputPath, audioBytes, 0644); err != nil {
					results <- fmt.Sprintf("❌ 写入文件失败 [%s]: %v", word, err)
					continue
				}

				results <- fmt.Sprintf("✅ 成功: %s", word)

				// 防止请求过快导致接口限流，视情况可在此休眠
				time.Sleep(100 * time.Millisecond)
			}
		}()
	}

	// 投递任务
	for _, word := range wordsToProcess {
		jobs <- word
	}
	close(jobs)

	// 等待并打印结果
	go func() {
		wg.Wait()
		close(results)
	}()

	for res := range results {
		fmt.Println(res)
	}

	fmt.Printf("\n🎉 测试完成！请去 %s 文件夹听听效果。\n", outputDir)
}

// 简单实现 .env 解析，省去第三方依赖报错问题
func loadEnv(filename string) {
	bytes, err := os.ReadFile(filename)
	if err != nil {
		return
	}
	lines := strings.Split(string(bytes), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if len(line) == 0 || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			val := strings.TrimSpace(parts[1])
			val = strings.Trim(val, `"'`) // 去除首尾引号
			os.Setenv(key, val)
		}
	}
}
