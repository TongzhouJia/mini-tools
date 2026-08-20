package main

import (
	"bufio"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

const usage = `audio_transcriber —— 音视频转文字（whisper.cpp，有独显就走 CUDA）

用法：
  audio_transcriber              交互式：提示你输入文件夹，批量转里面所有音视频
  audio_transcriber -f xxx.mp3   只转这一个文件（mp3 / mp4 都行）

产物（跟源文件同目录、同名，一次出两个）：
  xxx.txt   整篇纯文本
  xxx.srt   带时间轴的字幕

行为：
  断点续传 —— 同名 .txt 已存在就跳过，中断了直接重跑，不会白干
  实测 RTX 4060 + large-v3 约 14 倍速（1 小时音频约 4 分钟）

依赖：ffmpeg、whisper-cli（whisper.cpp）
环境变量：
  WHISPER_MODEL  模型文件路径（默认 ~/ggml-large-v3.bin）
  WHISPER_BIN    whisper 可执行文件（默认 whisper-cli）

参数：
`

// 模型路径和 whisper 可执行文件都可以用环境变量覆盖。
var (
	modelPath  = envOr("WHISPER_MODEL", defaultPath("ggml-large-v3.bin"))
	whisperBin = envOr("WHISPER_BIN", "whisper-cli")
)

// envOr 取环境变量，为空时回退到默认值。
func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// defaultPath 拼一个相对于用户主目录的默认路径。
func defaultPath(elem ...string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(elem...)
	}
	return filepath.Join(append([]string{home}, elem...)...)
}

// checkDeps 开跑前先确认外部依赖都在，免得处理到一半才炸。
func checkDeps() error {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return fmt.Errorf("❌ 找不到 ffmpeg，请先安装（用于抽取音轨）")
	}
	if _, err := exec.LookPath(whisperBin); err != nil {
		return fmt.Errorf("❌ 找不到 %s，请先安装 whisper.cpp，或用 WHISPER_BIN 指定可执行文件路径", whisperBin)
	}
	if _, err := os.Stat(modelPath); err != nil {
		return fmt.Errorf("❌ 找不到模型文件: %s\n   用 WHISPER_MODEL 环境变量指定实际位置", modelPath)
	}
	return nil
}

func main() {
	flag.CommandLine.SetOutput(os.Stdout)
	flag.Usage = func() {
		fmt.Print(usage)
		flag.PrintDefaults()
	}
	singleFile := flag.String("f", "", "只处理这一个音视频文件（给了就跳过交互式输入文件夹）")
	flag.Parse()

	if err := checkDeps(); err != nil {
		fmt.Println(err)
		return
	}

	// -f 指定单个文件时，直接处理它，不再问文件夹
	if p := cleanPath(*singleFile); p != "" {
		info, err := os.Stat(p)
		if err != nil {
			fmt.Printf("❌ 找不到文件: %s\n", p)
			return
		}
		if info.IsDir() {
			fmt.Printf("❌ -f 要的是文件不是文件夹: %s\n", p)
			return
		}
		process([]string{p})
		return
	}

	reader := bufio.NewReader(os.Stdin)
	fmt.Print("请输入包含 MP3/MP4 文件的文件夹路径: ")
	line, err := reader.ReadString('\n')
	if err != nil {
		fmt.Println("读取输入失败:", err)
		return
	}

	dirPath := cleanPath(line)
	if dirPath == "" {
		fmt.Println("路径不能为空，程序退出。")
		return
	}

	info, err := os.Stat(dirPath)
	if err != nil {
		fmt.Printf("❌ 找不到文件夹: %s\n", dirPath)
		return
	}
	if !info.IsDir() {
		fmt.Printf("❌ 该路径不是文件夹: %s\n", dirPath)
		return
	}

	// 递归收集文件夹（含所有嵌套子文件夹）下的 mp3 / mp4 文件
	var files []string
	walkErr := filepath.WalkDir(dirPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			fmt.Printf("⚠️  访问失败，跳过：%s（%v）\n", path, err)
			return nil
		}
		if d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(d.Name()))
		if ext != ".mp3" && ext != ".mp4" {
			fmt.Printf("⏭️  跳过非 MP3/MP4 文件：%s\n", path)
			return nil
		}
		files = append(files, path)
		return nil
	})
	if walkErr != nil {
		fmt.Println("读取文件夹失败:", walkErr)
		return
	}
	sort.Strings(files)

	if len(files) == 0 {
		fmt.Println("⚠️  文件夹下没有找到 MP3/MP4 文件。")
		return
	}

	process(files)
}

// process 逐个转写，已经有 .txt 的直接跳过（可续跑）。
func process(files []string) {
	fmt.Printf("共找到 %d 个文件，开始逐个处理...\n\n", len(files))

	var done, skipped, failed int
	for i, inputPath := range files {
		fmt.Printf("[%d/%d] %s\n", i+1, len(files), filepath.Base(inputPath))

		outputBase := strings.TrimSuffix(inputPath, filepath.Ext(inputPath))
		// 断点续传：如果已经有写好的 .txt，直接跳过
		if _, err := os.Stat(outputBase + ".txt"); err == nil {
			fmt.Printf("⏭️  已存在文本，跳过：%s.txt\n\n", outputBase)
			skipped++
			continue
		}

		if err := transcribe(inputPath, outputBase); err != nil {
			fmt.Printf("❌ 处理失败: %v\n\n", err)
			failed++
			continue
		}

		fmt.Printf("✅ 完成：%s.txt / .srt\n\n", outputBase)
		done++
	}

	fmt.Printf("全部结束。成功 %d 个，跳过 %d 个，失败 %d 个。\n", done, skipped, failed)
}

// transcribe 提取音频并语音转文字，输出到 outputBase + ".txt"
func transcribe(inputPath, outputBase string) error {
	// 1. 用 ffmpeg 转成 whisper 需要的 16kHz 单声道 WAV（同时支持 mp3 / mp4）
	wavFile, err := os.CreateTemp("", "audio_transcriber_*.wav")
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	wavPath := wavFile.Name()
	wavFile.Close()
	defer os.Remove(wavPath)

	fmt.Println("⏳ 正在提取音频...")
	ffmpegCmd := exec.Command("ffmpeg",
		"-y",
		"-i", inputPath,
		"-vn",
		"-ar", "16000",
		"-ac", "1",
		"-c:a", "pcm_s16le",
		wavPath,
	)
	ffmpegCmd.Stderr = os.Stderr
	if err := ffmpegCmd.Run(); err != nil {
		return fmt.Errorf("音频提取失败: %w", err)
	}

	// 2. 用 whisper-cli 识别，输出与原文件同名的 .txt（纯文本）和 .srt（带时间轴的字幕）
	fmt.Println("🗣️  正在语音转文字...")
	whisperCmd := exec.Command(whisperBin,
		"-m", modelPath,
		"-f", wavPath,
		"-l", "auto",
		"-otxt",
		"-osrt",
		"-of", outputBase,
		"-pp",
	)
	whisperCmd.Stdout = os.Stdout
	whisperCmd.Stderr = os.Stderr
	if err := whisperCmd.Run(); err != nil {
		return fmt.Errorf("语音转文字失败: %w", err)
	}

	return nil
}

// cleanPath 去除空白以及拖拽文件到终端时可能附带的引号
func cleanPath(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "'\"")
	return strings.TrimSpace(s)
}
