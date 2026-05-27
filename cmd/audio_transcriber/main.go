package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const modelPath = "/Users/jiatongzhou/whisper_models/ggml-large-v3-turbo.bin"

func main() {
	reader := bufio.NewReader(os.Stdin)
	fmt.Print("请输入 MP3/MP4 文件路径: ")
	line, err := reader.ReadString('\n')
	if err != nil {
		fmt.Println("读取输入失败:", err)
		return
	}

	inputPath := cleanPath(line)
	if inputPath == "" {
		fmt.Println("路径不能为空，程序退出。")
		return
	}

	if _, err := os.Stat(inputPath); err != nil {
		fmt.Printf("❌ 找不到文件: %s\n", inputPath)
		return
	}

	// 1. 用 ffmpeg 转成 whisper 需要的 16kHz 单声道 WAV（同时支持 mp3 / mp4）
	wavFile, err := os.CreateTemp("", "audio_transcriber_*.wav")
	if err != nil {
		fmt.Println("创建临时文件失败:", err)
		return
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
		fmt.Printf("\n❌ 音频提取失败: %v\n", err)
		return
	}

	// 2. 用 whisper-cli 识别，输出到与原文件同名的 .txt
	outputBase := strings.TrimSuffix(inputPath, filepath.Ext(inputPath))

	fmt.Println("🗣️  正在语音转文字...")
	whisperCmd := exec.Command("whisper-cli",
		"-m", modelPath,
		"-f", wavPath,
		"-l", "auto",
		"-otxt",
		"-of", outputBase,
		"-pp",
	)
	whisperCmd.Stdout = os.Stdout
	whisperCmd.Stderr = os.Stderr
	if err := whisperCmd.Run(); err != nil {
		fmt.Printf("\n❌ 语音转文字失败: %v\n", err)
		return
	}

	fmt.Printf("\n✅ 完成！文本已写入：\n%s.txt\n", outputBase)
}

// cleanPath 去除空白以及拖拽文件到终端时可能附带的引号
func cleanPath(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "'\"")
	return strings.TrimSpace(s)
}
