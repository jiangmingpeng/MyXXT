package xxt

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/textproto"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"xbt2/server/internal/antiban"
)

const (
	SignNormal   = 0
	SignQRCode   = 2
	SignGesture  = 3
	SignLocation = 4
	SignCode     = 5
	captchaID    = "42sxgHoTPTKbt0uZxPJ7ssOvtXr3ZgZ1"
	captchaType  = "slide"
)

// Options 是构造 Client 的完整参数。
type Options struct {
	AESKey            string
	FallbackUserAgent string
	InsecureTLS       bool
	ActiveFetchMax    int

	// Guard 为反风控引擎；为 nil 时退化成"直连 + 不节流"的历史行为。
	Guard antiban.Guard
	// UnifyLoginUA 让登录请求也使用移动端 UA。
	UnifyLoginUA bool
	// ClientIPMode 控制 clientip 参数：empty / auto / fixed。
	ClientIPMode  string
	FixedClientIP string
	// ExitIPProbeURL 用于 auto 模式探测代理出口 IP。
	ExitIPProbeURL string
	// LogPayloads 控制是否打印 enc / 完整 query 等敏感内容。
	LogPayloads bool
}

type Client struct {
	aesKey         string
	mobileUA       string
	activeFetchMax int
	http           *http.Client
	sessionMu      sync.Mutex
	sessions       map[string]*Session

	guard  antiban.Guard
	exitIP *antiban.ExitIPResolver
	opts   Options

	// 每个代理都必须有独立的 Transport：共用会让不同出口复用同一条 TCP 连接，
	// 等于把出口隔离打回成单 IP。
	clientMu  sync.Mutex
	httpByKey map[string]*http.Client

	// 验证码预取单飞：同一活动 + 账号的并发抓取只会真正执行一次，
	// 避免"预取失败后 Sign 再同步抓一遍"让单次签到耗时翻倍。
	captchaMu      sync.Mutex
	captchaFlights map[string]*captchaFlight
}

type Session struct {
	Mobile      string
	Password    string
	UID         int64
	Name        string
	Avatar      string
	Fid         int64  // 学校（单位）id，来自登录后的 _fid cookie；二维码签到必须携带真实值。
	DeviceCode  string // 设备标识，账号级稳定，跨登录不变。
	UserAgent   string // 账号专属 UA，与 DeviceCode 一起构成稳定设备指纹。
	ProxyURL    string // 该账号固定使用的出口代理，空表示直连。
	Jar         *cookiejar.Jar
	LastLoginAt time.Time
}

type LoginResult struct {
	UID    int64
	Name   string
	Avatar string
}

type Course struct {
	Teacher  string
	Name     string
	CourseID int64
	ClassID  int64
	Icon     string
}

type Active struct {
	ActiveID int64
	Name     string
	// 下面几项是 taskactivelist 里可能自带的定位信息（二维码/位置签到通常会带）。
	// 有值时可以直接用，省掉一次 getPPTActiveInfo 请求；没有时为零值。
	IfOpenAddress     bool
	LocationLatitude  float64
	LocationLongitude float64
	LocationRange     int64
	LocationAddress   string
	LocationFetched   bool
}

// LocationDetail 把列表里自带的定位信息包成 SignDetail，方便和缓存/详情合并。
func (a Active) LocationDetail() SignDetail {
	return SignDetail{
		IfOpenAddress:     a.IfOpenAddress,
		LocationLatitude:  a.LocationLatitude,
		LocationLongitude: a.LocationLongitude,
		LocationRange:     a.LocationRange,
		LocationAddress:   a.LocationAddress,
		LocationFetched:   a.LocationFetched,
	}
}

type SignDetail struct {
	StartTime    int64
	EndTime      int64
	SignType     int
	IfRefreshEWM bool
	IfPhoto      bool
	// 位置校验相关：老师在活动里设置的签到圆心与半径。
	// 数据来自 getPPTActiveInfo（必要时回退 signDetail）；没有位置校验时为零值。
	IfOpenAddress     bool
	LocationLatitude  float64
	LocationLongitude float64
	LocationRange     int64
	LocationAddress   string
	// LocationFetched 表示本次是否真的查过位置信息。
	// false 通常意味着详情接口没返回定位字段，用于决定是否需要回填缓存。
	LocationFetched bool
}

type FixedParams struct {
	ActiveID     int64
	UID          int64
	CourseID     int64
	ClassID      int64
	IfRefreshEWM bool
	// DBStrategy / StrategyPara 来自刷新二维码链接中的 DB_STRATEGY / STRATEGY_PARA。
	// 定时刷新活动（widget/sign/e）使用 PRIMARY_KEY + id，静态二维码没有这两个参数。
	DBStrategy   string
	StrategyPara string
}

// New 保留旧签名，等价于"直连 + 无节流"，仅用于测试与降级。
func New(aesKey, mobileUA string, insecureTLS bool, activeFetchMax int) *Client {
	return NewWithOptions(Options{
		AESKey:            aesKey,
		FallbackUserAgent: mobileUA,
		InsecureTLS:       insecureTLS,
		ActiveFetchMax:    activeFetchMax,
	})
}

// NewWithOptions 构造带反风控能力的 Client。
func NewWithOptions(o Options) *Client {
	guard := o.Guard
	baseUA := strings.TrimSpace(o.FallbackUserAgent)

	if o.ActiveFetchMax <= 0 {
		o.ActiveFetchMax = 20
	}

	// 直连 Transport：未配置代理、或代理不可用时使用。
	direct, err := antiban.NewTransport("", o.InsecureTLS)
	if err != nil {
		// 只可能在配置损坏时发生，此时退回标准 Transport 保证服务可用。
		direct = &http.Transport{}
	}

	c := &Client{
		aesKey:         o.AESKey,
		mobileUA:       baseUA,
		activeFetchMax: o.ActiveFetchMax,
		opts:           o,
		guard:          guard,
		http: &http.Client{
			Timeout:   20 * time.Second,
			Transport: direct,
		},
		sessions:  make(map[string]*Session),
		httpByKey: make(map[string]*http.Client),
		// 验证码单飞任务表按需惰性初始化（测试会直接构造 &Client{}）。
		captchaFlights: make(map[string]*captchaFlight),
	}
	if guard != nil && guard.Enabled() {
		c.exitIP = antiban.NewExitIPResolver(o.ExitIPProbeURL, o.InsecureTLS)
	}
	return c
}

// profileFor 返回账号的稳定画像；guard 为 nil 时给出一条"空画像"，
// 后续逻辑会自动回退到全局 mobileUA 与直连。
func (c *Client) profileFor(mobile string) antiban.Profile {
	if c == nil || c.guard == nil {
		return antiban.Profile{Mobile: mobile, UserAgent: c.mobileUA}
	}
	p := c.guard.ProfileFor(mobile)
	if strings.TrimSpace(p.UserAgent) == "" {
		p.UserAgent = c.mobileUA
	}
	return p
}

