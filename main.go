package main

import (
	"encoding/json"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/robfig/cron/v3"
	"io"
	"log"
	"math"
	"math/rand"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

const (
	maxBytes             = (1 << 20) * 100 // 每次上传最多100MB
	codeLen              = 4
	savePath             = "./"
	resourceTTL          = 8 * time.Hour
	cleanDurationFormula = "0 3 * * *" // 每天凌晨三点执行 cronjob
	tokenRate            = 1           // 每秒生成的令牌数
	bucketCapacity       = 20          // 令牌桶容量
	maxDownloadTimes     = 3
)

type IPLimiter struct {
	mu       sync.Mutex
	tokens   int
	lastTS   time.Time
	capacity int
	rate     time.Duration
}

func NewIPLimiter(capacity int, rate time.Duration) *IPLimiter {
	return &IPLimiter{
		capacity: capacity,
		rate:     rate,
		tokens:   capacity,
	}
}

var ipLimiterMap = sync.Map{}

type ResourceInfo struct {
	UploadTS               time.Time
	Bytes                  int
	AvailableDownloadTimes int
}

func NewResourceInfo(uploadTS time.Time, bytes, availableDownloadTimes int) *ResourceInfo {
	return &ResourceInfo{
		UploadTS:               uploadTS,
		Bytes:                  bytes,
		AvailableDownloadTimes: availableDownloadTimes,
	}
}

func (l *IPLimiter) take() bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(l.lastTS)
	fillTokens := int(elapsed / l.rate)
	if fillTokens > l.capacity {
		fillTokens = l.capacity
	}

	l.tokens += fillTokens
	if l.tokens > l.capacity {
		l.tokens = l.capacity
	}

	if l.tokens > 0 {
		l.tokens--
		l.lastTS = now
		return true
	}

	return false
}

func RateLimitMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		ip := c.Request.RemoteAddr
		fmt.Printf("ip: %v", ip)
		limiter, ok := ipLimiterMap.Load(ip)
		if !ok {
			limiter = NewIPLimiter(bucketCapacity, tokenRate)
			ipLimiterMap.Store(ip, limiter)
		}

		if !limiter.(*IPLimiter).take() {
			c.String(http.StatusTooManyRequests, "访问太频繁")
			c.Abort()
		}

		// 处理请求
		c.Next()
	}
}

func createRedisClient() *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr:     "60.204.242.203:6379", // Redis 服务器地址和端口
		Password: "213222204",           // 如果有密码，设置密码
		DB:       0,                     // 使用的数据库编号
	})
}

func main() {
	gin.SetMode(gin.ReleaseMode)
	r := gin.Default()
	redisClient := createRedisClient()

	// 添加 CORS 中间件
	r.Use(func(c *gin.Context) {
		c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		c.Writer.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
		c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	})

	r.GET("/ping", RateLimitMiddleware(), func(c *gin.Context) {
		c.JSON(200, gin.H{
			"message": "pong",
		})
	})

	r.POST("/upload", RateLimitMiddleware(), func(c *gin.Context) {
		file, err := c.FormFile("file")
		if err != nil {
			c.String(http.StatusInternalServerError, "上传图片出错")
			return
		}

		// 文件大于 maxBytes 不接收
		if file.Size > (maxBytes) {
			c.String(http.StatusBadRequest, "File too large")
			return
		}

		// 生成码
		rand.Seed(time.Now().UnixNano())
		var code int
		var codeStr string
		for {
			code = rand.Intn(int(math.Pow(10, codeLen)))
			codeStr = codeToString(code, codeLen)
			_, err := redisClient.Get(c, codeStr).Result()
			if err == redis.Nil {
				break
			}
		}

		resourceInfo := NewResourceInfo(time.Now(), int(file.Size), maxDownloadTimes)
		resourceInfoBytes, err := json.Marshal(resourceInfo)
		if err != nil {
			fmt.Println(err)
			return
		}
		fmt.Printf("resource json: %v", string(resourceInfoBytes))
		redisClient.Set(c, codeStr, string(resourceInfoBytes), resourceTTL)

		dst := path.Join(savePath, codeStr, file.Filename)
		fmt.Println(dst)
		err = c.SaveUploadedFile(file, dst)
		if err != nil {
			c.String(http.StatusInternalServerError, "保存图片出错")
			return
		}

		c.String(http.StatusOK, codeStr)
	})

	r.GET("/download", RateLimitMiddleware(), func(c *gin.Context) {
		codeStr := c.Query("code")
		resourceInfoStr, err := redisClient.Get(c, codeStr).Result()
		if err != nil {
			c.String(http.StatusBadRequest, "Invalid code")
			return
		}

		var resourceInfo ResourceInfo
		err = json.Unmarshal([]byte(resourceInfoStr), &resourceInfo)
		if err != nil {
			c.String(http.StatusInternalServerError, "资源信息有误")
			return
		}
		if resourceInfo.AvailableDownloadTimes == 0 || time.Now().Sub(resourceInfo.UploadTS) > resourceTTL {
			c.String(http.StatusBadRequest, "Invalid code")
			return
		}

		dirPath := path.Join(savePath, codeStr)
		filePath := getFirstFileInDirectory(dirPath)
		file, err := os.Open(filePath)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to open file"})
			return
		}
		defer file.Close()

		encodedFilename := mime.QEncoding.Encode("UTF-8", filepath.Base(file.Name()))
		c.Header("Content-Disposition", "attachment; filename="+encodedFilename)
		c.Header("Content-Type", "application/octet-stream")
		// 设置允许暴露的响应头
		c.Writer.Header().Set("Access-Control-Expose-Headers", "Content-Disposition")
		c.Status(http.StatusOK)

		_, err = io.Copy(c.Writer, file)
		if err != nil {
			log.Println(err)
		}

		resourceInfo.AvailableDownloadTimes--
		resourceInfoBytes, err := json.Marshal(resourceInfo)
		if err != nil {
			fmt.Println(err)
			return
		}
		fmt.Println(string(resourceInfoBytes))
		redisClient.Set(c, codeStr, string(resourceInfoBytes), redis.KeepTTL)
	})

	// 创建一个 cron 调度器
	cronJob := cron.New()
	// 定义定时任务
	spec := cleanDurationFormula
	// 注册定时任务
	_, err := cronJob.AddFunc(spec, func() {
		doClean(savePath)
	})
	if err != nil {
		log.Fatal(err)
	}

	// 启动cron调度器
	cronJob.Start()
	// 确保应用退出时停止cron
	defer cronJob.Stop()

	// listen and serve on 0.0.0.0:8080
	r.Run()
}

func codeToString(code, limitLen int) string {
	s := strconv.Itoa(code)
	for len(s) < limitLen {
		s = "0" + s
	}

	return s
}

func getFirstFileInDirectory(dirPath string) string {
	dir, err := os.Open(dirPath)
	if err != nil {
		fmt.Println("Error opening directory:", err)
		return ""
	}
	defer dir.Close()

	fileInfos, err := dir.Readdir(-1)
	if err != nil {
		fmt.Println("Error reading directory:", err)
		return ""
	}

	for _, fileInfo := range fileInfos {
		if !fileInfo.IsDir() {
			return path.Join(dirPath, fileInfo.Name())
		}
	}

	return ""
}

func doClean(dir string) error {
	now := time.Now()
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() {
			if now.Sub(info.ModTime()) > resourceTTL {
				fmt.Printf("Deleting directory: %s\n", path)
				return os.RemoveAll(path)
			}
		}
		return nil
	})
	return err
}
