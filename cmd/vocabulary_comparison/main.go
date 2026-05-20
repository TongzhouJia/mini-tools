package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/google/generative-ai-go/genai"
	"google.golang.org/api/option"
)

const (
	DataDir              = "/Users/jiatongzhou/GolandProjects/mini-tools/data/vocabulary_comparison"
	VocabFileName        = "vocab.csv"         // 待背单词表
	LearnedVocabFileName = "learned_vocab.csv" // 已背单词表（自动维护，用于过滤）
)

func main() {
	// 尝试手动读取 .env 文件
	workDir, _ := os.Getwd()
	envPath := filepath.Join(workDir, ".env")
	loadEnv(envPath)

	ctx := context.Background()

	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		log.Fatal("❌ 环境变量 GEMINI_API_KEY 为空，请先在终端中执行 export GEMINI_API_KEY=\"你的Key\" 或在项目根目录的 .env 文件中设置。")
	}

	vocabPath := filepath.Join(DataDir, VocabFileName)
	learnedPath := filepath.Join(DataDir, LearnedVocabFileName)

	// 1. 获取字幕文件路径
	reader := bufio.NewReader(os.Stdin)
	fmt.Print("请输入字幕文件的完整路径 (例如 /Users/.../video.en-US.srt): ")
	subPath, _ := reader.ReadString('\n')

	// 完美解决 Mac 终端拖拽路径自带单引号/双引号/空格的问题
	subPath = strings.Trim(subPath, "'\" \t\n\r")

	if subPath == "" || (!strings.HasSuffix(subPath, ".srt") && !strings.HasSuffix(subPath, ".vtt")) {
		fmt.Println("❌ 路径为空或不是有效的字幕文件 (.srt/.vtt)，程序退出。")
		return
	}

	// 2. 提取视频名称并创建专属输出目录
	baseName := filepath.Base(subPath)
	videoName := strings.Split(baseName, ".")[0]
	outputDir := filepath.Join(DataDir, videoName)

	err := os.MkdirAll(outputDir, os.ModePerm)
	if err != nil {
		log.Fatalf("❌ 创建视频专属目录失败: %v\n", err)
	}

	// 3. 加载两份本地单词表
	vocabMap, vocabLines, header, err := loadVocab(vocabPath)
	if err != nil {
		log.Fatalf("❌ 读取待背单词表失败: %v\n请确保文件存在于 %s\n", err, vocabPath)
	}

	learnedMap := loadLearnedVocab(learnedPath)

	fmt.Printf("✅ 成功加载 %d 个待背单词，%d 个已背单词。正在分析字幕...\n", len(vocabMap), len(learnedMap))

	// 4. 解析字幕，提取所有去重后的单词及原句
	videoWordsMap, uniqueWords, err := extractUniqueWordsFromSubtitles(subPath)
	if err != nil {
		log.Fatalf("❌ 处理字幕文件失败: %v\n", err)
	}
	fmt.Printf("✅ 从字幕中提取出去重单词共 %d 个。准备请求 Gemini 进行词形还原...\n", len(uniqueWords))

	// 5. 调用 Gemini 进行批量词形还原
	lemmaMap, err := batchLemmatizeWithGemini(ctx, apiKey, uniqueWords)
	if err != nil {
		log.Fatalf("❌ 请求 API 失败: %v", err)
	}
	fmt.Println("🎉 词形还原完成！开始进行双重对比...")

	// 6. 将还原后的词与两个本地单词表进行对比分类
	matchedWords, newWords := categorizeWords(videoWordsMap, lemmaMap, vocabMap, learnedMap)

	// 7. 更新文件（覆写待背表、追加已背表、生成复习/生词清单）
	err = updateFiles(vocabPath, learnedPath, vocabLines, header, outputDir, matchedWords, newWords)
	if err != nil {
		log.Fatalf("❌ 写入文件失败: %v\n", err)
	}

	fmt.Printf("\n🎉 全部搞定！任务报告：\n")
	fmt.Printf("- 命中了 %d 个待背词汇！(已从 vocab.csv 移至 learned_vocab.csv)\n", len(matchedWords))
	fmt.Printf("- 拦截了已背单词干扰，提取出真正的未知生词 %d 个。\n", len(newWords))
	fmt.Printf("- 结果已保存在目录: %s\n", outputDir)
}

// ---------------- 以下为功能函数 ----------------

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