// uaForProfile 返回该账号应当使用的 UA。
func (c *Client) uaForProfile(p antiban.Profile) string {
	if v := strings.TrimSpace(p.UserAgent); v != "" {
		return v
	}
	return c.mobileUA
}

// httpForProfile 返回绑定到该账号出口代理的 http.Client（不带 cookie jar）。
func (c *Client) httpForProfile(p antiban.Profile) *http.Client {
	proxyURL := strings.TrimSpace(p.ProxyURL)
	if proxyURL == "" {
		return c.http
	}
	key := proxyURL
	c.clientMu.Lock()
	defer c.clientMu.Unlock()
	if cli, ok := c.httpByKey[key]; ok {
		return cli
	}
	tr, err := antiban.NewTransport(proxyURL, c.opts.InsecureTLS)
	if err != nil {
		log.Printf("[antiban] proxy %q invalid, fallback to direct: %v", redactProxy(proxyURL), err)
		c.httpByKey[key] = c.http
		return c.http
	}
	cli := &http.Client{Timeout: 20 * time.Second, Transport: tr}
	c.httpByKey[key] = cli
	return cli
}

// clientForSession 返回该会话专属的 http.Client（带上账号 cookie jar 与代理）。
func (c *Client) clientForSession(s *Session) *http.Client {
	cli := *c.httpForProfile(antiban.Profile{Mobile: s.Mobile, ProxyURL: s.ProxyURL, UserAgent: s.UserAgent})
	cli.Jar = s.Jar
	return &cli
}

// acquire 申请一次"写"请求许可（登录/预签到/签到）。
func (c *Client) acquire(ctx context.Context, mobile string) (func(), error) {
	if c == nil || c.guard == nil {
		return func() {}, nil
	}
	return c.guard.Acquire(ctx, mobile)
}

// acquireRead 申请一次"读"请求许可（活动列表/详情）。
func (c *Client) acquireRead(ctx context.Context, mobile string) (func(), error) {
	if c == nil || c.guard == nil {
		return func() {}, nil
	}
	return c.guard.AcquireRead(ctx, mobile)
}

// do 是所有"写"请求的统一入口：节流 → 发请求 → 上报风控信号。
func (c *Client) do(ctx context.Context, mobile string, cli *http.Client, req *http.Request) (*http.Response, error) {
	return c.doWith(ctx, mobile, cli, req, c.acquire)
}

// doRead 是"读"请求入口：共享并发上限，但账号级间隔更短。
func (c *Client) doRead(ctx context.Context, mobile string, cli *http.Client, req *http.Request) (*http.Response, error) {
	return c.doWith(ctx, mobile, cli, req, c.acquireRead)
}

func (c *Client) doWith(
	ctx context.Context,
	mobile string,
	cli *http.Client,
	req *http.Request,
	acquire func(context.Context, string) (func(), error),
) (*http.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	req = req.WithContext(ctx)

	release, err := acquire(ctx, mobile)
	if err != nil {
		return nil, err
	}
	defer release()

	resp, err := cli.Do(req)
	if err != nil {
		c.observe(mobile, antiban.SignalTransient)
		return nil, err
	}
	// 先按状态码做一层粗判，正文级判定在具体接口里补充。
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		c.observe(mobile, antiban.SignalRateLimited)
	case resp.StatusCode == http.StatusForbidden:
		c.observe(mobile, antiban.SignalBlocked)
	case resp.StatusCode >= 500:
		c.observe(mobile, antiban.SignalTransient)
	}
	return resp, nil
}

func (c *Client) observe(mobile string, sig antiban.Signal) {
	if c == nil || c.guard == nil || sig == antiban.SignalNone {
		return
	}
	c.guard.Observe(mobile, sig)
}

// clientIPValue 计算 stuSignajax 的 clientip 参数。
//
//   - empty：保持历史行为（空字符串），最稳；
//   - fixed：使用配置里的固定 IP；
//   - auto ：经代理探测真实出口 IP，让"IP 归属地"与请求来源一致。
func (c *Client) clientIPValue(mobile string, p antiban.Profile) string {
	switch strings.ToLower(strings.TrimSpace(c.opts.ClientIPMode)) {
	case "fixed":
		return strings.TrimSpace(c.opts.FixedClientIP)
	case "auto":
		if v := strings.TrimSpace(p.ExitIP); v != "" {
			return v
		}
		if c.exitIP != nil && strings.TrimSpace(p.ProxyURL) != "" {
			if ip := c.exitIP.Resolve(p.ProxyURL); ip != "" {
				if e, ok := c.guard.(*antiban.Engine); ok {
					e.SetExitIP(mobile, ip)
				}
				return ip
			}
		}
		return ""
	default:
		return ""
	}
}

// applyDeviceHeaders 给请求补齐真实 App 会带的公共头。
// 刻意不手工设置 Accept-Encoding：交给 Go 自动协商并透明解压，
// 手工设置会导致 gzip 正文无法解压。
func applyDeviceHeaders(req *http.Request, ua string) {
	if req == nil {
		return
	}
	if strings.TrimSpace(ua) != "" {
		req.Header.Set("User-Agent", ua)
	}
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "*/*")
	}
	if req.Header.Get("Connection") == "" {
		req.Header.Set("Connection", "keep-alive")
	}
}

