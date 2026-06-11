// 临时盘 - Go后端
// 代理百度网盘青春版API，将Cookie保存在服务端，不暴露给前端
package main

import (
	"crypto/md5"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/dhowden/tag"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------- 配置 ----------

// Config 服务配置
type Config struct {
	Port               int    `json:"port"`
	BaiduCookie        string `json:"baidu_cookie"`
	RateLimitPerSecond int    `json:"rate_limit_per_second"`
	BaiduAppID         string `json:"baidu_app_id"`
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}
	if cfg.Port == 0 {
		cfg.Port = 8080
	}
	if cfg.RateLimitPerSecond == 0 {
		cfg.RateLimitPerSecond = 10
	}
	if cfg.BaiduAppID == "" {
		cfg.BaiduAppID = "250528"
	}
	return &cfg, nil
}

// ---------- 频率限制 ----------

// RateLimiter 简单的令牌桶频率限制器（按IP）
type RateLimiter struct {
	mu       sync.Mutex
	clients  map[string]*clientState
	rate     int // 每秒允许的请求数
	interval time.Duration
}

type clientState struct {
	tokens   int
	lastSeen time.Time
}

func newRateLimiter(ratePerSecond int) *RateLimiter {
	rl := &RateLimiter{
		clients:  make(map[string]*clientState),
		rate:     ratePerSecond,
		interval: time.Second,
	}
	// 定期清理过期客户端记录
	go func() {
		for range time.Tick(time.Minute) {
			rl.mu.Lock()
			for ip, cs := range rl.clients {
				if time.Since(cs.lastSeen) > 5*time.Minute {
					delete(rl.clients, ip)
				}
			}
			rl.mu.Unlock()
		}
	}()
	return rl
}

// Allow 判断该IP是否被允许访问
func (rl *RateLimiter) Allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	cs, ok := rl.clients[ip]
	now := time.Now()
	if !ok {
		rl.clients[ip] = &clientState{tokens: rl.rate - 1, lastSeen: now}
		return true
	}

	// 按时间间隔补充令牌
	elapsed := now.Sub(cs.lastSeen)
	refill := int(elapsed / rl.interval)
	if refill > 0 {
		cs.tokens += refill * rl.rate
		if cs.tokens > rl.rate {
			cs.tokens = rl.rate
		}
		cs.lastSeen = now
	}

	if cs.tokens <= 0 {
		return false
	}
	cs.tokens--
	return true
}

// ---------- 路径校验 ----------

// validPathRe 只允许以/开头的合法路径，防止路径穿越
var validPathRe = regexp.MustCompile(`^/[^\x00-\x1f]*$`)

func isValidBaiduPath(p string) bool {
	if !validPathRe.MatchString(p) {
		return false
	}
	// 清理后确保没有 .. 穿越
	cleaned := path.Clean(p)
	return !strings.Contains(cleaned, "..") && strings.HasPrefix(cleaned, "/")
}

// fileMeta 文件元数据，包含下载签名计算所需的 id 和 md5
type fileMeta struct {
	FsID int64  `json:"fs_id"`
	MD5  string `json:"md5"`
}

// fileListCache 缓存文件列表中每个文件的元数据，按路径索引
type fileListCache struct {
	mu          sync.RWMutex
	filesByPath map[string]fileMeta // 文件路径 -> 元数据
	updatedAt   time.Time
}

// audioMetaCache 音频元数据缓存
type audioMetaCache struct {
	mu    sync.RWMutex
	cache map[string]*audioMeta
}

type audioMeta struct {
	Title  string `json:"title"`
	Artist string `json:"artist"`
	Album  string `json:"album"`
	Cover  string `json:"cover"` // base64
	Lyrics string `json:"lyrics"`
	cached time.Time
}

func newAudioMetaCache() *audioMetaCache {
	return &audioMetaCache{cache: make(map[string]*audioMeta)}
}

func (c *audioMetaCache) get(path string) (*audioMeta, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	meta, ok := c.cache[path]
	if ok && time.Since(meta.cached) < 10*time.Minute {
		return meta, true
	}
	return nil, false
}

func (c *audioMetaCache) set(path string, meta *audioMeta) {
	c.mu.Lock()
	defer c.mu.Unlock()
	meta.cached = time.Now()
	c.cache[path] = meta
}

func newFileListCache() *fileListCache {
	return &fileListCache{filesByPath: make(map[string]fileMeta)}
}

