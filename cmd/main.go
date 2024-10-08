package main

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"github.com/PuerkitoBio/goquery"
	"github.com/andybalholm/brotli"
	"io"
	"log"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	MainTargetURL  string   `json:"main_target_url"`
	MainEntryList  []string `json:"main_entry_list"`
	ResTargetURL   string   `json:"res_target_url"`
	ResEntryList   []string `json:"res_entry_list"`
	ApiEndpoint    []string `json:"api_endpoint"`
	AllowedOrigins []string `json:"allowed_origins"`
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
	ClientIP   string
	RequestURL string
	Method     string
	UserAgent  string
	StatusCode int
	Latency    time.Duration
}

// HackActionType 定义枚举值
type HackActionType string

var (
	targetsMap     = make(map[string]*url.URL)
	targetsTypeMap = make(map[string]string)
	logChannel     = make(chan LogMessage, 1000)
)

const (
	ModifyHTMLFile HackActionType = "modifyHTMLFile"
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
var hackCacheDir = "./_cacheAfterHacked"

var allowedOrigins = make(map[string]bool)

func main() {
	/*
		处理配置文件
	*/
	// 读取配置文件
	configFile, err := os.ReadFile("config/config.json")
	if err != nil {
		log.Fatalf("unable to read config file: %v", err)
	}

	// 解析JSON配置文件
	var config Config
	err = json.Unmarshal(configFile, &config)
	if err != nil {
		log.Fatalf("unable to parse config file: %v", err)
	}

	for _, origin := range config.AllowedOrigins {
		allowedOrigins[origin] = true
	}
	// 添加 main_entry_list 中的项到 targetsMap
	for _, entry := range config.MainEntryList {
		targetsMap[entry] = mustParseURL(config.MainTargetURL)
		targetsTypeMap[entry] = "main"
	}

	// 添加 res_entry_list 中的项到 targetsMap
	for _, entry := range config.ResEntryList {
		targetsMap[entry] = mustParseURL(config.ResTargetURL)
		targetsTypeMap[entry] = "res"
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

		log.Println("try to refresh cache", req.Site, req.CacheType, req.FilePath)

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

	http.HandleFunc("/", mainProxyHandler)

	/*
		服务启动
	*/
	log.Println("Serving on :" + strconv.Itoa(config.Port))
	err = http.ListenAndServe(":"+strconv.Itoa(config.Port), nil)
	if err != nil {
		log.Fatal("ListenAndServe: ", err)
	}
}

func mainProxyHandler(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	isGetRequest := r.Method == http.MethodGet
	isHtmlRequest := strings.Contains(r.Header.Get("Accept"), "text/html")
	host := strings.Split(r.Host, ":")[0] // 获取不带端口的主机名
	currentTargetURL, ok := targetsMap[host]
	if !ok {
		http.Error(w, "HTTP CODE 403. Forbidden By Tencent EdgeOne……", http.StatusForbidden)
		return
	}

	/*
	 *	处理请求部分
	 */
	r.URL.Scheme = currentTargetURL.Scheme
	r.URL.Host = currentTargetURL.Host
	r.Header.Set("X-Forwarded-Host", r.Header.Get("Host"))
	r.Host = currentTargetURL.Host

	targetURLType, ok := targetsTypeMap[host]
	if !ok {
		http.Error(w, "HTTP CODE 403. Can't Find URL Type. Forbidden By Tencent Edge One……", http.StatusForbidden)
		return
	}
	hostDir := targetURLType + ".site"

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
			serveFileWithCORS(w, r)
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
				bw := brotli.NewWriterLevel(w, brotli.DefaultCompression)
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
			return
		}
	}

	// 创建反向代理
	proxy := httputil.NewSingleHostReverseProxy(currentTargetURL)
	proxy.ModifyResponse = func(response *http.Response) error {
		if isGetRequest {
			// 如果请求的是根路径，则将缓存路径设置为index.html
			if isHtmlRequest && r.URL.Path == "/" {
				cachePath = filepath.Join(cacheDir, host, "index.html")
			}
			if isHtmlRequest && filepath.Ext(r.URL.Path) == "" {
				cachePath = filepath.Join(cacheDir, hostDir, r.URL.Path, "index.html")
			}
			// 只有2xx请求才考虑是否缓存，其他HTTP CODE不应该缓存处理
			if response.StatusCode >= 200 && response.StatusCode < 300 {
				// 判定是否应该缓存
				if shouldCache(response) {
					var reader io.Reader = response.Body
					var err error
					// 响应压缩算法
					switch response.Header.Get("Content-Encoding") {
					case "gzip":
						reader, err = gzip.NewReader(response.Body)
						if err != nil {
							return err
						}
					case "deflate":
						reader = flate.NewReader(response.Body)
					case "br":
						reader = brotli.NewReader(response.Body)
					}

					body, err := io.ReadAll(reader)
					if err != nil {
						return err
					}
					// 这里插入内容是临时的，用于修改index.html，此时对于原始数据的解压已经完成
					if r.URL.Path == "/" {
						doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
						if err != nil {
							http.Error(w, "Unable to parse HTML", http.StatusInternalServerError)
							return err
						}

						// 修改 HTML 内容
						//doc.Find("head").PrependHtml(`<base href="//wyhj.bun.sh.cn/" />`)
						doc.Find("head title").SetText("网页红井-联机对战平台")
						doc.Find(`meta[name="description"]`).Remove()

						doc.Find("head title").AfterHtml(`<script type="text/javascript" src="lib/nipplejs.js"></script><script type="text/javascript" src="lib/local-trans.js"></script>`)
						doc.Find(`script[src="https://www.googletagmanager.com/gtag/js?id=G-NT498QGSGZ"]`).Remove()
						doc.Find("head title").AfterHtml(`<meta name="description" content="在网页上就能玩经典的红色井界游戏，无需下载安装，随时随地在手机、电脑、平板甚至手表上畅玩。提供多种游戏模式和地图，与全球玩家实时对战。">`)
						doc.Find("head title").AfterHtml(`<meta name="keywords" content="红色警戒下载, 如何玩红警, webra2, 苹果如何玩红警, 平板上如何玩红警, 手机上如何玩红警, win7如何玩红警, win10如何玩红警, win11如何玩红警, 红警, 红警2, 红色警戒2, 网页红警, 云红警, 在线游戏, 游戏平台，对战平台，战网, 红色警戒3, 红警3, RA2, RA2WEB">`)

						modifiedBody, err := doc.Html()
						if err != nil {
							http.Error(w, "Unable to render modified HTML", http.StatusInternalServerError)
							return err
						}

						body = []byte(modifiedBody)
					}

					if r.URL.Path == "/dist/workerHost.min.js" {
						bodyStr := string(body)
						bodyStr = strings.Replace(bodyStr, `(null===(r=null==t?void 0:t.CORSWorkaround)||void 0===r||r)`, `true`, 1)
						bodyStr = strings.Replace(bodyStr, `"string"==typeof e&&o(e)&&(null===(i=null==t?void 0:t.CORSWorkaround)||void 0===i||i)`, `true`, 1)

						body = []byte(bodyStr)
					}

					response.Body = io.NopCloser(bytes.NewReader(body))
					response.Header.Set("Content-Length", strconv.Itoa(len(body))) // 更新Content-Length头
					// 更新Content-Encoding头
					response.Header.Del("Content-Encoding")

					isHostMatchMainSite := checkMainSiteHostMatch(cachePath, host)
					if isHostMatchMainSite {
						indexCachePath := strings.Replace(cachePath, host, "main.site", 1)
						// 如果匹配，那么cachePath中一定有host的字符串，替换掉第一个成为main.site，就是正常
						err = os.MkdirAll(filepath.Dir(indexCachePath), 0755)
						if err != nil {
							return err
						}

						err = os.WriteFile(indexCachePath, body, 0644)
						if err != nil {
							return err
						}
					} else {
						err = os.MkdirAll(filepath.Dir(cachePath), 0755)
						if err != nil {
							return err
						}

						err = os.WriteFile(cachePath, body, 0644)
						if err != nil {
							return err
						}
					}
				}
			} else {
				if response.StatusCode == http.StatusNotFound {
					filePath := "views/404page.html"

					// 读取错误页面文件
					content, err := os.ReadFile(filePath)
					if err != nil {
						log.Println(err)
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

	// 删除X-Frame-Options头以允许所有站点
	responseRecorder.Header().Del("X-Frame-Options")

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

	if allowedOrigins[origin] {
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
	/*
	 *	可观测性支持部分
	 */
	// 获取基本请求信息
	clientIP := r.RemoteAddr
	requestURL := r.URL.String()
	method := r.Method
	userAgent := r.UserAgent()
	statusCode := responseRecorder.Result().StatusCode
	latency := time.Since(start)
	// 将日志消息发送到日志通道
	logChannel <- LogMessage{
		ClientIP:   clientIP,
		RequestURL: requestURL,
		Method:     method,
		UserAgent:  userAgent,
		StatusCode: statusCode,
		Latency:    latency,
	}
}

func mustParseURL(rawURL string) *url.URL {
	parsedUrl, err := url.Parse(rawURL)
	if err != nil {
		log.Fatalf("Failed to parse URL %q: %v", rawURL, err)
	}
	return parsedUrl
}

func logger(logChannel chan LogMessage) {
	for msg := range logChannel {
		logEntry := LogEntry{
			ClientIP:   msg.ClientIP,
			RequestURL: msg.RequestURL,
			Method:     msg.Method,
			UserAgent:  msg.UserAgent,
			StatusCode: msg.StatusCode,
			Latency:    msg.Latency,
		}
		logEntryJSON, err := json.Marshal(logEntry)
		if err != nil {
			log.Printf("Failed to marshal log entry: %v", err)
		} else {
			log.Println(string(logEntryJSON))
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
	fileExt := filepath.Ext(path)

	if fileExt != "" {
		return true
	}

	return false
}

func serveFileWithCORS(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		referer := r.Header.Get("Referer")
		if referer != "" {
			origin = getOriginFromReferer(referer)
		}
	}
	if allowedOrigins[origin] {
		w.Header().Set("Access-Control-Allow-Origin", "*")
	}
	w.Header().Set("Access-Control-Allow-Methods", "*")
	w.Header().Set("Access-Control-Allow-Headers", "*")
	return
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

func checkMainSiteHostMatch(input string, host string) bool {
	// 按照 \ 截取第二节内容
	parts := strings.Split(input, "\\")
	if len(parts) < 2 {
		// 没有第二节内容，直接返回 false
		return false
	}

	// 检查第二节内容是否和 host 一致
	if parts[1] == host {
		return true
	}

	return false
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
		if allowedOrigins[origin] {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		}
		w.Header().Set("Access-Control-Allow-Methods", "*")
		w.Header().Set("Access-Control-Allow-Headers", "*")

		http.ServeFile(w, r, filePath)
	}
}

// modifyHTML 根据修改点列表修改 HTML 内容
func modifyHTML(data []byte, modifyPoints []ModifyPoint) ([]byte, error) {
	// 解析 HTML 内容
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(data)))
	if err != nil {
		return nil, fmt.Errorf("failed to parse HTML: %v", err)
	}

	for _, point := range modifyPoints {
		selection := doc.Find(point.Selector)
		if selection.Length() == 0 {
			return nil, fmt.Errorf("selector not found: %s", point.Selector)
		}

		switch point.Action {
		case Insert:
			if point.Position == "before" {
				selection.BeforeHtml(point.Content)
			} else if point.Position == "after" {
				selection.AfterHtml(point.Content)
			} else {
				return nil, fmt.Errorf("invalid position: %s", point.Position)
			}
		case Delete:
			selection.Remove()
		case Replace:
			selection.ReplaceWithHtml(point.Content)
		case ReplaceJS:
			selection.Each(func(i int, s *goquery.Selection) {
				if strings.Contains(s.Text(), point.OldContent) {
					newContent := strings.Replace(s.Text(), point.OldContent, point.NewContent, -1)
					s.SetText(newContent)
				}
			})
		default:
			return nil, fmt.Errorf("invalid action: %s", point.Action)
		}
	}

	html, err := doc.Html()
	if err != nil {
		return nil, fmt.Errorf("failed to render HTML: %v", err)
	}

	return []byte(html), nil
}

func isDomainAllowedCallApi(host string, c Config) bool {
	for _, allowedHost := range c.ApiEndpoint {
		if host == allowedHost {
			return true
		}
	}
	return false
}
