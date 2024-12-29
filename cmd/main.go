package main

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"ra2web-proxy/pkg/utils"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/andybalholm/brotli"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"golang.org/x/sync/singleflight"
)

type Config struct {
	MainTargetURL  string   `json:"main_target_url"`
	MainEntryList  []string `json:"main_entry_list"`
	ResTargetURL   string   `json:"res_target_url"`
	ResEntryList   []string `json:"res_entry_list"`
	ApiEndpoint    []string `json:"api_endpoint"`
	AllowedOrigins []string `json:"allowed_origins"`
	BaseHref       string   `json:"base_href"`
	Port           int      `json:"port"`
}

type LogEntry struct {
	ClientIP   string        `json:"client_ip"`
	RequestURL string        `json:"request_url"`
	Method     string        `json:"method"`
	UserAgent  string        `json:"user_agent"`
	StatusCode int           `json:"status_code"`
	Latency    time.Duration `json:"latency"`
}

type CacheRequest struct {
	Site      string `json:"site"`
	CacheType string `json:"cacheType"`
	FilePath  string `json:"filePath"`
}

type LogMessage struct {
	ClientIP    string
	RequestURL  string
	Method      string
	UserAgent   string
	StatusCode  int
	Latency     time.Duration
	CacheHit    bool   // 是否命中缓存
	CachePath   string // 缓存路径
	UpstreamURL string // 上游URL
	Error       error  // 错误信息
}

// HackActionType 定义枚举值
type HackActionType string

var (
	targetsMap     sync.Map
	targetsTypeMap sync.Map
	logChannel     = make(chan LogMessage, 10000)
	singleGroup    singleflight.Group
	config         Config
	allowedOrigins sync.Map
)

// ModifyActionType 定义修改动作类型的枚举值
type ModifyActionType string

const (
	Insert    ModifyActionType = "insert"
	Delete    ModifyActionType = "delete"
	Replace   ModifyActionType = "replace"
	ReplaceJS ModifyActionType = "replaceJS"
)

// ModifyPoint 定义修改点的数据结构
type ModifyPoint struct {
	Action     ModifyActionType `json:"action"`     // 操作类型: insert/delete/replace/replaceJS
	Selector   string           `json:"selector"`   // CSS 选择器
	Position   string           `json:"position"`   // 插入位置: before/after，仅在插入操作时使用
	Content    string           `json:"content"`    // 要插入或替换的内容
	OldContent string           `json:"oldContent"` // 旧的内联JS内容，仅在replaceJS操作时使用
	NewContent string           `json:"newContent"` // 新的内联JS内容，仅在replaceJS操作时使用
}

// HackDetail 定义详细操作的数据结构
type HackDetail struct {
	ModifyPointsList []ModifyPoint `json:"modifyPointsList"`
}

// HackConfig 定义整体操作配置的数据结构
type HackConfig struct {
	HackAction HackActionType `json:"hackAction"`
	HackSource string         `json:"hackSource"`
	HackDetail HackDetail     `json:"hackDetail"`
}

var cacheDir = "./_cacheRaw"