// loadVocab 读取待背单词表 (CSV格式)
func loadVocab(filepath string) (map[string]string, []string, string, error) {
	file, err := os.Open(filepath)
	if err != nil {
		return nil, nil, "", err
	}
	defer file.Close()

	vocabMap := make(map[string]string)
	var vocabLines []string
	var header string

	scanner := bufio.NewScanner(file)
	isFirstLine := true

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		if isFirstLine && strings.Contains(strings.ToLower(line), "english") {
			header = line
			isFirstLine = false
			continue
		}
		isFirstLine = false
		vocabLines = append(vocabLines, line)

		parts := strings.Split(line, ",")
		if len(parts) >= 2 {
			word := strings.ToLower(strings.TrimSpace(parts[0]))
			meaning := strings.TrimSpace(parts[1])
			if word != "" {
				vocabMap[word] = meaning
			}
		}
	}
	return vocabMap, vocabLines, header, scanner.Err()
}

// loadLearnedVocab 读取已背单词表 (如果不存在则返回空 map，不报错)
// 将忽略空行、"----------" 分隔符，以及代表日期时间的格式（如包含 "-" 和 ":"）。
func loadLearnedVocab(filepath string) map[string]bool {
	learnedMap := make(map[string]bool)
	file, err := os.Open(filepath)
	if err != nil {
		return learnedMap // 文件不存在属于正常情况，直接返回空 map
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "----------") || (len(line) >= 10 && line[4] == '-' && line[7] == '-') {
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) >= 1 && line != "" {
			word := strings.ToLower(strings.TrimSpace(parts[0]))
			learnedMap[word] = true
		}
	}
	return learnedMap
}

// extractUniqueWordsFromSubtitles 解析字幕
func extractUniqueWordsFromSubtitles(subPath string) (map[string]string, []string, error) {
	file, err := os.Open(subPath)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()

	videoWordsMap := make(map[string]string)
	var uniqueWords []string
	wordRegex := regexp.MustCompile(`[a-zA-Z]+`)
	scanner := bufio.NewScanner(file)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.Contains(line, "-->") || (!strings.ContainsAny(line, "a-zA-Z") && wordRegex.MatchString(line)) {
			continue
		}

		wordsInLine := wordRegex.FindAllString(line, -1)
		for _, w := range wordsInLine {
			word := strings.ToLower(w)
			if len(word) == 1 && word != "a" && word != "i" {
				continue
			}
			if _, exists := videoWordsMap[word]; !exists {
				videoWordsMap[word] = line
				uniqueWords = append(uniqueWords, word)
			}
		}
	}
	return videoWordsMap, uniqueWords, scanner.Err()
}

// batchLemmatizeWithGemini 批量还原词形
func batchLemmatizeWithGemini(ctx context.Context, apiKey string, words []string) (map[string]string, error) {
	client, err := genai.NewClient(ctx, option.WithAPIKey(apiKey))
	if err != nil {
		return nil, err
	}
	defer client.Close()

	model := client.GenerativeModel("gemini-flash-lite-latest")
	model.ResponseMIMEType = "application/json"

	finalLemmaMap := make(map[string]string)
	batchSize := 150

	for i := 0; i < len(words); i += batchSize {
		end := i + batchSize
		if end > len(words) {
			end = len(words)
		}
		batch := words[i:end]

		prompt := fmt.Sprintf(`You are a lemmatizer. Convert the following English words to their base/dictionary form (lemma). 
If the word is already in base form, keep it as is.
Return ONLY a flat JSON object where the key is the original word and the value is the base form. 
Words to process: %s`, strings.Join(batch, ", "))

		fmt.Printf("⏳ 正在请求 API 处理第 %d 到 %d 个单词...\n", i+1, end)
		resp, err := model.GenerateContent(ctx, genai.Text(prompt))
		if err != nil {
			return nil, fmt.Errorf("生成内容失败: %w", err)
		}

		if len(resp.Candidates) > 0 && len(resp.Candidates[0].Content.Parts) > 0 {
			jsonStr := fmt.Sprintf("%v", resp.Candidates[0].Content.Parts[0])
			jsonStr = strings.TrimPrefix(jsonStr, "```json\n")
			jsonStr = strings.TrimSuffix(jsonStr, "\n```")

			var batchResult map[string]string
			err = json.Unmarshal([]byte(jsonStr), &batchResult)
			if err != nil {
				fmt.Printf("⚠️ JSON 解析警告 (将跳过此批次): %v\n原始输出: %s\n", err, jsonStr)
				continue
			}

			for k, v := range batchResult {
				finalLemmaMap[strings.ToLower(k)] = strings.ToLower(v)
			}
		}
	}
	return finalLemmaMap, nil
}