// update 解析 Baidu 列表响应并更新索引
func (c *fileListCache) update(listJSON []byte) {
	var resp struct {
		Errno int `json:"errno"`
		List  []struct {
			Path string `json:"path"`
			FsID int64  `json:"fs_id"`
			MD5  string `json:"md5"`
		} `json:"list"`
	}
	if err := json.Unmarshal(listJSON, &resp); err != nil {
		log.Printf("[WARN] 解析文件列表失败: %v", err)
		return
	}
	if resp.Errno != 0 {
		// 截取前 200 字节便于调试，不记录完整响应（可能包含 Cookie 等敏感信息）
		preview := listJSON
		if len(preview) > 200 {
			preview = preview[:200]
		}
		log.Printf("[WARN] 百度API返回错误 errno=%d, 响应头: %s", resp.Errno, preview)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.filesByPath == nil {
		c.filesByPath = make(map[string]fileMeta)
	}
	for _, f := range resp.List {
		if f.Path != "" && f.FsID != 0 {
			c.filesByPath[f.Path] = fileMeta{
				FsID: f.FsID,
				MD5:  f.MD5,
			}
		}
	}
	c.updatedAt = time.Now()
	log.Printf("[缓存] 更新完成：共 %d 个文件，当前总共已缓存 %d 个元数据", len(resp.List), len(c.filesByPath))
}

// getFileMeta 根据路径获取文件元数据
func (c *fileListCache) getFileMeta(path string) (fileMeta, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	meta, ok := c.filesByPath[path]
	return meta, ok
}

// ---------- 服务器 ----------

// Server 应用服务器
type Server struct {
	cfg        *Config
	limiter    *RateLimiter
	mux        *http.ServeMux
	cache      *fileListCache // 文件列表缓存，dlink 存在服务端，不暴露给前端
	audioCache *audioMetaCache
	uk         int64      // 百度用户uk
	sk         string     // 百度授权sk
	sessionMu  sync.Mutex // 保护 uk/sk 刷新
}

func newServer(cfg *Config) *Server {
	s := &Server{
		cfg:        cfg,
		limiter:    newRateLimiter(cfg.RateLimitPerSecond),
		mux:        http.NewServeMux(),
		cache:      newFileListCache(),
		audioCache: newAudioMetaCache(),
	}
	s.mux.HandleFunc("/api/files", s.withSecurity(s.handleFiles))
	s.mux.HandleFunc("/api/download", s.withSecurity(s.handleDownload))
	s.mux.HandleFunc("/api/audiometa", s.withSecurity(s.handleAudioMeta))
	// 使用 FileServer 提供 static/ 目录下的所有静态资源（index.html, app.js 等）
	staticFS := http.FileServer(http.Dir("static"))
	s.mux.HandleFunc("/", s.withSecurity(func(w http.ResponseWriter, r *http.Request) {
		// 只允许访问 / 和已知静态文件，防止目录枚举
		allowed := map[string]bool{"/": true, "/index.html": true, "/app.js": true, "/aplayer/aplayer.min.js": true, "/aplayer/aplayer.min.css": true}
		if !allowed[r.URL.Path] {
			http.NotFound(w, r)
			return
		}
		staticFS.ServeHTTP(w, r)
	}))
	return s
}

// withSecurity 统一安全中间件：添加安全响应头 + 频率限制
func (s *Server) withSecurity(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 只允许 GET 方法（减少攻击面）
		if r.Method != http.MethodGet {
			http.Error(w, "方法不允许", http.StatusMethodNotAllowed)
			return
		}

		// 设置安全响应头
		// TODO(security): CSP nonce 未实现，当前使用严格的 default-src 'self'
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: https://*.baidupcs.com; media-src https://*.baidupcs.com; frame-ancestors 'none'; object-src 'none'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Cache-Control", "no-store")

		// TODO(security): 当前仅限本地访问，如需对外开放需添加认证（OAuth/JWT）
		// 频率限制：取真实IP
		ip := r.RemoteAddr
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// 只取第一个，防止伪造
			ip = strings.SplitN(xff, ",", 2)[0]
		}
		ip = strings.TrimSpace(ip)

		isStatic := r.URL.Path == "/" || r.URL.Path == "/index.html" || r.URL.Path == "/app.js"
		if !isStatic {
			if !s.limiter.Allow(ip) {
				http.Error(w, "请求过于频繁，请稍后重试", http.StatusTooManyRequests)
				return
			}
		}

		next(w, r)
	}
}

// ---------- API处理 ----------

