package main

import (
	"fmt"
	"github.com/gin-gonic/gin"
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
	cleanDurationFormula = "0 * * * *" // 每小时执行 cronjob
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
var codesMap = sync.Map{}

type ResourceInfo struct {
	uploadTS               time.Time
	bytes                  int
	availableDownloadTimes int
}

func NewResourceInfo(uploadTS time.Time, bytes, availableDownloadTimes int) *ResourceInfo {
	return &ResourceInfo{
		uploadTS:               uploadTS,
		bytes:                  bytes,
		availableDownloadTimes: availableDownloadTimes,
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

func main() {
	gin.SetMode(gin.ReleaseMode)
	r := gin.Default()

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
			if _, ok := codesMap.Load(codeStr); !ok {
				break
			}
		}
		codesMap.Store(codeStr, NewResourceInfo(time.Now(), int(file.Size), maxDownloadTimes))
		fmt.Println(codesMap)

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
		resourceInfo, ok := codesMap.Load(codeStr)
		if !ok || resourceInfo.(*ResourceInfo).availableDownloadTimes == 0 || time.Now().Sub(resourceInfo.(*ResourceInfo).uploadTS) > resourceTTL {
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

		resourceInfo.(*ResourceInfo).availableDownloadTimes--
		codesMap.Store(codeStr, resourceInfo)
	})

	// 创建一个cron调度器
	cronJob := cron.New()
	// 定义定时任务
	spec := cleanDurationFormula // 每5秒执行一次
	// 注册定时任务
	_, err := cronJob.AddFunc(spec, func() {
		fmt.Println("执行定时任务")
		codesMap.Range(func(k, v interface{}) bool {
			if time.Now().Sub(v.(*ResourceInfo).uploadTS) > resourceTTL {
				err := doClean(path.Join(savePath, k.(string)))
				if err != nil {
					fmt.Println("删除失败")
				}
			}
			return true
		})
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
	return os.RemoveAll(dir)
}