func (c *Client) PreLogin(mobile, password string) (*LoginResult, error) {
	profile := c.profileFor(mobile)
	jar, _ := cookiejar.New(nil)
	cli := *c.httpForProfile(profile)
	cli.Jar = jar
	ua := c.uaForProfile(profile)

	// 登录态也统一成移动端 UA：如果登录用浏览器 UA、后面签到用 App UA，
	// 同一 cookie jar 里"客户端身份"前后不一致，本身就是一条异常信号。
	loginUA := "Mozilla/5.0"
	if c.opts.UnifyLoginUA {
		loginUA = ua
	}

	form := url.Values{}
	form.Set("fid", "-1")
	form.Set("uname", encryptXXTByAES(mobile, c.aesKey))
	form.Set("password", encryptXXTByAES(password, c.aesKey))
	form.Set("refer", "https://i.chaoxing.com")
	form.Set("t", "true")
	form.Set("forbidotherlogin", "0")
	form.Set("validate", "")
	form.Set("doubleFactorLogin", "0")
	form.Set("independentId", "0")
	form.Set("independentNameId", "0")

	req, _ := http.NewRequest(http.MethodPost, "https://passport2.chaoxing.com/fanyalogin?"+form.Encode(), nil)
	applyDeviceHeaders(req, loginUA)
	resp, err := c.do(context.Background(), mobile, &cli, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var lr struct {
		Status bool `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil {
		return nil, fmt.Errorf("login decode failed: %w", err)
	}
	if !lr.Status {
		return nil, fmt.Errorf("账号或密码错误")
	}

	req2, _ := http.NewRequest(http.MethodGet, "http://i.chaoxing.com/base", nil)
	// 这一步是 webview 里的 HTML 页面，保持浏览器 UA 更自然，也能保证 HTML 结构稳定。
	applyDeviceHeaders(req2, "Mozilla/5.0")
	resp2, err := c.do(context.Background(), mobile, &cli, req2)
	if err != nil {
		return nil, err
	}
	defer resp2.Body.Close()
	html, _ := io.ReadAll(resp2.Body)
	s := string(html)

	name := extractBetween(s, `<p class="user-name">`, `</p>`)
	avatar := extractBetween(s, `<img class="icon-head" src="`, `">`)
	uid := int64(0)
	for _, ck := range jar.Cookies(&url.URL{Scheme: "https", Host: "passport2.chaoxing.com"}) {
		if ck.Name == "UID" {
			uid, _ = strconv.ParseInt(ck.Value, 10, 64)
			break
		}
	}
	if uid == 0 {
		for _, ck := range jar.Cookies(&url.URL{Scheme: "https", Host: "i.chaoxing.com"}) {
			if ck.Name == "UID" {
				uid, _ = strconv.ParseInt(ck.Value, 10, 64)
				break
			}
		}
	}
	if uid == 0 {
		return nil, fmt.Errorf("登录后未获取到UID")
	}

	fid := fidFromJar(jar)
	if fid == 0 {
		// 少数账号登录后不会下发 _fid cookie，退回到 base 页面里的 fid 配置。
		fid = fidFromHTML(s)
	}

	c.sessionMu.Lock()
	c.sessions[mobile] = &Session{
		Mobile:      mobile,
		Password:    password,
		UID:         uid,
		Name:        name,
		Avatar:      avatar,
		Fid:         fid,
		DeviceCode:  profile.DeviceCode,
		UserAgent:   ua,
		ProxyURL:    profile.ProxyURL,
		Jar:         jar,
		LastLoginAt: time.Now(),
	}
	c.sessionMu.Unlock()

	// 让画像表记下 uid，方便后台按账号排查指纹。
	if binder, ok := c.guard.(interface {
		BindUID(mobile string, uid int64)
	}); ok {
		binder.BindUID(mobile, uid)
	}

	return &LoginResult{UID: uid, Name: name, Avatar: avatar}, nil
}

func (c *Client) ensureSession(mobile, password string) (*Session, error) {
	c.sessionMu.Lock()
	s, ok := c.sessions[mobile]
	c.sessionMu.Unlock()
	if ok && s.Password == password && time.Since(s.LastLoginAt) < 24*time.Hour {
		return s, nil
	}
	_, err := c.PreLogin(mobile, password)
	if err != nil {
		return nil, err
	}
	c.sessionMu.Lock()
	defer c.sessionMu.Unlock()
	return c.sessions[mobile], nil
}

func (c *Client) GetCourses(mobile, password string) ([]Course, error) {
	s, err := c.ensureSession(mobile, password)
	if err != nil {
		return nil, err
	}
	cli := *c.clientForSession(s)

	u := "https://mooc1-api.chaoxing.com/mycourse/backclazzdata?view=json&getTchClazzType=1&mcode="
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	// 与签到请求保持同一设备身份：同一 cookie 会话里 UA 来回切换本身就是异常信号。
	applyDeviceHeaders(req, s.UserAgent)
	resp, err := c.doRead(context.Background(), s.Mobile, &cli, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var payload struct {
		ChannelList []struct {
			Content map[string]interface{} `json:"content"`
		} `json:"channelList"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}

	seen := map[string]struct{}{}
	courses := make([]Course, 0)
	for _, ch := range payload.ChannelList {
		content := ch.Content
		if content == nil {
			continue
		}
		if _, ok := content["folderName"]; ok {
			continue
		}
		if rt, ok := content["roletype"].(float64); ok && int(rt) == 1 {
			continue
		}
		courseMap, ok := content["course"].(map[string]interface{})
		if !ok {
			continue
		}
		dataArr, ok := courseMap["data"].([]interface{})
		if !ok {
			continue
		}
		for _, item := range dataArr {
			m, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			squareURL, _ := m["courseSquareUrl"].(string)
			u2, err := url.Parse(squareURL)
			if err != nil {
				continue
			}
			q := u2.Query()
			courseID, _ := strconv.ParseInt(q.Get("courseId"), 10, 64)
			classID, _ := strconv.ParseInt(q.Get("classId"), 10, 64)
			if courseID == 0 || classID == 0 {
				continue
			}
			key := fmt.Sprintf("%d_%d", courseID, classID)
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			courses = append(courses, Course{
				Teacher:  strVal(m["teacherfactor"]),
				Name:     strVal(m["name"]),
				CourseID: courseID,
				ClassID:  classID,
				Icon:     strVal(m["imageurl"]),
			})
		}
	}
	return courses, nil
}

func (c *Client) GetActives(mobile, password string, courseID, classID int64) ([]Active, error) {
	s, err := c.ensureSession(mobile, password)
	if err != nil {
		return nil, err
	}
	cli := *c.clientForSession(s)

	u := fmt.Sprintf("https://mobilelearn.chaoxing.com/ppt/activeAPI/taskactivelist?courseId=%d&classId=%d", courseID, classID)
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	applyDeviceHeaders(req, s.UserAgent)
	resp, err := c.doRead(context.Background(), s.Mobile, &cli, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var payload interface{}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}

	activeList := findActiveList(payload)
	if len(activeList) == 0 {
		activeList = findBestActivityArray(payload)
	}
	if len(activeList) == 0 {
		log.Printf("GetActives empty source: course=%d class=%d body=%s", courseID, classID, truncateForLog(string(raw), 240))
	}
	out := make([]Active, 0)
	seen := make(map[int64]struct{})
	probeHits := 0
	for _, a := range activeList {
		activeType := int64FromAny(firstNonNil(a["activeType"], a["type"], a["atype"]))
		name := strVal(firstNonNil(a["nameOne"], a["name"], a["activeName"], a["title"]))
		id := int64FromAny(firstNonNil(a["id"], a["activeId"], a["active_id"]))
		if id == 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		if activeType != 2 && !strings.Contains(name, "签到") {
			continue
		}
		if name == "" {
			name = fmt.Sprintf("活动 %d", id)
		}
		// taskactivelist 里常常直接带 locationLatitude/locationLongitude/locationRange，
		// 先把它们解析出来；解析不到也没关系，后面还有 getPPTActiveInfo 兜底。
		loc := SignDetail{}
		applyLocationFields(&loc, a)
		loc.LocationFetched = ValidLatLng(loc.LocationLatitude, loc.LocationLongitude)
		if logLocationCandidates("activelist", id, a) {
			probeHits++
		}
		out = append(out, Active{
			ActiveID:          id,
			Name:              name,
			IfOpenAddress:     loc.IfOpenAddress,
			LocationLatitude:  loc.LocationLatitude,
			LocationLongitude: loc.LocationLongitude,
			LocationRange:     loc.LocationRange,
			LocationAddress:   loc.LocationAddress,
			LocationFetched:   loc.LocationFetched,
		})
		if len(out) >= c.activeFetchMax {
			break
		}
	}
	// 一个定位字段都没找到时，把原始活动的顶层 key 打出来，方便判断返回结构。
	if probeHits == 0 && len(activeList) > 0 {
		log.Printf("[activelist] course=%d class=%d 未发现定位字段，样本顶层 key=%v",
			courseID, classID, TopLevelKeys(activeList[0]))
	}
	return out, nil
}

func (c *Client) GetSignDetail(mobile, password string, activityID int64) (SignDetail, error) {
	s, err := c.ensureSession(mobile, password)
	if err != nil {
		return SignDetail{}, err
	}
	cli := *c.clientForSession(s)
	u := fmt.Sprintf("https://mobilelearn.chaoxing.com/newsign/signDetail?activePrimaryId=%d&type=1", activityID)
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	applyDeviceHeaders(req, s.UserAgent)
	resp, err := c.doRead(context.Background(), s.Mobile, &cli, req)
	if err != nil {
		return SignDetail{}, err
	}
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return SignDetail{}, err
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return SignDetail{}, err
	}
	end := parseTimeMillis(payload["endTime"])
	if end == 0 {
		end = 64060559999000
	}
	detail := SignDetail{
		StartTime:    parseTimeMillis(payload["startTime"]),
		EndTime:      end,
		SignType:     int(int64FromAny(payload["otherId"])),
		IfRefreshEWM: boolFromAny(payload["ifRefreshEwm"]),
		IfPhoto:      boolFromAny(deepFindFirst(payload, "ifphoto", "ifPhoto")),
	}
	// signDetail 本身偶尔也带定位字段，先就地解析一次。
	applyLocationFields(&detail, payload)
	logLocationCandidates("signDetail", activityID, payload)
	// 二维码签到 / 位置签到的距离校验要用到老师设置的签到圆心与半径，
	// signDetail 多数情况下不返回，需要再拉一次 getPPTActiveInfo。
	if detail.SignType == SignQRCode || detail.SignType == SignLocation {
		if info, err := c.fetchActiveInfo(&cli, s, activityID); err != nil {
			log.Printf("[activeInfo] activity=%d err=%v", activityID, err)
		} else {
			applyLocationFields(&detail, info)
			logLocationCandidates("activeInfo", activityID, info)
		}
	}
	// 只有真的解析出圆心才算补齐；否则保持 false，让下次列表刷新再拉一次
	// （同时也会再打一次诊断日志，方便定位字段名）。
	detail.LocationFetched = ValidLatLng(detail.LocationLatitude, detail.LocationLongitude)
	if !detail.LocationFetched && (detail.SignType == SignQRCode || detail.SignType == SignLocation) {
		// 学习通实测只下发 range/locationText，经纬度字段是空串 → 前端会用地址反查圆心。
		if detail.LocationAddress != "" {
			log.Printf("[activeInfo] activity=%d 未下发圆心经纬度，改用地址反查 signType=%d range=%d address=%q",
				activityID, detail.SignType, detail.LocationRange, truncateForLog(detail.LocationAddress, 120))
		} else {
			log.Printf("[activeInfo] activity=%d 未能解析到签到圆心，前端将无法画圈 signType=%d ifopenAddress=%v range=%d",
				activityID, detail.SignType, detail.IfOpenAddress, detail.LocationRange)
		}
	}
	return detail, nil
}