func main() {
	// 配置 zerolog
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	log.Logger = log.Output(zerolog.ConsoleWriter{
		Out:        os.Stdout,
		TimeFormat: time.RFC3339,
	})

	/*
		处理配置文件
	*/
	// 读取配置文件
	configFile, err := os.ReadFile("config/config.json")
	if err != nil {
		log.Fatal().Msgf("unable to read config file: %v", err)
	}

	// 解析JSON配置文件，配置送入全局
	err = json.Unmarshal(configFile, &config)
	if err != nil {
		log.Fatal().Msgf("unable to parse config file: %v", err)
	}

	for _, origin := range config.AllowedOrigins {
		allowedOrigins.Store(origin, true)
	}
	// 添加 main_entry_list 中的项到 targetsMap
	for _, entry := range config.MainEntryList {
		targetsMap.Store(entry, mustParseURL(config.MainTargetURL))
		targetsTypeMap.Store(entry, "main")
	}

	// 添加 res_entry_list 中的项到 targetsMap
	for _, entry := range config.ResEntryList {
		targetsMap.Store(entry, mustParseURL(config.ResTargetURL))
		targetsTypeMap.Store(entry, "res")
	}

	/*
		初始化日志等可观测配件协程
	*/
	go logger(logChannel)

	/*
		路由注册与处理逻辑
	*/
	http.HandleFunc("/proxy-svc/api/v1/refresh-cache", func(w http.ResponseWriter, r *http.Request) {
		if !isDomainAllowedCallApi(r.Host, config) {
			mainProxyHandler(w, r)
			return
		}

		var req CacheRequest

		// 解析请求体
		err := json.NewDecoder(r.Body).Decode(&req)
		if err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		// 检查站点和缓存类型是否为空
		if req.Site == "" || req.CacheType == "" {
			http.Error(w, "Site and CacheType could not empty.", http.StatusBadRequest)
			return
		}

		log.Info().
			Str("site", req.Site).
			Str("cacheType", req.CacheType).
			Str("filePath", req.FilePath).
			Msg("try to refresh cache")

		// 拼接路径
		targetPath := filepath.Join(cacheDir, req.Site+".site")
		if req.FilePath != "" {
			targetPath = filepath.Join(targetPath, req.FilePath)
		}

		// 删除目录或文件
		var deleteErr error
		if req.FilePath == "" {
			deleteErr = os.RemoveAll(targetPath)
		} else {
			deleteErr = os.Remove(targetPath)
		}

		// 处理删除错误
		if deleteErr != nil {
			http.Error(w, "Failed to delete cache: "+deleteErr.Error(), http.StatusInternalServerError)
			return
		}

		// 返回成功响应
		w.WriteHeader(http.StatusNoContent)
	})

	http.HandleFunc("/proxy-svc/api/healthz", func(w http.ResponseWriter, r *http.Request) {
		// 这里可以检查应用的健康状态
		w.WriteHeader(http.StatusOK)
		_, err := w.Write([]byte("OK"))
		if err != nil {
			return
		}
	})

	http.HandleFunc("/proxy-svc/api/readyz", func(w http.ResponseWriter, r *http.Request) {
		// 这里可以检查应用是否准备好接受流量
		w.WriteHeader(http.StatusOK)
		_, err := w.Write([]byte("OK"))
		if err != nil {
			return
		}
	})

	http.HandleFunc("/config.ini", serveFileHandler("overwrite/config.ini"))
	http.HandleFunc("/breaking-news.html", serveFileHandler("overwrite/breaking-news.html"))
	//servers.ini由作者保持最新
	//http.HandleFunc("/servers.ini", serveFileHandler("overwrite/servers.ini"))
	http.HandleFunc("/lib/local-trans.js", serveFileHandler("overwrite/local-trans.js"))
	http.HandleFunc("/lib/nipplejs.js", serveFileHandler("overwrite/nipplejs.js"))
	http.HandleFunc("/res/locale/zh-CN.json", serveFileHandler("overwrite/zh-CN.json"))
	http.HandleFunc("/res/locale/zh-TW.json", serveFileHandler("overwrite/zh-CN.json"))
	http.HandleFunc("/robots.txt", serveFileHandler("overwrite/robots.txt"))
	http.HandleFunc("/res/mods.ini", serveFileHandler("overwrite/mods.ini"))

	http.HandleFunc("/", mainProxyHandler)

	/*
		服务启动
	*/
	log.Info().Msgf("Serving on :%d", config.Port)
	err = http.ListenAndServe(":"+strconv.Itoa(config.Port), nil)
	if err != nil {
		log.Fatal().Msg("ListenAndServe: " + err.Error())
	}
}