// fetchFileList 拉取百度网盘文件列表（dlink=1 返回预签名下载链接）
func (s *Server) fetchFileList(dir string) ([]byte, error) {
	if dir == "" {
		dir = "/"
	}
	apiURL := fmt.Sprintf(
		"https://pan.baidu.com/youth/api/list?clienttype=0&app_id=%s&web=1&order=time&desc=1&num=100&page=1&dlink=1&dir=%s",
		url.QueryEscape(s.cfg.BaiduAppID),
		url.QueryEscape(dir),
	)
	return s.baiduGet(apiURL, "")
}

// handleFiles 获取文件列表，代理百度网盘 list API
func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request) {
	dir := r.URL.Query().Get("dir")
	if dir == "" {
		dir = "/"
	}
	if !isValidBaiduPath(dir) {
		log.Printf("[WARN] 获取文件列表的非法路径被拒绝: %q", dir)
		http.Error(w, "非法路径", http.StatusBadRequest)
		return
	}

	body, err := s.fetchFileList(dir)
	if err != nil {
		log.Printf("[ERROR] 获取文件列表失败: %v", err)
		http.Error(w, "获取文件列表失败", http.StatusBadGateway)
		return
	}

	// 异步更新缓存（不阻塞响应）
	go s.cache.update(body)

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Write(body) //nolint:errcheck
}

// fetchUserSession 从百度接口获取 uk 与 sk
func (s *Server) fetchUserSession() (int64, string, error) {
	// 1. 尝试从 /youth/api/user/getinfo 获取 uk/sk
	apiURL := fmt.Sprintf(
		"https://pan.baidu.com/youth/api/user/getinfo?app_id=%s&clienttype=0&web=1&need_selfinfo=1",
		url.QueryEscape(s.cfg.BaiduAppID),
	)
	body, err := s.baiduGet(apiURL, "")
	var uk int64
	var sk string
	if err == nil {
		var resp struct {
			Errno   int `json:"errno"`
			Records []struct {
				Uk int64  `json:"uk"`
				Sk string `json:"sk"`
			} `json:"records"`
		}
		if json.Unmarshal(body, &resp) == nil && len(resp.Records) > 0 {
			uk = resp.Records[0].Uk
			sk = resp.Records[0].Sk
		}
	}

	// 2. 如果不完整，从 /api/gettemplatevariable 获取
	if uk == 0 || sk == "" {
		fallbackURL := `https://pan.baidu.com/api/gettemplatevariable?fields=["bdstoken","uk","sk"]`
		body2, err2 := s.baiduGet(fallbackURL, "")
		if err2 == nil {
			var resp2 struct {
				Result struct {
					Uk int64  `json:"uk"`
					Sk string `json:"sk"`
				} `json:"result"`
			}
			if json.Unmarshal(body2, &resp2) == nil {
				if uk == 0 {
					uk = resp2.Result.Uk
				}
				if sk == "" {
					sk = resp2.Result.Sk
				}
			}
		}
	}

	// 3. 如果还是没有 sk，从 /youth/api/report/user 获取
	if sk == "" && uk != 0 {
		skURL := fmt.Sprintf(
			"https://pan.baidu.com/youth/api/report/user?app_id=%s&clienttype=0&web=1&action=sapi_auth&timestamp=%d",
			url.QueryEscape(s.cfg.BaiduAppID),
			time.Now().UnixMilli(),
		)
		bodySK, errSK := s.baiduGet(skURL, "")
		if errSK == nil {
			var respSK struct {
				Uinfo string `json:"uinfo"`
			}
			if json.Unmarshal(bodySK, &respSK) == nil && respSK.Uinfo != "" {
				sk = respSK.Uinfo
			}
		}
	}

	if uk == 0 || sk == "" {
		return 0, "", fmt.Errorf("无法从百度网盘获取完整的 uk (%d) 或 sk (%q)", uk, sk)
	}

	return uk, sk, nil
}

// getSession 线程安全地获取或刷新 uk/sk
func (s *Server) getSession() (int64, string, error) {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	if s.uk != 0 && s.sk != "" {
		return s.uk, s.sk, nil
	}
	uk, sk, err := s.fetchUserSession()
	if err != nil {
		return 0, "", err
	}
	s.uk = uk
	s.sk = sk
	log.Printf("[Session] 获取成功, uk: %d, sk: %s", uk, sk)
	return uk, sk, nil
}

// locatedownloadRand 使用 SHA-1 计算位于下载的 rand 参数
func locatedownloadRand(uk int64, sk string, nowMilli int64) string {
	data := fmt.Sprintf("%d%s%d0", uk, sk, nowMilli)
	h := sha1.New()
	h.Write([]byte(data))
	return hex.EncodeToString(h.Sum(nil))
}

