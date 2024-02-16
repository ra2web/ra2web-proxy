package main

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"github.com/andybalholm/brotli"
	"io"
	"log"
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

type LogMessage struct {
	ClientIP   string
	RequestURL string
	Method     string
	UserAgent  string
	StatusCode int
	Latency    time.Duration
}

var cacheDir = "./_cacheRaw"
var hackCacheDir = "./_cacheHacked"

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

	targetsMap := make(map[string]*url.URL)
	targetsTypeMap := make(map[string]string)

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
	logChannel := make(chan LogMessage, 1000)
	go logger(logChannel)

	/*
		路由注册与处理逻辑
	*/
	http.HandleFunc("/proxy-svc/healthz", func(w http.ResponseWriter, r *http.Request) {
		// 这里可以检查应用的健康状态
		w.WriteHeader(http.StatusOK)
		_, err := w.Write([]byte("OK"))
		if err != nil {
			return
		}
	})

	http.HandleFunc("/proxy-svc/readyz", func(w http.ResponseWriter, r *http.Request) {
		// 这里可以检查应用是否准备好接受流量
		w.WriteHeader(http.StatusOK)
		_, err := w.Write([]byte("OK"))
		if err != nil {
			return
		}
	})

	http.HandleFunc("/config.ini", func(w http.ResponseWriter, r *http.Request) {
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

		http.ServeFile(w, r, "overwrite/config.ini")
	})

	http.HandleFunc("/breaking-news.html", func(w http.ResponseWriter, r *http.Request) {
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

		http.ServeFile(w, r, "overwrite/breaking-news.html")
	})

	http.HandleFunc("/servers.ini", func(w http.ResponseWriter, r *http.Request) {
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

		http.ServeFile(w, r, "overwrite/servers.ini")
	})

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
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
		if r.URL.Path == "/" {
			cachePath = filepath.Join(cacheDir, hostDir, "index.html")
		}
		if fileExists(cachePath) {
			serveFileWithCORS(w, r)
			// 读取未压缩的缓存文件
			data, err := os.ReadFile(cachePath)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
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

		// 创建反向代理
		proxy := httputil.NewSingleHostReverseProxy(currentTargetURL)
		proxy.ModifyResponse = func(response *http.Response) error {
			// 如果请求的是根路径，则将缓存路径设置为index.html
			if r.URL.Path == "/" {
				cachePath = filepath.Join(cacheDir, host, "index.html")
			}
			// 只有2xx请求才考虑是否缓存，其他HTTP CODE不应该缓存处理
			if response.StatusCode >= 200 && response.StatusCode < 300 {
				// 判定是否应该缓存
				if shouldCache(response) {
					var reader io.Reader = response.Body
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
	})

	/*
		服务启动
	*/
	log.Println("Serving on :" + strconv.Itoa(config.Port))
	err = http.ListenAndServe(":"+strconv.Itoa(config.Port), nil)
	if err != nil {
		log.Fatal("ListenAndServe: ", err)
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