func mainProxyHandler(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	isGetRequest := r.Method == http.MethodGet
	isHtmlRequest := strings.Contains(r.Header.Get("Accept"), "text/html")
	host := strings.Split(r.Host, ":")[0]
	currentTargetURLValue, ok := targetsMap.Load(host)
	if !ok {
		http.Error(w, "HTTP CODE 403. Forbidden By Tencent EdgeOne……", http.StatusForbidden)
		return
	}
	currentTargetURL := currentTargetURLValue.(*url.URL)

	r.URL.Scheme = currentTargetURL.Scheme
	r.URL.Host = currentTargetURL.Host
	r.Header.Set("X-Forwarded-Host", r.Header.Get("Host"))
	r.Host = currentTargetURL.Host

	targetURLType, ok := targetsTypeMap.Load(host)
	if !ok {
		http.Error(w, "HTTP CODE 403. Can't Find URL Type. Forbidden By Tencent Edge One……", http.StatusForbidden)
		return
	}
	hostDir := targetURLType.(string) + ".site"

	// 代理缓存命中检查
	cachePath := filepath.Join(cacheDir, hostDir, r.URL.Path)
	// 只有GET请求才考虑缓存相关
	if isGetRequest {
		if isHtmlRequest && r.URL.Path == "/" {
			cachePath = filepath.Join(cacheDir, hostDir, "index.html")
		}
		if isHtmlRequest && filepath.Ext(r.URL.Path) == "" {
			cachePath = filepath.Join(cacheDir, hostDir, r.URL.Path, "index.html")
		}
		if fileExists(cachePath) {
			// 设置CORS头
			serveFileWithCORS(w, r)

			// 获取文件信息
			fileInfo, err := os.Stat(cachePath)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}

			modTime := fileInfo.ModTime().UTC()
			fileSize := fileInfo.Size()
			etag := fmt.Sprintf(`"%x-%x"`, modTime.Unix(), fileSize)

			// 设置Last-Modified和ETag头
			w.Header().Set("Last-Modified", modTime.Format(http.TimeFormat))
			w.Header().Set("ETag", etag)

			// 设置或覆盖Server头
			w.Header().Set("Server", "ra2web-proxy")

			// 检查If-None-Match和If-Modified-Since头
			ifNoneMatch := r.Header.Get("If-None-Match")
			ifModifiedSince := r.Header.Get("If-Modified-Since")

			// 比较ETag
			if ifNoneMatch == etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}

			// 比较Last-Modified
			if ifModifiedSince != "" {
				if t, err := time.Parse(http.TimeFormat, ifModifiedSince); err == nil {
					// 如果文件未被修改
					if modTime.Before(t.Add(1 * time.Second)) {
						w.WriteHeader(http.StatusNotModified)
						return
					}
				}
			}

			// 读取未压缩的缓存文件
			data, err := os.ReadFile(cachePath)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}

			// 根据文件的扩展名设置Content-Type
			ext := filepath.Ext(cachePath)
			mimeType := mime.TypeByExtension(ext)
			if mimeType != "" {
				w.Header().Set("Content-Type", mimeType)
			}

			// 检查客户端支持的压缩方法
			encodings := r.Header.Get("Accept-Encoding")

			if strings.Contains(encodings, "br") {
				// 使用Brotli压缩
				w.Header().Set("Content-Encoding", "br")
				bw := brotli.NewWriterLevel(w, brotli.BestCompression)
				defer func(bw *brotli.Writer) {
					err := bw.Close()
					if err != nil {
						return
					}
				}(bw)
				_, err := bw.Write(data)
				if err != nil {
					return
				}
			} else if strings.Contains(encodings, "gzip") {
				// 使用gzip压缩
				w.Header().Set("Content-Encoding", "gzip")
				gz := gzip.NewWriter(w)
				defer func(gz *gzip.Writer) {
					err := gz.Close()
					if err != nil {
						return
					}
				}(gz)
				_, err := gz.Write(data)
				if err != nil {
					return
				}
			} else {
				// 未压缩
				_, err := w.Write(data)
				if err != nil {
					return
				}
			}

			sendLog(LogMessage{
				ClientIP:   r.RemoteAddr,
				RequestURL: r.URL.String(),
				Method:     r.Method,
				UserAgent:  r.UserAgent(),
				StatusCode: http.StatusOK,
				Latency:    time.Since(start),
				CacheHit:   true,
				CachePath:  cachePath,
			})
			return
		}
	}

	// 创建反向代理
	proxy := httputil.NewSingleHostReverseProxy(currentTargetURL)
	proxy.ModifyResponse = func(response *http.Response) error {
		if isGetRequest {
			// 如果请求的是根路径，则将缓存路径设置为index.html
			if isHtmlRequest && r.URL.Path == "/" {
				cachePath = filepath.Join(cacheDir, hostDir, "index.html")
			}
			if isHtmlRequest && filepath.Ext(r.URL.Path) == "" {
				cachePath = filepath.Join(cacheDir, hostDir, r.URL.Path, "index.html")
			}
			// 只有2xx请求才考虑是否缓存，其他HTTP CODE不应该缓存处理
			if response.StatusCode >= 200 && response.StatusCode < 300 {
				// 判定是否应该缓存
				if shouldCache(response) {
					var reader io.ReadCloser = response.Body
					defer response.Body.Close()

					// 响应压缩算法
					switch response.Header.Get("Content-Encoding") {
					case "gzip":
						gzReader, err := gzip.NewReader(response.Body)
						if err != nil {
							return err
						}
						defer gzReader.Close()
						reader = gzReader
					case "deflate":
						reader = flate.NewReader(response.Body)
						defer reader.Close()
					case "br":
						reader = io.NopCloser(brotli.NewReader(response.Body))
						// brotli.Reader 不需要关闭
					}

					body, err := io.ReadAll(reader)
					if err != nil {
						return err
					}
					// 这里插入内容是临时的，用于修改index.html，此时对于原始数据的解压已经完成
					if r.URL.Path == "/" {
						body, err = modifyIndexHTML(body)
						if err != nil {
							return err
						}
					}

					if r.URL.Path == "/dist/workerHost.min.js" {
						body = modifyWorkerHostJS(body)
					}

					response.Body = io.NopCloser(bytes.NewReader(body))
					response.Header.Set("Content-Length", strconv.Itoa(len(body))) // 更新Content-Length头
					// 更新Content-Encoding头
					response.Header.Del("Content-Encoding")

					if err := writeCacheFile(cachePath, body); err != nil {
						return err
					}
				}
			} else {
				if response.StatusCode == http.StatusNotFound {
					filePath := "views/404page.html"

					// 读取错误页面文件
					content, err := os.ReadFile(filePath)
					if err != nil {
						log.Error().Err(err).Msg("Error reading 404 page")
						// 返回基本404错误码
						errorPage := []byte("Page 404")
						response.Body = io.NopCloser(bytes.NewReader(errorPage))
						response.Header.Set("Content-Length", strconv.Itoa(len(errorPage)))
						response.Header.Set("Content-Type", "text/html")
					}

					response.Body = io.NopCloser(bytes.NewReader(content))
					response.Header.Set("Content-Length", strconv.Itoa(len(content)))
					response.Header.Set("Content-Type", "text/html")
				} else {
					// 返回其他模式下的错误页面
					errorPage := []byte("Page Status Error")
					response.Body = io.NopCloser(bytes.NewReader(errorPage))
					response.Header.Set("Content-Length", strconv.Itoa(len(errorPage)))
					response.Header.Set("Content-Type", "text/html")
				}
			}
		}
		return nil
	}

	// 创建一个响应记录器来捕获响应状态码并根据不同的路由切换不同的代理
	responseRecorder := httptest.NewRecorder()
	proxy.ServeHTTP(responseRecorder, r)

	// 删除X-Frame-Options头以允许有站点
	responseRecorder.Header().Del("X-Frame-Options")

	// 设置或覆盖Server头
	responseRecorder.Header().Set("Server", "ra2web-proxy")

	// 统一跨域逻辑处理
	responseRecorder.Header().Del("Access-Control-Allow-Origin")
	responseRecorder.Header().Del("Access-Control-Allow-Methods")
	responseRecorder.Header().Del("Access-Control-Allow-Headers")
	origin := r.Header.Get("Origin")
	if origin == "" {
		referer := r.Header.Get("Referer")
		if referer != "" {
			origin = getOriginFromReferer(referer)
		}
	}

	if _, ok := allowedOrigins.Load(origin); ok {
		responseRecorder.Header().Set("Access-Control-Allow-Origin", "*")
	}
	responseRecorder.Header().Set("Access-Control-Allow-Methods", "*")
	responseRecorder.Header().Set("Access-Control-Allow-Headers", "*")

	// 只有2xx请求才考虑是否缓存，其他HTTP CODE不应该缓存处理
	if responseRecorder.Code >= 200 && responseRecorder.Code < 300 {
		// 只有正常情况，才将代理的响应写回到原始的响应写入器
		for k, vv := range responseRecorder.Header() {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
	}

	w.WriteHeader(responseRecorder.Code)
	_, err := w.Write(responseRecorder.Body.Bytes())
	if err != nil {
		return
	}

	sendLog(LogMessage{
		ClientIP:    r.RemoteAddr,
		RequestURL:  r.URL.String(),
		Method:      r.Method,
		UserAgent:   r.UserAgent(),
		StatusCode:  responseRecorder.Result().StatusCode,
		Latency:     time.Since(start),
		CacheHit:    false,
		UpstreamURL: currentTargetURL.String(),
		Error:       nil, // 如果有错误，设置相应的错误信息
	})
}