// locatedownloadSign 使用 MD5 计算位于下载的 sign 参数
func locatedownloadSign(fileMD5 string, fileID string, uk int64, nowMilli int64) string {
	data := fmt.Sprintf("%s_%d_%s_%d", fileMD5, uk, fileID, nowMilli)
	h := md5.New()
	h.Write([]byte(data))
	return hex.EncodeToString(h.Sum(nil))
}

// getBaiduDLink 计算签名并向百度 locatedownload 接口获取直链（包含 sk 过期自动重试）
func (s *Server) getBaiduDLink(filePath string, ua string) (string, error) {
	// 1. 获取文件元数据
	meta, ok := s.cache.getFileMeta(filePath)
	if !ok {
		parentDir := path.Dir(filePath)
		log.Printf("[缓存] 未命中 %q，重新拉取父目录 %q 的文件列表", filePath, parentDir)
		listBody, err := s.fetchFileList(parentDir)
		if err != nil {
			return "", fmt.Errorf("重新拉取文件列表失败: %w", err)
		}
		s.cache.update(listBody)
		meta, ok = s.cache.getFileMeta(filePath)
	}

	if !ok {
		// 调试：列出当前缓存中所有路径
		s.cache.mu.RLock()
		for p := range s.cache.filesByPath {
			log.Printf("[调试] 缓存中有路径: %q", p)
		}
		s.cache.mu.RUnlock()
		return "", fmt.Errorf("在缓存中找不到路径: %s", filePath)
	}

	// 2. 获取或刷新 uk/sk
	uk, sk, err := s.getSession()
	if err != nil {
		return "", fmt.Errorf("获取百度Session失败: %w", err)
	}

	// 3. 计算签名并调用 locatedownload
	nowMilli := time.Now().UnixMilli()
	randVal := locatedownloadRand(uk, sk, nowMilli)
	signVal := locatedownloadSign(meta.MD5, strconv.FormatInt(meta.FsID, 10), uk, nowMilli)
	dpLogID := strconv.FormatInt(time.Now().UnixNano(), 10)

	locateURL := fmt.Sprintf(
		"https://pan.baidu.com/youth/api/locatedownload?app_id=%s&clienttype=0&web=1&devuid=0&dp-logid=%s&path=%s&rand=%s&sign=%s&time=%d",
		url.QueryEscape(s.cfg.BaiduAppID),
		dpLogID,
		url.QueryEscape(filePath),
		randVal,
		signVal,
		nowMilli,
	)

	body, err := s.baiduGet(locateURL, ua)
	if err != nil {
		log.Printf("[WARN] 首次 locatedownload 失败: %v，尝试清除sk并重试", err)
		s.sessionMu.Lock()
		s.sk = ""
		s.sessionMu.Unlock()

		uk, sk, err = s.getSession()
		if err != nil {
			return "", fmt.Errorf("重新获取Session失败: %w", err)
		}

		nowMilli = time.Now().UnixMilli()
		randVal = locatedownloadRand(uk, sk, nowMilli)
		signVal = locatedownloadSign(meta.MD5, strconv.FormatInt(meta.FsID, 10), uk, nowMilli)
		dpLogID = strconv.FormatInt(time.Now().UnixNano(), 10)

		locateURL = fmt.Sprintf(
			"https://pan.baidu.com/youth/api/locatedownload?app_id=%s&clienttype=0&web=1&devuid=0&dp-logid=%s&path=%s&rand=%s&sign=%s&time=%d",
			url.QueryEscape(s.cfg.BaiduAppID),
			dpLogID,
			url.QueryEscape(filePath),
			randVal,
			signVal,
			nowMilli,
		)
		body, err = s.baiduGet(locateURL, ua)
		if err != nil {
			return "", fmt.Errorf("重试 locatedownload 失败: %w", err)
		}
	}

	var respLocate struct {
		Errno   int    `json:"errno"`
		ShowMsg string `json:"show_msg"`
		URL     string `json:"url"`
	}
	if err := json.Unmarshal(body, &respLocate); err != nil {
		return "", fmt.Errorf("解析 locatedownload 响应失败: %w", err)
	}

	if respLocate.Errno != 0 || respLocate.URL == "" {
		return "", fmt.Errorf("百度 locatedownload 返回错误 errno=%d, msg=%q", respLocate.Errno, respLocate.ShowMsg)
	}

	dlink := respLocate.URL
	if !strings.Contains(dlink, "response-cache-control=") {
		sep := "&"
		if !strings.Contains(dlink, "?") {
			sep = "?"
		}
		dlink += sep + "response-cache-control=private"
	}

	return dlink, nil
}