// categorizeWords 进行双重过滤对比
func categorizeWords(videoWordsMap map[string]string, lemmaMap map[string]string, vocabMap map[string]string, learnedMap map[string]bool) (map[string][2]string, map[string]string) {
	matchedWords := make(map[string][2]string)
	newWords := make(map[string]string)

	for originalWord, sentence := range videoWordsMap {
		baseWord, existsInLemma := lemmaMap[originalWord]
		if !existsInLemma || baseWord == "" {
			baseWord = originalWord
		}

		// 第一层：在待背表中吗？
		if meaning, existsInVocab := vocabMap[baseWord]; existsInVocab {
			if _, alreadyFound := matchedWords[baseWord]; !alreadyFound {
				matchedWords[baseWord] = [2]string{meaning, sentence}
			}
		} else {
			// 第二层：不在待背表，那在已背表中吗？
			// 只有不在已背表中，才算真正的未知生词！
			if !learnedMap[baseWord] {
				if _, alreadyFound := newWords[originalWord]; !alreadyFound {
					newWords[originalWord] = sentence
				}
			}
		}
	}
	return matchedWords, newWords
}

// updateFiles 统一处理文件更新
//   - 覆写待背表 (vocab.csv)，剔除已掌握单词
//   - 将新掌握的单词追加到已背表 (learned_vocab.csv)，并注入日期时间与 "----------" 分隔符以作区分
//   - 在视频专属目录下生成已掌握复习清单 (matched_words.md)，使用选项卡格式
//   - 在视频专属目录下生成全新生词清单 (new_words_in_video.txt)
func updateFiles(vocabPath string, learnedPath string, vocabLines []string, header string, outputDir string, matchedWords map[string][2]string, newWords map[string]string) error {
	// 1. 覆写原始待背单词表 vocab.csv
	vocabFile, err := os.Create(vocabPath)
	if err != nil {
		return err
	}
	defer vocabFile.Close()

	writer := bufio.NewWriter(vocabFile)
	if header != "" {
		writer.WriteString(header + "\n")
	}

	// 记录哪些行是被剔除的（准备加入已背表）
	var wordsToAppendToLearned []string

	for _, line := range vocabLines {
		parts := strings.Split(line, ",")
		if len(parts) >= 1 {
			word := strings.ToLower(strings.TrimSpace(parts[0]))
			if _, isMatched := matchedWords[word]; !isMatched {
				// 未命中，继续留在待背表
				writer.WriteString(line + "\n")
			} else {
				// 命中了，准备挪进已背表（保持原始带中文释义的 CSV 格式）
				wordsToAppendToLearned = append(wordsToAppendToLearned, line)
			}
		}
	}
	writer.Flush()

	// 2. 自动维护：将这次背会的单词追加到 learned_vocab.csv 中
	if len(wordsToAppendToLearned) > 0 {
		learnedFile, err := os.OpenFile(learnedPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return err
		}
		defer learnedFile.Close()
		learnedWriter := bufio.NewWriter(learnedFile)

		// 如果文件是新创建的，顺便把表头写进去
		stat, _ := learnedFile.Stat()
		if stat.Size() == 0 && header != "" {
			learnedWriter.WriteString(header + "\n")
		}

		// 写入日期时间和分隔符，便于后续按时间追溯已背词汇
		nowStr := time.Now().Format("2006-01-02 15:04:05")
		learnedWriter.WriteString(nowStr + "\n")
		learnedWriter.WriteString("----------\n")

		for _, line := range wordsToAppendToLearned {
			learnedWriter.WriteString(line + "\n")
		}
		learnedWriter.Flush()
	}

	// 3. 生成本次复习表 (Markdown 格式)
	matchedPath := filepath.Join(outputDir, "matched_words.md")
	mFile, err := os.Create(matchedPath)
	if err != nil {
		return err
	}
	defer mFile.Close()
	mWriter := bufio.NewWriter(mFile)
	mWriter.WriteString("# 本期视频：已掌握单词复习\n\n")
	for word, data := range matchedWords {
		meaning := data[0]
		sentence := data[1]
		mWriter.WriteString(fmt.Sprintf("- [ ] **%s**\n    - 释义：%s\n    - 原句：%s\n\n", word, meaning, sentence))
	}
	mWriter.Flush()

	// 4. 生成本次真正未见生词表
	newPath := filepath.Join(outputDir, "new_words_in_video.txt")
	nFile, err := os.Create(newPath)
	if err != nil {
		return err
	}
	defer nFile.Close()
	nWriter := bufio.NewWriter(nFile)
	nWriter.WriteString("======== 本期视频：未见生词捕获 ========\n\n")
	for word, sentence := range newWords {
		nWriter.WriteString(fmt.Sprintf("真正生词(原形或变体): %s\n视频原句: %s\n\n", word, sentence))
	}
	nWriter.Flush()

	return nil
}