// fetchActiveInfo 拉取活动详情（getPPTActiveInfo），用于读取签到圆心与半径。
// 该接口返回结构随版本变化，这里保留原始 map 交给 applyLocationFields 做兼容解析。
func (c *Client) fetchActiveInfo(cli *http.Client, s *Session, activityID int64) (map[string]interface{}, error) {
	base := fmt.Sprintf("https://mobilelearn.chaoxing.com/v2/apis/active/getPPTActiveInfo?activeId=%d", activityID)
	payload, err := c.getActiveInfoJSON(cli, s, activityID, base)
	if err != nil {
		return nil, err
	}
	if _, _, ok := locationPairFromAny(payload); ok {
		return payload, nil
	}
	// 默认请求拿不到圆心时再带 ifgetrange=1 试一次：返回体里本身就带 ifgetrange 字段，
	// 说明服务端支持“按需下发签到范围/圆心”。
	retry, retryErr := c.getActiveInfoJSON(cli, s, activityID, base+"&ifgetrange=1")
	if retryErr == nil {
		if _, _, ok := locationPairFromAny(retry); ok {
			return retry, nil
		}
	}
	return payload, nil
}

func (c *Client) getActiveInfoJSON(cli *http.Client, s *Session, activityID int64, u string) (map[string]interface{}, error) {
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	applyDeviceHeaders(req, s.UserAgent)
	resp, err := c.doRead(context.Background(), s.Mobile, cli, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(raw, &payload); err != nil {
		log.Printf("[activeInfo] activity=%d status=%d url=%s 非 JSON 返回 body=%q", activityID, resp.StatusCode,
			u, truncateForLog(strings.TrimSpace(string(raw)), 300))
		return nil, err
	}
	// 完整原始返回只截断到 2000 字符，必要时能看到 data 里的字段名。
	log.Printf("[activeInfo] activity=%d status=%d url=%s body=%q", activityID, resp.StatusCode,
		u, truncateForLog(strings.TrimSpace(string(raw)), 2000))
	return payload, nil
}

// locationProbeRe 命中“可能和定位有关”的 JSON 路径，用于诊断日志。
var locationProbeRe = regexp.MustCompile(`(?i)(lat|lng|lon|range|radius|address|location|distance|geo)`)

// logLocationCandidates 打印返回里所有疑似定位字段的“路径=值”，
// 这样即使真实字段名和我们猜的不一样，也能一眼看出来。返回是否命中。
func logLocationCandidates(tag string, activityID int64, payload interface{}) bool {
	if payload == nil {
		return false
	}
	hits := make([]string, 0, 16)
	for _, pair := range JSONLeafPairs(payload, 200) {
		if idx := strings.Index(pair, "="); idx > 0 && locationProbeRe.MatchString(pair[:idx]) {
			hits = append(hits, pair)
		}
	}
	if len(hits) == 0 {
		return false
	}
	log.Printf("[%s] activity=%d 疑似定位字段: %s", tag, activityID, strings.Join(hits, ", "))
	return true
}

// 学习通不同接口/版本里定位字段名不一致，这里按优先级列出候选名。
// 同时覆盖大小写变体（部分老接口返回全小写）。
var (
	// locationLatLngPairs 是成对的经纬度字段：同一组两个字段一定同时出现，
	// 必须成对解析。实测 getPPTActiveInfo 会同时下发
	// locationLatitude=""（空串）、latitude=0 和 locationLatitude_gd=""，
	// 按“第一个命中的 key”去取值会被空串挡住，导致永远解析不到圆心。
	locationLatLngPairs = [][2]string{
		{"locationLatitude", "locationLongitude"},
		{"locationLatitude_gd", "locationLongitude_gd"},
		{"locationlatitude", "locationlongitude"},
		{"activeLatitude", "activeLongitude"},
		{"signLatitude", "signLongitude"},
		{"latitude", "longitude"},
	}
	locationRangeKeys = []string{
		"locationRange", "locationrange", "activeRange", "signRange",
		"radius", "range",
	}
	// locationText 是学习通下发的“格式化地址 + POI 名”，
	// 例如 “湖南省湘西…人民南路120号 吉首大学-图书馆”。
	// 它常常是唯一可用的定位线索（经纬度被服务端置空），前端可据此反查圆心。
	locationAddrKeys = []string{
		"locationAddress", "locationaddress", "activeAddress", "signAddress",
		"locationText", "locationtext", "positionText", "positiontext",
		"locationName", "locationname", "address", "location",
	}
	locationOpenKeys = []string{
		"ifopenAddress", "ifopenaddress", "ifOpenAddress",
		"ifopenLocation", "ifOpenLocation", "openAddress",
	}
)

// applyLocationFields 从活动详情 JSON 中解析签到圆心/半径/地址，写回 SignDetail。
// 多来源（signDetail / getPPTActiveInfo）可以重复调用，非零值会覆盖旧值。
func applyLocationFields(detail *SignDetail, payload map[string]interface{}) {
	if detail == nil || len(payload) == 0 {
		return
	}
	if lat, lng, ok := locationPairFromAny(payload); ok {
		detail.LocationLatitude = lat
		detail.LocationLongitude = lng
	}
	if rng := int64FromAny(deepFindFirst(payload, locationRangeKeys...)); rng > 0 {
		detail.LocationRange = rng
	}
	if addr := locationAddressValue(deepFindFirst(payload, locationAddrKeys...)); addr != "" {
		detail.LocationAddress = addr
	}
	if open, ok := firstBoolField(payload, locationOpenKeys...); ok {
		detail.IfOpenAddress = open
	} else if detail.LocationRange > 0 || ValidLatLng(detail.LocationLatitude, detail.LocationLongitude) {
		// 没显式声明时，只要拿到了圆心/半径就认为该活动启用了位置校验。
		detail.IfOpenAddress = true
	}
}

// locationPairFromAny 按候选组合成对解析经纬度。
// 关键点：第一个候选（locationLatitude）即使存在也可能是个空字符串，
// 必须继续往后试，直到拿到一对合法坐标为止。
func locationPairFromAny(payload map[string]interface{}) (float64, float64, bool) {
	for _, pair := range locationLatLngPairs {
		lat, ok := floatFromAny(deepFindFirst(payload, pair[0]))
		if !ok {
			continue
		}
		lng, ok := floatFromAny(deepFindFirst(payload, pair[1]))
		if !ok {
			continue
		}
		// 0 或超范围视为无效，避免把空字段当成北京天安门外的海面坐标。
		if ValidLatLng(lat, lng) {
			return lat, lng, true
		}
	}
	return 0, 0, false
}

// firstBoolField 按候选名顺序查找布尔字段，返回 (值, 是否存在)。
func firstBoolField(payload map[string]interface{}, keys ...string) (bool, bool) {
	for _, k := range keys {
		raw, ok := deepFindKey(payload, k)
		if !ok {
			continue
		}
		return boolFromAny(raw), true
	}
	return false, false
}

// ValidLatLng 判断经纬度是否是有效的非零坐标。
func ValidLatLng(lat, lng float64) bool {
	if lat == 0 && lng == 0 {
		return false
	}
	return lat >= -90 && lat <= 90 && lng >= -180 && lng <= 180
}

// locationAddressValue 只接受字符串形态的地址，避免把嵌套对象转成 "map[...]"。
func locationAddressValue(v interface{}) string {
	s, ok := v.(string)
	if !ok {
		return ""
	}
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "map[") {
		return ""
	}
	return s
}