func mustParseURL(rawURL string) *url.URL {
	parsedUrl, err := url.Parse(rawURL)
	if err != nil {
		log.Fatal().Msgf("Failed to parse URL %q: %v", rawURL, err)
	}
	return parsedUrl
}

func logger(logChannel chan LogMessage) {
	for msg := range logChannel {
		event := log.Info()
		if msg.Error != nil {
			event = log.Error().Err(msg.Error)
		}

		// 构建基础日志字段
		event.Str("client_ip", msg.ClientIP).
			Str("method", msg.Method).
			Str("url", msg.RequestURL).
			Int("status", msg.StatusCode).
			Dur("latency", msg.Latency).
			Str("user_agent", msg.UserAgent)

		if msg.CacheHit {
			// 缓存命中的日志
			event.Bool("cache_hit", true).
				Str("cache_path", msg.CachePath).
				Msg("Cache Hit")
		} else {
			// 代理请求的日志
			event.Bool("cache_hit", false).
				Str("upstream_url", msg.UpstreamURL).
				Msg("Proxy Request")
		}
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return !os.IsNotExist(err)
}

func shouldCache(response *http.Response) bool {
	// 根据文件类型判定
	contentType := response.Header.Get("Content-Type")
	if strings.Contains(contentType, "text/html") ||
		strings.Contains(contentType, "text/css") ||
		strings.Contains(contentType, "application/javascript") ||
		strings.Contains(contentType, "application/octet-stream") ||
		strings.Contains(contentType, "image/png") ||
		strings.Contains(contentType, "image/svg+xml") ||
		strings.Contains(contentType, "video/mp4") {
		return true
	}

	// 除了对文件类型判定还对扩展名进行判定
	reqUrl := response.Request.URL
	path := reqUrl.Path
	return filepath.Ext(path) != ""
}

func serveFileWithCORS(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		referer := r.Header.Get("Referer")
		if referer != "" {
			origin = getOriginFromReferer(referer)
		}
	}
	if _, ok := allowedOrigins.Load(origin); ok {
		w.Header().Set("Access-Control-Allow-Origin", "*")
	}
	w.Header().Set("Access-Control-Allow-Methods", "*")
	w.Header().Set("Access-Control-Allow-Headers", "*")
}

