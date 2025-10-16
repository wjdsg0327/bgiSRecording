package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"gopkg.in/yaml.v2"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed web/*
var embeddedFiles embed.FS

type Config struct {
	ServerAddr string   `yaml:"serverAddr"`
	FileName   string   `yaml:"fileName"`
	Keywords   []string `yaml:"keywords"`
}

var config Config

func init() {
	data, err := os.ReadFile("config.yaml")
	if err != nil {
		log.Fatalf("读取配置文件失败: %v", err)
	}
	if err := yaml.Unmarshal(data, &config); err != nil {
		log.Fatalf("解析配置文件失败: %v", err)
	}
	log.Printf("配置文件内容: %+v", config)
}

const (
	videoDir      = "./videos"
	errorVideoDir = "./error_videos"
	maxVideos     = 5
	segmentTime   = 180 // 每段3分钟
	ffmpegExePath = "./ffmpeg.exe"
)

var (
	lastTrigger time.Time
	copyLock    sync.Mutex
)

func main() {
	// 创建视频目录
	if err := os.MkdirAll(videoDir, os.ModePerm); err != nil {
		log.Fatalf("创建视频目录失败: %v", err)
	}

	// 启动清理线程
	go func() {
		for {
			time.Sleep(60 * time.Second)
			cleanupOldVideos()
		}
	}()

	// 启动 WebSocket 日志监听
	go func() {
		u := url.URL{Scheme: "ws", Host: config.ServerAddr, Path: "/ws/" + config.FileName}
		wsBgiLog(u.String())
	}()

	// 启动 Web 界面
	go func() {
		startWebServer()
	}()

	// 启动录制
	startFFmpeg()
}

func startFFmpeg() {
	outputPattern := filepath.Join(videoDir, "record_%03d.mp4")
	log.Println("开始录制，每3分钟自动分段，并只保留最新5段...")

	cmd := exec.Command(ffmpegExePath,
		"-y",
		"-f", "gdigrab",
		"-framerate", "30",
		"-video_size", "1920x1080",
		"-i", "desktop",
		"-f", "lavfi", "-i", "anullsrc",
		"-vcodec", "libx264",
		"-preset", "veryfast",
		"-b:v", "2500k",
		"-profile:v", "baseline",
		"-maxrate", "2500k",
		"-bufsize", "5000k",
		"-level", "3.0",
		"-pix_fmt", "yuv420p",
		"-c:a", "aac",
		"-f", "segment",
		"-segment_time", fmt.Sprintf("%d", segmentTime),
		"-reset_timestamps", "1",
		outputPattern,
	)

	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		log.Printf("录制失败: %v", err)
	}
}

// ---------------- WebSocket 日志监听 ----------------
func wsBgiLog(wsURL string) {
	for {
		err := connectAndListen(wsURL)
		log.Printf("WebSocket断开，3秒后重连: %v", err)
		time.Sleep(3 * time.Second)
	}
}

func connectAndListen(wsURL string) error {
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return fmt.Errorf("连接失败: %v", err)
	}
	defer conn.Close()
	log.Println("✅ WebSocket 已连接，开始接收日志...")

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		handleLogMessage(string(msg))
	}
}

func handleLogMessage(msg string) {
	log.Printf("[日志] %s", msg)
	for _, keyword := range config.Keywords {
		if strings.Contains(msg, keyword) {
			if time.Since(lastTrigger) < 3*time.Minute {
				log.Printf("检测到关键词 [%s]，但仍在冷却期。", keyword)
				return
			}
			lastTrigger = time.Now()
			log.Printf("检测到关键词 [%s]，2分钟后复制最近2个视频...", keyword)
			go func() {
				time.Sleep(2 * time.Minute)
				if err := copyLatestVideos(2); err != nil {
					log.Printf("复制视频出错: %v", err)
				} else {
					log.Printf("✅ 已复制最新2个视频到 error_videos 文件夹。")
				}
			}()
		}
	}
}

// ---------------- 视频管理 ----------------
func cleanupOldVideos() {
	files, err := filepath.Glob(filepath.Join(videoDir, "record_*.mp4"))
	if err != nil {
		log.Printf("获取视频列表失败: %v", err)
		return
	}
	if len(files) <= maxVideos {
		return
	}
	sort.Slice(files, func(i, j int) bool {
		fi, _ := os.Stat(files[i])
		fj, _ := os.Stat(files[j])
		return fi.ModTime().Before(fj.ModTime())
	})
	for _, f := range files[:len(files)-maxVideos] {
		log.Printf("🗑 删除旧视频: %s", f)
		os.Remove(f)
	}
}

func waitForFileStable(filePath string, timeout time.Duration) bool {
	start := time.Now()
	lastSize := int64(-1)
	stableCount := 0
	for time.Since(start) < timeout {
		fi, err := os.Stat(filePath)
		if err != nil {
			return false
		}
		if fi.Size() == lastSize {
			stableCount++
			if stableCount >= 2 {
				return true
			}
		} else {
			stableCount = 0
			lastSize = fi.Size()
		}
		time.Sleep(5 * time.Second)
	}
	return false
}

func fetchIndexData() (map[string]interface{}, error) {
	resp, err := http.Get("http://" + config.ServerAddr + "/api/index")
	if err != nil {
		return nil, fmt.Errorf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("解析JSON失败: %v", err)
	}
	return m, nil
}

func copyLatestVideos(n int) error {
	copyLock.Lock()
	defer copyLock.Unlock()

	if err := os.MkdirAll(errorVideoDir, os.ModePerm); err != nil {
		return fmt.Errorf("创建错误视频目录失败: %v", err)
	}

	indexData, err := fetchIndexData()
	if err != nil {
		log.Printf("无法获取索引信息: %v", err)
		indexData = map[string]interface{}{
			"scriptName": "unknown",
			"line":       "unknown",
		}
	}

	files, err := filepath.Glob(filepath.Join(videoDir, "record_*.mp4"))
	if err != nil {
		return fmt.Errorf("获取视频列表失败: %v", err)
	}
	if len(files) == 0 {
		return fmt.Errorf("没有找到可复制的视频")
	}

	sort.Slice(files, func(i, j int) bool {
		fi, _ := os.Stat(files[i])
		fj, _ := os.Stat(files[j])
		return fi.ModTime().After(fj.ModTime())
	})

	if len(files) > n {
		files = files[:n]
	}

	for _, f := range files {
		log.Printf("检测文件是否稳定: %s", f)
		if !waitForFileStable(f, 30*time.Second) {
			log.Printf("文件未稳定，跳过: %s", f)
			continue
		}
		errFileName := fmt.Sprintf("%s_%s_%s.mp4",
			fmt.Sprint(indexData["scriptName"]),
			filepath.Base(fmt.Sprint(indexData["line"])),
			time.Now().Format("20060102150405"),
		)
		dest := filepath.Join(errorVideoDir, errFileName)

		data, err := os.ReadFile(f)
		if err != nil {
			log.Printf("读取视频失败: %v", err)
			continue
		}
		if err := os.WriteFile(dest, data, 0644); err != nil {
			log.Printf("写入视频失败: %v", err)
			continue
		}
		log.Printf("✅ 复制完成: %s → %s", f, dest)
	}
	return nil
}

// ---------------- Gin Web 部分 ----------------
func startWebServer() {
	gin.SetMode(gin.ReleaseMode)
	router := gin.Default()

	subFS, _ := fs.Sub(embeddedFiles, "web")
	router.StaticFS("/web", http.FS(subFS))

	router.GET("/", func(c *gin.Context) {
		data, err := subFS.Open("index.html")
		if err != nil {
			c.String(http.StatusNotFound, "index.html not found")
			return
		}
		c.DataFromReader(http.StatusOK, -1, "text/html; charset=utf-8", data, nil)
	})

	router.Static("/error_videos", errorVideoDir)

	router.GET("/api/errors", func(c *gin.Context) {
		files, err := listErrorVideos()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"count": len(files), "data": files})
	})

	router.GET("/api/error/:name", func(c *gin.Context) {
		name := filepath.Base(c.Param("name"))
		full := filepath.Join(errorVideoDir, name)
		if _, err := os.Stat(full); os.IsNotExist(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "文件不存在"})
			return
		}
		c.File(full)
	})

	log.Println("🌐 服务启动: http://localhost:10189")
	router.Run(":10189")
}

func listErrorVideos() ([]gin.H, error) {
	files, err := filepath.Glob(filepath.Join(errorVideoDir, "*.mp4"))
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool {
		fi, _ := os.Stat(files[i])
		fj, _ := os.Stat(files[j])
		return fi.ModTime().After(fj.ModTime())
	})
	var res []gin.H
	for _, f := range files {
		info, _ := os.Stat(f)
		res = append(res, gin.H{
			"name": info.Name(),
			"size": info.Size(),
			"time": info.ModTime().Format("2006-01-02 15:04:05"),
			"url":  "/error_videos/" + info.Name(),
		})
	}
	return res, nil
}