func (c *Client) PreSign(mobile, password string, fixed FixedParams, code, enc string) error {
	s, err := c.ensureSession(mobile, password)
	if err != nil {
		return err
	}
	cli := *c.clientForSession(s)

	vals := url.Values{}
	vals.Set("courseId", strconv.FormatInt(fixed.CourseID, 10))
	vals.Set("classId", strconv.FormatInt(fixed.ClassID, 10))
	vals.Set("activePrimaryId", strconv.FormatInt(fixed.ActiveID, 10))
	vals.Set("general", "1")
	vals.Set("sys", "1")
	vals.Set("ls", "1")
	vals.Set("appType", "15")
	vals.Set("uid", strconv.FormatInt(fixed.UID, 10))
	vals.Set("isTeacherViewOpen", "0")
	if fixed.IfRefreshEWM && code != "" {
		// 注意：这里必须传原始字符串，vals.Encode() 会统一转义一次；
		// 若提前 QueryEscape 会造成双重编码（SIGNIN%253Aaid...），学习通无法解析 rcode。
		vals.Set("rcode", buildPreSignRCode(fixed.ActiveID, code, enc))
	}

	req, _ := http.NewRequest(http.MethodGet, "https://mobilelearn.chaoxing.com/newsign/preSign?"+vals.Encode(), nil)
	applyDeviceHeaders(req, s.UserAgent)
	resp, err := c.do(context.Background(), s.Mobile, &cli, req)
	if err != nil {
		return err
	}
	preBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	// 刷新二维码是否被接受主要看这里：静态码/刷新码的差异只有 rcode。
	log.Printf("[preSign] activeId=%d refresh=%t c=%s enc=%s body=%q",
		fixed.ActiveID, fixed.IfRefreshEWM,
		truncateForLog(code, 16), truncateForLog(enc, 24),
		truncateForLog(strings.TrimSpace(string(preBody)), 300))

	// 刷新二维码（widget/sign/e）链接会带 DB_STRATEGY=PRIMARY_KEY&STRATEGY_PARA=id，
	// analysis / analysis2 必须沿用同一套查询策略；静态码没有该参数时保持 RANDOM。
	dbStrategy := strings.TrimSpace(fixed.DBStrategy)
	if dbStrategy == "" {
		dbStrategy = "RANDOM"
	}
	strategyPara := strings.TrimSpace(fixed.StrategyPara)

	a1Vals := buildAnalysisQuery(fixed.ActiveID, dbStrategy, strategyPara)
	a1 := "https://mobilelearn.chaoxing.com/pptSign/analysis?" + a1Vals.Encode()
	req2, _ := http.NewRequest(http.MethodGet, a1, nil)
	applyDeviceHeaders(req2, s.UserAgent)
	resp2, err := c.do(context.Background(), s.Mobile, &cli, req2)
	if err != nil {
		return err
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	code2, ok := extractAnalysisCode(body2)
	log.Printf("[analysis] activeId=%d strategy=%s para=%s code=%t body=%q",
		fixed.ActiveID, dbStrategy, strategyPara, ok,
		truncateForLog(strings.TrimSpace(string(body2)), 300))
	if !ok {
		return nil
	}
	a2Vals := url.Values{}
	a2Vals.Set("DB_STRATEGY", dbStrategy)
	if strategyPara != "" {
		a2Vals.Set("STRATEGY_PARA", strategyPara)
	}
	a2Vals.Set("code", code2)
	a2 := "https://mobilelearn.chaoxing.com/pptSign/analysis2?" + a2Vals.Encode()
	req3, _ := http.NewRequest(http.MethodGet, a2, nil)
	applyDeviceHeaders(req3, s.UserAgent)
	resp3, err := c.do(context.Background(), s.Mobile, &cli, req3)
	if err == nil {
		_, _ = io.Copy(io.Discard, resp3.Body)
		resp3.Body.Close()
	}
	return nil
}

func (c *Client) GetPanUploadToken(mobile, password string) (string, error) {
	s, err := c.ensureSession(mobile, password)
	if err != nil {
		return "", err
	}
	cli := *c.clientForSession(s)

	req, _ := http.NewRequest(http.MethodGet, "https://pan-yz.chaoxing.com/api/token/uservalid", nil)
	applyDeviceHeaders(req, s.UserAgent)
	resp, err := c.doRead(context.Background(), s.Mobile, &cli, req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var payload interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("pan token decode failed: %w", err)
	}
	token := strings.TrimSpace(strVal(deepFindFirst(payload, "_token", "token")))
	if token == "" {
		return "", fmt.Errorf("pan token missing: %s", truncateForLog(string(body), 200))
	}
	return token, nil
}

func (c *Client) UploadPanFile(mobile, password, filename, contentType string, data []byte) (string, error) {
	if len(data) == 0 {
		return "", fmt.Errorf("photo file is empty")
	}
	s, err := c.ensureSession(mobile, password)
	if err != nil {
		return "", err
	}
	token, err := c.GetPanUploadToken(mobile, password)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(filename) == "" {
		filename = "photo.jpg"
	}
	filename = sanitizeMultipartFilename(filename)
	if strings.TrimSpace(contentType) == "" {
		contentType = "application/octet-stream"
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	partHeader := textproto.MIMEHeader{}
	partHeader.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, escapeMultipartFilename(filename)))
	partHeader.Set("Content-Type", contentType)
	part, err := writer.CreatePart(partHeader)
	if err != nil {
		return "", err
	}
	if _, err := part.Write(data); err != nil {
		return "", err
	}
	if err := writer.WriteField("puid", strconv.FormatInt(s.UID, 10)); err != nil {
		return "", err
	}
	if err := writer.Close(); err != nil {
		return "", err
	}

	cli := *c.clientForSession(s)
	uploadURL := "https://pan-yz.chaoxing.com/upload?_from=mobilelearn&_token=" + url.QueryEscape(token)
	req, _ := http.NewRequest(http.MethodPost, uploadURL, &body)
	applyDeviceHeaders(req, s.UserAgent)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := c.do(context.Background(), s.Mobile, &cli, req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("pan upload failed: http %d %s", resp.StatusCode, truncateForLog(string(respBody), 200))
	}

	var payload interface{}
	if err := json.Unmarshal(respBody, &payload); err != nil {
		return "", fmt.Errorf("pan upload decode failed: %w", err)
	}
	objectID := strings.TrimSpace(strVal(deepFindFirst(payload, "objectId", "objectid")))
	if objectID == "" {
		return "", fmt.Errorf("pan upload objectId missing: %s", truncateForLog(string(respBody), 200))
	}
	return objectID, nil
}