func getOriginFromReferer(referer string) string {
	if referer == "" {
		return ""
	}

	parsedUrl, err := url.Parse(referer)
	if err != nil {
		fmt.Println("Error parsing URL:", err)
		return ""
	}

	origin := fmt.Sprintf("%s://%s", parsedUrl.Scheme, parsedUrl.Host)
	return origin
}

// serveFileHandler 是一个通用的处理函数
func serveFileHandler(filePath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			referer := r.Header.Get("Referer")
			if referer != "" {
				origin = getOriginFromReferer(referer)
			}
		}

		w.Header().Del("X-Frame-Options")

		// 跨域逻辑处理
		w.Header().Del("Access-Control-Allow-Origin")
		w.Header().Del("Access-Control-Allow-Methods")
		w.Header().Del("Access-Control-Allow-Headers")
		if _, ok := allowedOrigins.Load(origin); ok {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		}
		w.Header().Set("Access-Control-Allow-Methods", "*")
		w.Header().Set("Access-Control-Allow-Headers", "*")

		// 设置或覆盖Server头
		w.Header().Set("Server", "ra2web-proxy")

		// 设置Last-Modified和ETag头
		fileInfo, err := os.Stat(filePath)
		if err == nil && !fileInfo.IsDir() {
			modTime := fileInfo.ModTime().UTC()
			fileSize := fileInfo.Size()
			etag := fmt.Sprintf(`"%x-%x"`, modTime.Unix(), fileSize)

			w.Header().Set("Last-Modified", modTime.Format(http.TimeFormat))
			w.Header().Set("ETag", etag)

			// 检查If-None-Match和If-Modified-Since头
			ifNoneMatch := r.Header.Get("If-None-Match")
			ifModifiedSince := r.Header.Get("If-Modified-Since")

			// 比较ETag
			if ifNoneMatch == etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}

			// 比较Last-Modified
			if ifModifiedSince != "" {
				if t, err := time.Parse(http.TimeFormat, ifModifiedSince); err == nil {
					// 如果文件未被修改
					if modTime.Before(t.Add(1 * time.Second)) {
						w.WriteHeader(http.StatusNotModified)
						return
					}
				}
			}
		}

		http.ServeFile(w, r, filePath)
	}
}

func isDomainAllowedCallApi(host string, c Config) bool {
	for _, allowedHost := range c.ApiEndpoint {
		if host == allowedHost {
			return true
		}
	}
	return false
}

