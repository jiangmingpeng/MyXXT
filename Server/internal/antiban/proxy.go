package antiban

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// allowedProxySchemes 是 net/http 原生支持的代理类型。
// socks5/socks5h 由标准库的 transport 内置处理，不需要额外依赖。
var allowedProxySchemes = map[string]struct{}{
	"http":    {},
	"https":   {},
	"socks5":  {},
	"socks5h": {},
}

// NormalizeProxy 校验并规范化代理地址。
// 允许省略 scheme（默认按 http 处理），方便直接粘贴 host:port。
func NormalizeProxy(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid proxy %q: %w", raw, err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("invalid proxy %q: missing host", raw)
	}
	if _, ok := allowedProxySchemes[strings.ToLower(u.Scheme)]; !ok {
		return "", fmt.Errorf("unsupported proxy scheme %q (want http/https/socks5)", u.Scheme)
	}
	return u.String(), nil
}

// NewTransport 构造一个走指定代理的 Transport。
//
// 注意：每个代理都必须是独立的 Transport 实例，因为 Transport 内部会按
// "代理 + 目标 host" 维护连接池；共用实例会让不同出口复用同一条 TCP 连接，
// 等于把代理隔离又打回成单 IP。
func NewTransport(proxyURL string, insecureTLS bool) (*http.Transport, error) {
	proxyURL, err := NormalizeProxy(proxyURL)
	if err != nil {
		return nil, err
	}

	tr := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 8,
		// 签到是"隔几分钟才发生一次"的稀疏流量，60s 的空闲回收会让每次签到都
		// 重新付一次 TCP + TLS 握手的钱；慢代理上单次握手就要 2~4s，
		// 而一次二维码签到要连 mobilelearn / captcha 两个域名。
		// 这里把空闲连接留久一点，等于让代理隧道保持温暖（空闲连接几乎不耗流量）。
		IdleConnTimeout:       5 * time.Minute,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     false,
	}
	if insecureTLS {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	if proxyURL != "" {
		u, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("parse proxy failed: %w", err)
		}
		tr.Proxy = http.ProxyURL(u)
	}
	return tr, nil
}

// ExitIPResolver 负责"查一次代理的真实出口 IP 并缓存"。
//
// 之所以需要它：clientip 参数在真机上是手机自己的公网 IP，
// 直连场景下为空尚可接受，但一旦走代理却仍然留空，
// 就会出现"IP 归属地跟请求来源不一致"的矛盾信号。
//
// 探测失败不会影响签到主流程（返回空串），只是退回旧行为。
type ExitIPResolver struct {
	probeURL string
	ttl      time.Duration
	client   *http.Client

	mu    sync.Mutex
	cache map[string]exitIPEntry
}

type exitIPEntry struct {
	ip      string
	expires time.Time
}

// NewExitIPResolver 构造解析器。probeURL 为空时使用默认的 ifconfig.me 明文接口。
func NewExitIPResolver(probeURL string, insecureTLS bool) *ExitIPResolver {
	if strings.TrimSpace(probeURL) == "" {
		// 刻意不用 api.ipify.org：国内部分线路和代理节点会直接断连，
		// ifconfig.me 返回裸 IP，实测经代理可达。探测失败本身不影响主流程。
		probeURL = "https://ifconfig.me/ip"
	}
	tr, err := NewTransport("", insecureTLS)
	if err != nil {
		tr = &http.Transport{}
	}
	return &ExitIPResolver{
		probeURL: probeURL,
		ttl:      30 * time.Minute,
		client:   &http.Client{Timeout: 5 * time.Second, Transport: tr},
		cache:    make(map[string]exitIPEntry),
	}
}

// Resolve 返回该代理的出口 IP；空代理返回空串。结果缓存 30 分钟。
func (r *ExitIPResolver) Resolve(proxyURL string) string {
	if r == nil || strings.TrimSpace(proxyURL) == "" {
		return ""
	}
	proxyURL, err := NormalizeProxy(proxyURL)
	if err != nil {
		return ""
	}

	r.mu.Lock()
	if e, ok := r.cache[proxyURL]; ok && time.Now().Before(e.expires) {
		r.mu.Unlock()
		return e.ip
	}
	r.mu.Unlock()

	ip := r.probe(proxyURL)
	r.mu.Lock()
	r.cache[proxyURL] = exitIPEntry{ip: ip, expires: time.Now().Add(r.ttl)}
	r.mu.Unlock()
	return ip
}

func (r *ExitIPResolver) probe(proxyURL string) string {
	tr, err := NewTransport(proxyURL, false)
	if err != nil {
		return ""
	}
	cli := &http.Client{Timeout: 5 * time.Second, Transport: tr}
	req, err := http.NewRequest(http.MethodGet, r.probeURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", "curl/8.4.0")
	resp, err := cli.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ""
	}
	buf := make([]byte, 64)
	n, _ := resp.Body.Read(buf)
	ip := strings.TrimSpace(string(buf[:n]))
	if net.ParseIP(ip) == nil {
		return ""
	}
	return ip
}