func (c *Client) SignPhoto(mobile, password string, fixed FixedParams, objectID string) (string, error) {
	objectID = strings.TrimSpace(objectID)
	if objectID == "" {
		return "", fmt.Errorf("missing photo object_id")
	}
	return c.Sign(mobile, password, fixed, SignNormal, map[string]interface{}{"object_id": objectID})
}

func (c *Client) Sign(mobile, password string, fixed FixedParams, signType int, special map[string]interface{}) (string, error) {
	s, err := c.ensureSession(mobile, password)
	if err != nil {
		return "", err
	}
	cli := *c.clientForSession(s)

	params := url.Values{}
	params.Set("activeId", strconv.FormatInt(fixed.ActiveID, 10))
	params.Set("uid", strconv.FormatInt(fixed.UID, 10))
	params.Set("clientip", c.clientIPValue(s.Mobile, c.profileFor(s.Mobile)))
	params.Set("appType", "15")
	params.Set("fid", "")
	params.Set("name", s.Name)
	if objectID := strings.TrimSpace(strVal(firstNonNil(special["object_id"], special["objectId"]))); objectID != "" {
		params.Set("objectId", objectID)
		params.Set("useragent", "")
		params.Set("latitude", "-1")
		params.Set("longitude", "-1")
	}
	// 先空 validate 发起一次请求；仅在学习通明确要求时再走验证码。
	params.Set("validate", "")

	switch signType {
	case SignQRCode:
		enc := strVal(special["enc"])
		if enc == "" {
			return "", fmt.Errorf("缺少二维码 enc 参数")
		}
		params.Set("enc", enc)
		// 学习通二维码签到要求携带账号所属学校的真实 fid。
		// 空 fid 会被判为无效请求（返回“签到失败，请重新扫描。”）。
		if s.Fid > 0 {
			params.Set("fid", strconv.FormatInt(s.Fid, 10))
		}
		if fixed.CourseID > 0 {
			params.Set("courseId", strconv.FormatInt(fixed.CourseID, 10))
		}
		// 设备标识与反作弊参数：与 ChaoxingSignFaker 的二维码签到请求保持一致。
		if s.DeviceCode != "" {
			params.Set("deviceCode", s.DeviceCode)
		}
		params.Set("vpProbability", "-1")
		params.Set("vpStrategy", "")
		// 兼容 1.0 的二维码附加位置变种：透传 location 给学习通。
		// 兼容两种输入：
		// 1) special.location（1.0 旧格式）
		// 2) special.latitude/longitude/description（2.0 前端当前格式）
		if locationJSON, ok := buildQRCodeLocationParam(special); ok {
			params.Set("location", locationJSON)
			// locationResult 是学习通要求的第二个定位参数：
			// 结构与 location 相同，额外携带 mockData 字符串（声明未使用模拟定位）。
			if locResult, ok := buildQRCodeLocationResultParam(special); ok {
				params.Set("locationResult", locResult)
			}
			// 排查定位参数：确认二维码签到实际下发的 location JSON。
			log.Printf("[sign/qrcode] activity=%d uid=%d fid=%d deviceCode=%s location=%s locationResult=%s",
				fixed.ActiveID, fixed.UID, s.Fid, truncateForLog(s.DeviceCode, 12),
				truncateForLog(locationJSON, 500), truncateForLog(params.Get("locationResult"), 500))
		} else {
			// 没有 location 时学习通只会收到 latitude=-1&longitude=-1，极易返回 errorLocation3。
			log.Printf("[sign/qrcode] activity=%d uid=%d location=MISSING latitude=%q longitude=%q description=%q",
				fixed.ActiveID, fixed.UID,
				strVal(special["latitude"]), strVal(special["longitude"]), strVal(special["description"]))
		}
		// 参考实现（ChaoxingSignFaker）的二维码签到不携带 useragent 参数，
		// 空 useragent 反而可能被学习通当作异常客户端。
		params.Del("useragent")
		params.Set("latitude", "-1")
		params.Set("longitude", "-1")
	case SignGesture, SignCode:
		signCode := strVal(special["sign_code"])
		if signCode == "" {
			return "", fmt.Errorf("缺少 sign_code 参数")
		}
		if signType == SignGesture {
			checkURL := fmt.Sprintf("https://mobilelearn.chaoxing.com/widget/sign/pcStuSignController/checkSignCode?activeId=%d&signCode=%s", fixed.ActiveID, url.QueryEscape(signCode))
			reqC, _ := http.NewRequest(http.MethodGet, checkURL, nil)
			applyDeviceHeaders(reqC, s.UserAgent)
			respC, err := c.do(context.Background(), s.Mobile, &cli, reqC)
			if err == nil {
				defer respC.Body.Close()
				var check struct {
					Result   int    `json:"result"`
					ErrorMsg string `json:"errorMsg"`
				}
				_ = json.NewDecoder(respC.Body).Decode(&check)
				if check.Result != 1 {
					if check.ErrorMsg == "" {
						check.ErrorMsg = "手势码校验失败"
					}
					return "", fmt.Errorf("%s", check.ErrorMsg)
				}
			}
		}
		params.Set("signCode", signCode)
		params.Set("latitude", "")
		params.Set("longitude", "")
	case SignLocation:
		params.Set("address", strVal(special["description"]))
		params.Set("latitude", strVal(special["latitude"]))
		params.Set("longitude", strVal(special["longitude"]))
		if accStr := strings.TrimSpace(strVal(special["accuracy"])); accStr != "" {
			params.Set("accuracy", accStr)
		}
		params.Set("ifTiJiao", "1")
	default:
		params.Set("latitude", "-1")
		params.Set("longitude", "-1")
	}

	// 二维码签到在部分场景下会出现 enc 只能使用一次的情况。
	// 若先空 validate 试探再重试，会导致第二次请求被判定“签到校验未通过”。
	// 这里与 1.0 行为对齐：二维码签到首发请求直接携带验证码。
	if signType == SignQRCode {
		// 调用方（SignService）会把预抓的验证码放进 special["validate"]，
		// 让它与预签到并行完成；只有没预抓到时才在这里兜底。
		// 兜底同样走单飞：如果预取还在途就等同一个结果，失败则快速返回，
		// 不再重复解一遍验证码（那会让单次签到耗时翻倍）。
		validate := strVal(special["validate"])
		if validate == "" {
			v, err := c.awaitCaptchaPrefetch(s.Mobile, fixed)
			if err != nil {
				log.Printf("captcha fetch failed before qrcode sign: activity=%d uid=%d err=%v", fixed.ActiveID, fixed.UID, err)
			} else {
				validate = v
			}
		}
		if validate != "" {
			params.Set("validate", validate)
		}
		return c.doStuSignRequest(&cli, s, params)
	}

	result, err := c.doStuSignRequest(&cli, s, params)
	if err != nil {
		return "", err
	}
	if result != "validate" {
		return result, nil
	}

	// 仅对需要验证码的活动执行一次验证码识别，不在后端重试。
	validate, err := c.fetchCaptchaValidate(s.Mobile, fixed)
	if err != nil {
		log.Printf("captcha fetch failed: activity=%d uid=%d err=%v", fixed.ActiveID, fixed.UID, err)
		return "validate", nil
	}
	params.Set("validate", validate)
	return c.doStuSignRequest(&cli, s, params)
}