// 添加一个新的函数来处理缓存写入
func writeCacheFile(cachePath string, body []byte) error {
	_, err, _ := singleGroup.Do(cachePath, func() (interface{}, error) {
		// 确保目录存在
		if err := os.MkdirAll(filepath.Dir(cachePath), 0755); err != nil {
			return nil, fmt.Errorf("failed to create directory: %w", err)
		}

		// 创建临时文件
		tmpFile, err := os.CreateTemp(filepath.Dir(cachePath), "tmp-*")
		if err != nil {
			return nil, fmt.Errorf("failed to create temp file: %w", err)
		}
		tmpPath := tmpFile.Name()

		// 获取文件锁
		if err := utils.LockFile(tmpFile); err != nil {
			tmpFile.Close()
			os.Remove(tmpPath)
			return nil, fmt.Errorf("failed to lock file: %w", err)
		}

		// 确保函数返回前解锁和清理
		defer func() {
			utils.UnlockFile(tmpFile)
			tmpFile.Close()
			os.Remove(tmpPath)
		}()

		// 写入数据
		if _, err := tmpFile.Write(body); err != nil {
			return nil, fmt.Errorf("failed to write to temp file: %w", err)
		}

		// 强制同步到磁盘
		if err := tmpFile.Sync(); err != nil {
			return nil, fmt.Errorf("failed to sync temp file: %w", err)
		}

		// 关闭文件（保持锁定）
		if err := tmpFile.Close(); err != nil {
			return nil, fmt.Errorf("failed to close temp file: %w", err)
		}

		// 原子性地重命名文件
		if err := os.Rename(tmpPath, cachePath); err != nil {
			return nil, fmt.Errorf("failed to rename temp file: %w", err)
		}

		// 打开新文件并同步目录
		dir, err := os.Open(filepath.Dir(cachePath))
		if err == nil {
			dir.Sync() // 同步目录确保重命名操作持久化
			dir.Close()
		}

		return nil, nil
	})

	return err
}

// 添加辅助函数来处理 index.html 的修改
func modifyIndexHTML(body []byte) ([]byte, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	// 从配置中读取 base_href
	if config.BaseHref != "" {
		doc.Find("head").PrependHtml(fmt.Sprintf(`<base href="%s" />`, config.BaseHref))
	}

	doc.Find("head title").SetText("网页红井-联机对战平台")
	doc.Find(`meta[name="description"]`).Remove()
	doc.Find("head title").AfterHtml(`<script type="text/javascript" src="lib/nipplejs.js"></script><script type="text/javascript" src="lib/local-trans.js"></script>`)
	doc.Find(`script[src="https://www.googletagmanager.com/gtag/js?id=G-NT498QGSGZ"]`).Remove()
	doc.Find("head title").AfterHtml(`<meta name="description" content="在网页上就能玩经典的红色井界游戏，无需下载安装，随时随地在手机、电脑、平板甚至手表上畅玩。提供多种游戏模式和地图，与全球玩家实时对战。">`)
	doc.Find("head title").AfterHtml(`<meta name="keywords" content="红色警戒下载, 如何玩红警, webra2, 苹果如何玩红警, 平板上如何玩红警, 手机上如何玩红警, win7如何玩红警, win10如何玩红警, win11如何玩红警, 红警, 红警2, 红色警戒2, 网页红警, 云红警, 在线游戏, 游戏平台，对战平台，战网, 红色警戒3, 红警3, RA2, RA2WEB">`)

	html, err := doc.Html()
	if err != nil {
		return nil, err
	}
	return []byte(html), nil
}

// 添加辅助函数来处理 workerHost.min.js 的修改
func modifyWorkerHostJS(body []byte) []byte {
	bodyStr := string(body)
	bodyStr = strings.Replace(bodyStr, `(null===(r=null==t?void 0:t.CORSWorkaround)||void 0===r||r)`, `true`, 1)
	bodyStr = strings.Replace(bodyStr, `"string"==typeof e&&o(e)&&(null===(i=null==t?void 0:t.CORSWorkaround)||void 0===i||i)`, `true`, 1)
	return []byte(bodyStr)
}

// 添加辅助函数来发送日志
func sendLog(msg LogMessage) {
	select {
	case logChannel <- msg:
		// 成功发送
	default:
		// 通道已满，直接打印日志避免阻塞
		log.Warn().
			Str("client_ip", msg.ClientIP).
			Str("url", msg.RequestURL).
			Msg("Log channel full, message dropped")
	}
}