// handleDownload 获取百度网盘文件直链并返回
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	filePath := r.URL.Query().Get("path")
	if filePath == "" {
		http.Error(w, "缺少 path 参数", http.StatusBadRequest)
		return
	}

	if !isValidBaiduPath(filePath) {
		log.Printf("[WARN] 非法路径请求被拒绝: %q", filePath)
		http.Error(w, "非法路径", http.StatusBadRequest)
		return
	}

	// 1. 调用公共方法获取百度直链
	dlink, err := s.getBaiduDLink(filePath, r.Header.Get("User-Agent"))
	if err != nil {
		log.Printf("[ERROR] 获取百度直链失败: %v", err)
		http.Error(w, "获取直链失败", http.StatusBadGateway)
		return
	}

	format := r.URL.Query().Get("format")
	if format == "json" {
		// 返回 JSON 格式直链供前端处理
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		respJSON := fmt.Sprintf(`{"urls":[{"url":%q}]}`, dlink)
		w.Write([]byte(respJSON)) //nolint:errcheck
	} else {
		// 浏览器直接请求时，直接重定向到真实的直链以触发直接下载
		http.Redirect(w, r, dlink, http.StatusFound)
	}
}

// handleAudioMeta 获取音频文件元数据
func (s *Server) handleAudioMeta(w http.ResponseWriter, r *http.Request) {
	filePath := r.URL.Query().Get("path")
	if filePath == "" || !isValidBaiduPath(filePath) {
		http.Error(w, "非法路径", http.StatusBadRequest)
		return
	}

	// 检查缓存
	if cached, ok := s.audioCache.get(filePath); ok {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(cached)
		return
	}

	// 获取直链
	dlink, err := s.getBaiduDLink(filePath, r.Header.Get("User-Agent"))
	if err != nil {
		log.Printf("[ERROR] 获取音频直链失败: %v", err)
		http.Error(w, "获取直链失败", http.StatusBadGateway)
		return
	}

	// 下载前 512KB 读取元数据
	req, err := http.NewRequest("GET", dlink, nil)
	if err != nil {
		http.Error(w, "创建请求失败", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Range", "bytes=0-524287")
	req.Header.Set("User-Agent", r.Header.Get("User-Agent"))
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, "下载音频失败", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// 读取到内存
	data, err := io.ReadAll(io.LimitReader(resp.Body, 524288))
	if err != nil {
		http.Error(w, "读取音频失败", http.StatusBadGateway)
		return
	}

	meta := &audioMeta{}
	m, err := tag.ReadFrom(strings.NewReader(string(data)))
	if err == nil {
		meta.Title = m.Title()
		meta.Artist = m.Artist()
		meta.Album = m.Album()
		if pic := m.Picture(); pic != nil {
			meta.Cover = "data:" + pic.MIMEType + ";base64," + base64.StdEncoding.EncodeToString(pic.Data)
		}
		if lyrics := m.Lyrics(); lyrics != "" {
			meta.Lyrics = lyrics
		}
	}

	s.audioCache.set(filePath, meta)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(meta); err != nil {
		log.Printf("[ERROR] JSON编码失败: %v", err)
	}
}

// ---------- 百度网盘请求 ----------

// baiduGet 向百度网盘API发起GET请求，Cookie由服务端注入
func (s *Server) baiduGet(apiURL string, ua string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}

	// Cookie只在服务端注入，绝不返回给客户端
	req.Header.Set("Cookie", s.cfg.BaiduCookie)
	if ua == "" {
		ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36"
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Referer", "https://pan.baidu.com/")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求百度网盘API失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("百度网盘API返回非200状态: %d", resp.StatusCode)
	}

	// 限制读取大小，防止响应过大
	body, err := io.ReadAll(io.LimitReader(resp.Body, 5*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}
	return body, nil
}

// ---------- 主函数 ----------

func main() {
	cfg, err := loadConfig("config.json")
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	if cfg.BaiduCookie == "" {
		log.Fatal("配置文件中 baidu_cookie 不能为空")
	}

	srv := newServer(cfg)

	addr := fmt.Sprintf(":%d", cfg.Port)
	log.Printf("临时盘启动，访问地址: http://localhost%s", addr)
	log.Fatal(http.ListenAndServe(addr, srv.mux))
}