func (c *Client) doStuSignRequest(cli *http.Client, s *Session, params url.Values) (string, error) {
	u := "https://mobilelearn.chaoxing.com/pptSign/stuSignajax?" + params.Encode()
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	applyDeviceHeaders(req, s.UserAgent)
	resp, err := c.do(context.Background(), s.Mobile, cli, req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	bodyStr := strings.TrimSpace(string(body))

	// 正文级风控判定：这一步是熔断器最重要的输入。
	c.observe(s.Mobile, antiban.ClassifySignBody(resp.StatusCode, bodyStr))

	// 便于排查二维码 enc 失效、定位参数缺失等场景：打印学习通原始返回。
	// enc / validate 属敏感内容，默认脱敏；排障时可用 antiban.log_payloads 打开。
	log.Printf("[stuSignajax] activeId=%s uid=%s enc=%s validate=%t location=%s latitude=%q longitude=%q body=%q",
		params.Get("activeId"), params.Get("uid"),
		c.logValue(params.Get("enc"), 12), params.Get("validate") != "",
		c.logValue(params.Get("location"), 600),
		params.Get("latitude"), params.Get("longitude"),
		truncateForLog(bodyStr, 500))
	if c.opts.LogPayloads {
		// 完整 query（含 location）便于对照学习通定位校验。
		log.Printf("[stuSignajax] full_query=%s", truncateForLog(params.Encode(), 2000))
	}
	return bodyStr, nil
}

// analysisCodeRe 兼容学习通 analysis 页面里 code 的几种写法：
// code='+xxx' / code='\+xxx' / code='\\+xxx' / code='\+'xxx'。
var analysisCodeRe = regexp.MustCompile(`code='\\*\+'?([^']*)'`)

func extractAnalysisCode(body []byte) (string, bool) {
	m := analysisCodeRe.FindSubmatch(body)
	if len(m) < 2 {
		return "", false
	}
	return string(m[1]), true
}

// buildPreSignRCode 返回预签到 rcode 的原始值（未 URL 编码）。
// 调用方应通过 url.Values.Set 写入，由 Encode() 统一转义一次。
func buildPreSignRCode(activeID int64, code, enc string) string {
	return fmt.Sprintf("SIGNIN:aid=%d&source=15&Code=%s&enc=%s", activeID, code, enc)
}

// buildAnalysisQuery 按二维码链接携带的查询策略组装 analysis 参数。
// dbStrategy 为空时回退 RANDOM（静态二维码行为）；
// strategyPara 形如 id 时，额外以该名字带上活动 id。
func buildAnalysisQuery(activeID int64, dbStrategy, strategyPara string) url.Values {
	dbStrategy = strings.TrimSpace(dbStrategy)
	if dbStrategy == "" {
		dbStrategy = "RANDOM"
	}
	strategyPara = strings.TrimSpace(strategyPara)

	vals := url.Values{}
	vals.Set("vs", "1")
	vals.Set("DB_STRATEGY", dbStrategy)
	vals.Set("aid", strconv.FormatInt(activeID, 10))
	if strategyPara != "" {
		vals.Set("STRATEGY_PARA", strategyPara)
		if _, exists := vals[strategyPara]; !exists {
			vals.Set(strategyPara, strconv.FormatInt(activeID, 10))
		}
	}
	return vals
}

func sanitizeMultipartFilename(filename string) string {
	filename = strings.TrimSpace(filename)
	if filename == "" {
		return "photo.jpg"
	}
	filename = strings.ReplaceAll(filename, "\\", "_")
	filename = strings.ReplaceAll(filename, "/", "_")
	filename = strings.ReplaceAll(filename, "\x00", "")
	return filename
}

func escapeMultipartFilename(filename string) string {
	return strings.NewReplacer("\\", "\\\\", `"`, "\\\"").Replace(filename)
}

// locationMockDataJSON 是学习通定位结构里的反模拟定位声明：
// strategy=0（默认策略）、probability=-1（未检测到伪造定位）。
// 只有 locationResult 参数会携带它，location 参数不带。
const locationMockDataJSON = `{"strategy":0,"probability":-1}`

// buildQRCodeLocationParam 生成 stuSignajax 的 location 参数（不带 mockData）。
func buildQRCodeLocationParam(special map[string]interface{}) (string, bool) {
	return buildQRCodeLocationPayload(special, false)
}

// buildQRCodeLocationResultParam 生成 stuSignajax 的 locationResult 参数（带 mockData）。
func buildQRCodeLocationResultParam(special map[string]interface{}) (string, bool) {
	return buildQRCodeLocationPayload(special, true)
}

// buildQRCodeLocationPayload 生成学习通二维码签到的定位 JSON。
// 结构对齐 ChaoxingSignFaker 的 addLocationDataParameter：
//
//	{"result":1,"latitude":30.1,"longitude":120.2,"address":"..."}            // location
//	{"result":1,"latitude":30.1,"longitude":120.2,"address":"...","mockData":"{\"strategy\":0,\"probability\":-1}"} // locationResult
//
// 注意：
//   - result 必须是整数 1（字符串或缺失都会让服务端判为无效定位）；
//   - 没有 altitude 字段；
//   - mockData 是“JSON 字符串”而不是对象，且只在 locationResult 中携带。
func buildQRCodeLocationPayload(special map[string]interface{}, withMockData bool) (string, bool) {
	if locRaw, ok := special["location"]; ok && locRaw != nil {
		var raw string
		switch v := locRaw.(type) {
		case string:
			raw = strings.TrimSpace(v)
		default:
			if b, err := json.Marshal(v); err == nil && string(b) != "null" && string(b) != `""` {
				raw = string(b)
			}
		}
		if raw != "" {
			if !withMockData {
				return raw, true
			}
			// 1.0 透传的 location 需要补上 mockData 才能作为 locationResult 使用。
			var payload map[string]interface{}
			if err := json.Unmarshal([]byte(raw), &payload); err == nil {
				payload["mockData"] = locationMockDataJSON
				if b, err := json.Marshal(payload); err == nil {
					return string(b), true
				}
			}
			return raw, true
		}
	}

	latStr := strings.TrimSpace(strVal(special["latitude"]))
	lngStr := strings.TrimSpace(strVal(special["longitude"]))
	desc := strings.TrimSpace(strVal(special["description"]))
	if latStr == "" && lngStr == "" && desc == "" {
		return "", false
	}

	location := map[string]interface{}{
		"result":  1,
		"address": desc,
	}
	if lat, err := strconv.ParseFloat(latStr, 64); err == nil {
		location["latitude"] = lat
	} else if latStr != "" {
		location["latitude"] = latStr
	}
	if lng, err := strconv.ParseFloat(lngStr, 64); err == nil {
		location["longitude"] = lng
	} else if lngStr != "" {
		location["longitude"] = lngStr
	}
	if withMockData {
		location["mockData"] = locationMockDataJSON
	}

	b, err := json.Marshal(location)
	if err != nil || string(b) == "{}" {
		return "", false
	}
	return string(b), true
}

// fidFromJar 从登录后的 cookie 中解析账号所属学校（单位）id。
// 学习通登录后会在 i.chaoxing.com / passport2.chaoxing.com 下写入 _fid（部分账号为 fid）。
func fidFromJar(jar *cookiejar.Jar) int64 {
	if jar == nil {
		return 0
	}
	hosts := []string{
		"i.chaoxing.com",
		"passport2.chaoxing.com",
		"passport.chaoxing.com",
		"mobilelearn.chaoxing.com",
	}
	for _, host := range hosts {
		for _, ck := range jar.Cookies(&url.URL{Scheme: "https", Host: host}) {
			if ck.Name != "_fid" && ck.Name != "fid" {
				continue
			}
			if v, err := strconv.ParseInt(strings.TrimSpace(ck.Value), 10, 64); err == nil && v > 0 {
				return v
			}
		}
	}
	return 0
}

// fidFromHTML 从登录后的 i.chaoxing.com/base 页面里兜底解析 fid。
// 页面里常见写法：var _fid = "12345"; / "fid":12345 / fid='12345'。
var fidFromHTMLRe = regexp.MustCompile(`(?i)(?:_fid|"fid"|'fid'|\bfid)\s*[:=]\s*["']?(\d{3,})`)

func fidFromHTML(html string) int64 {
	m := fidFromHTMLRe.FindStringSubmatch(html)
	if len(m) < 2 {
		return 0
	}
	v, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil || v <= 0 {
		return 0
	}
	return v
}

// newDeviceCode 生成一次随机设备标识（32 位小写 hex）。
//
// 注意：正式流程已改用账号级稳定的 antiban.StableDeviceCode，
// 这里保留随机版本供测试/对比使用，不要再接回登录链路。
func newDeviceCode() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%032x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// logValue 按配置决定是否打印敏感内容：
// 关闭 log_payloads 时只保留尾部，避免把 enc / 定位串原样写进日志。
func (c *Client) logValue(raw string, keep int) string {
	if c != nil && c.opts.LogPayloads {
		return truncateForLog(raw, keep)
	}
	return truncateForLog(maskSecret(raw), keep)
}

// maskSecret 保留前 4 位，其余替换为 *，用于 enc / deviceCode 等敏感值。
func maskSecret(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if len(s) <= 4 {
		return "***"
	}
	return s[:4] + "***"
}

// redactProxy 隐藏代理地址里的账号密码，只保留 host:port。
func redactProxy(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "***"
	}
	return u.Scheme + "://" + u.Host
}

// sleepCtx 是可被取消的睡眠，用于验证码重试之间的随机停顿。
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
