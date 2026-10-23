package xxt

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"xbt2/server/internal/antiban"
)

// captchaSolveBudget 是一次验证码识别允许消耗的时间上限。
//
// 刷新二维码的 enc 有效期只有 5~10s，签到链路后面还要留一次 stuSignajax 的往返。
// 超过这个预算就快速失败，让前端换一张新码重试，而不是把有效期全部烧在
// 试错上（旧实现最多 8 个候选 × 每次一个代理往返，慢节点上能到 16s）。
const captchaSolveBudget = 12 * time.Second

// isTransportError 判断错误是否来自网络层（代理超时、连接被重置等），
// 而不是"滑块位置不对"这类业务失败。两者的重试策略完全不同。
func isTransportError(err error) bool {
	if err == nil {
		return false
	}
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return true
	}
	var nerr net.Error
	return errors.As(err, &nerr)
}

// captchaImageRequest 说明为什么验证码图片走"读"档：
//
// 元信息 / 背景图 / 滑块图都是静态下载，不改变账号状态。若按写请求
// （300~1200ms）排队，会把账号的下一次写请求（stuSignajax）额外推后一个间隔。
// 读档与写档在 pacer 里是两条独立时间线（AcquireRead 用 "<mobile>|r"），
// 既仍然受全局并发上限约束，也不会占用签到的写窗口。
func (c *Client) captchaImageRequest(cli *http.Client, mobile string, req *http.Request) (*http.Response, error) {
	return c.doRead(context.Background(), mobile, cli, req)
}

// FetchCaptchaValidate 预抓一次滑动验证码，返回可直接用于 stuSignajax 的 validate。
// 验证码使用独立的临时 cookie jar，与目标账号会话互不影响，
// 因此调用方可以把它和预签到等步骤并行执行，缩短刷新二维码的签到耗时。
//
// mobile 用于取该账号的代理与设备指纹，并参与节流：验证码服务本身也有频率风控，
// 暴力枚举滑块位置是最容易触发封禁的行为之一。
func (c *Client) FetchCaptchaValidate(mobile string, fixed FixedParams) (string, error) {
	return c.fetchCaptchaValidate(mobile, fixed)
}

func (c *Client) fetchCaptchaValidate(mobile string, fixed FixedParams) (string, error) {
	started := time.Now()
	profile := c.profileFor(mobile)
	ua := c.uaForProfile(profile)

	jar, _ := cookiejar.New(nil)
	captchaHTTP := *c.httpForProfile(profile)
	captchaHTTP.Jar = jar

	captchaKey, token := generateCaptchaKeyAndToken(captchaID, captchaType)
	referer := fmt.Sprintf(
		"https://mobilelearn.chaoxing.com/page/sign/signIn?courseId=%d&classId=%d&activeId=%d&fid=0&timetable=0",
		fixed.CourseID, fixed.ClassID, fixed.ActiveID,
	)

	imageURL := "https://captcha.chaoxing.com/captcha/get/verification/image"
	q := url.Values{}
	q.Set("callback", "cx_captcha_function")
	q.Set("captchaId", captchaID)
	q.Set("type", captchaType)
	q.Set("version", "1.1.20")
	q.Set("captchaKey", captchaKey)
	q.Set("token", token)
	q.Set("referer", referer)

	req, _ := http.NewRequest(http.MethodGet, imageURL+"?"+q.Encode(), nil)
	setCaptchaHeaders(req, referer, ua)
	resp, err := c.captchaImageRequest(&captchaHTTP, mobile, req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	obj, err := extractJSONPObject(body)
	if err != nil {
		return "", err
	}
	var payload struct {
		Token               string `json:"token"`
		ImageVerificationVo struct {
			ShadeImage  string `json:"shadeImage"`
			CutoutImage string `json:"cutoutImage"`
		} `json:"imageVerificationVo"`
	}
	if err := json.Unmarshal(obj, &payload); err != nil {
		return "", err
	}
	if payload.Token == "" || payload.ImageVerificationVo.ShadeImage == "" || payload.ImageVerificationVo.CutoutImage == "" {
		return "", errors.New("captcha payload incomplete")
	}

	// 背景图与滑块图互不依赖，并行下载可以省掉一次代理往返（慢节点上约 1~2s）。
	var (
		bgBytes, targetBytes []byte
		bgErr, targetErr     error
		wg                   sync.WaitGroup
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		bgBytes, bgErr = c.downloadBytes(&captchaHTTP, mobile, ua, payload.ImageVerificationVo.ShadeImage, referer)
	}()
	go func() {
		defer wg.Done()
		targetBytes, targetErr = c.downloadBytes(&captchaHTTP, mobile, ua, payload.ImageVerificationVo.CutoutImage, referer)
	}()
	wg.Wait()
	if bgErr != nil {
		return "", bgErr
	}
	if targetErr != nil {
		return "", targetErr
	}
	candidates, err := detectSlideXCandidates(targetBytes, bgBytes)
	if err != nil {
		return "", err
	}

	// 关键风控点：以前最多会对一张验证码连续尝试 40 个坐标，
	// 在学习通看来这就是一个明显的机器人在穷举。这里改成
	// "少量最优候选 + 每次失败后随机停顿"，代价是偶发需要用户重试。
	maxAttempts := 8
	if c.guard != nil {
		maxAttempts = c.guard.CaptchaMaxAttempts()
	}
	// 整段验证码识别的总预算。刷新二维码的 enc 只有几秒有效期，
	// 无脑重试到第 8 个候选会把有效期全部吃光，反而不如快速失败让前端换新码。
	budget := started.Add(captchaSolveBudget)
	// 连续的网络层失败（代理超时 / 连接被重置）与"滑块位置不对"不同：
	// 代理连续两次都不通时，继续把 8 个候选全试一遍只是在烧二维码有效期。
	transportFails := 0

	var lastErr error
	attempts := 0
	for _, x := range candidates {
		if attempts >= maxAttempts {
			break
		}
		if time.Now().After(budget) {
			lastErr = errors.New("captcha solve budget exceeded")
			break
		}
		attempts++
		validate, err := c.verifyCaptchaAtX(&captchaHTTP, mobile, ua, referer, payload.Token, x)
		if err == nil && strings.TrimSpace(validate) != "" {
			return validate, nil
		}
		lastErr = err
		if isTransportError(err) {
			transportFails++
			if transportFails >= 2 {
				lastErr = fmt.Errorf("captcha transport failed %d times: %w", transportFails, err)
				break
			}
		} else {
			transportFails = 0
		}
		// 失败后随机停顿，避免"同一毫秒连续提交 8 次"的机器特征。
		_ = sleepCtx(context.Background(), antiban.RandomDuration(180*time.Millisecond, 620*time.Millisecond))
	}
	if attempts > 0 {
		// 用尽候选仍失败：说明验证码链路大概率已经被风控，记一次信号。
		c.observe(mobile, antiban.SignalCaptcha)
	}
	if lastErr == nil {
		lastErr = errors.New("captcha verify failed")
	}
	return "", lastErr
}

func (c *Client) verifyCaptchaAtX(cli *http.Client, mobile, ua, referer, token string, x int) (string, error) {
	checkURL := "https://captcha.chaoxing.com/captcha/check/verification/result"
	q2 := url.Values{}
	q2.Set("callback", "cx_captcha_function")
	q2.Set("captchaId", captchaID)
	q2.Set("type", captchaType)
	q2.Set("token", token)
	q2.Set("textClickArr", fmt.Sprintf("[{\"x\":%d}]", x))
	q2.Set("coordinate", "[]")
	q2.Set("runEnv", "10")
	q2.Set("version", "1.1.20")
	req2, _ := http.NewRequest(http.MethodGet, checkURL+"?"+q2.Encode(), nil)
	setCaptchaHeaders(req2, referer, ua)
	resp2, err := c.do(context.Background(), mobile, cli, req2)
	if err != nil {
		return "", err
	}
	defer resp2.Body.Close()
	body2, _ := io.ReadAll(resp2.Body)
	obj2, err := extractJSONPObject(body2)
	if err != nil {
		return "", err
	}
	var verify struct {
		Result    bool   `json:"result"`
		ExtraData string `json:"extraData"`
	}
	if err := json.Unmarshal(obj2, &verify); err != nil {
		return "", err
	}
	if !verify.Result {
		return "", errors.New("captcha verify failed")
	}
	var extra struct {
		Validate string `json:"validate"`
	}
	if err := json.Unmarshal([]byte(verify.ExtraData), &extra); err != nil {
		return "", err
	}
	if extra.Validate == "" {
		return "", errors.New("validate empty")
	}
	return extra.Validate, nil
}

func (c *Client) downloadBytes(cli *http.Client, mobile, ua, rawURL string, referer string) ([]byte, error) {
	req, _ := http.NewRequest(http.MethodGet, rawURL, nil)
	setCaptchaHeaders(req, referer, ua)
	resp, err := c.captchaImageRequest(cli, mobile, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// setCaptchaHeaders 使用与账号一致的 UA：验证码服务会按 UA 区分客户端，
// 用一套浏览器 UA 反复刷滑块，比用 App UA 更容易被判成脚本。
func setCaptchaHeaders(req *http.Request, referer, ua string) {
	if strings.TrimSpace(ua) == "" {
		ua = "Mozilla/5.0 (Linux; Android 13) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Mobile Safari/537.36"
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Referer", referer)
	req.Header.Set("Accept", "*/*")
}

func extractJSONPObject(body []byte) ([]byte, error) {
	s := string(body)
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start < 0 || end < 0 || end <= start {
		return nil, errors.New("invalid jsonp")
	}
	return []byte(s[start : end+1]), nil
}

func generateCaptchaKeyAndToken(captchaIDVal, captchaTypeVal string) (string, string) {
	ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
	randomStr := randomString(32) + ts
	captchaKey := md5Hex(randomStr)
	token := md5Hex(ts + captchaIDVal + captchaTypeVal + captchaKey)
	tsNum, _ := strconv.ParseInt(ts, 10, 64)
	token = token + ":" + strconv.FormatInt(tsNum+0x493e0, 10)
	return captchaKey, token
}

func randomString(n int) string {
	const chars = "ABCDEFGHJKMNPQRSTWXYZabcdefhijkmnprstwxyz2345678"
	if n <= 0 {
		return ""
	}
	buf := make([]byte, n)
	raw := make([]byte, n)
	_, _ = rand.Read(raw)
	for i := 0; i < n; i++ {
		buf[i] = chars[int(raw[i])%len(chars)]
	}
	return string(buf)
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func detectSlideXCandidates(targetBytes, bgBytes []byte) ([]int, error) {
	targetImg, _, err := image.Decode(bytes.NewReader(targetBytes))
	if err != nil {
		return nil, err
	}
	bgImg, _, err := image.Decode(bytes.NewReader(bgBytes))
	if err != nil {
		return nil, err
	}
	tb := targetImg.Bounds()
	bb := bgImg.Bounds()
	tw, th := tb.Dx(), tb.Dy()
	bw, bh := bb.Dx(), bb.Dy()
	if tw <= 0 || th <= 0 || bw <= 0 || bh <= 0 {
		return nil, errors.New("invalid image size")
	}

	limitY := minInt(th, bh)
	type pt struct{ x, y int }
	mask := make([]pt, 0, tw*limitY/2)
	for y := 0; y < limitY; y += 2 {
		for x := 0; x < tw; x += 2 {
			_, _, _, a := targetImg.At(tb.Min.X+x, tb.Min.Y+y).RGBA()
			if a>>8 < 32 {
				continue
			}
			mask = append(mask, pt{x: x, y: y})
		}
	}
	if len(mask) < 80 {
		return nil, errors.New("captcha mask too small")
	}

	maxX := bw - tw
	if maxX <= 1 {
		return nil, errors.New("captcha width mismatch")
	}

	type match struct {
		x     int
		score float64
	}
	matches := make([]match, 0, maxX+1)
	edge := make([]float64, maxX+1)
	for offX := 0; offX <= maxX; offX++ {
		score := 0.0
		for _, p := range mask {
			tg := gray(targetImg.At(tb.Min.X+p.x, tb.Min.Y+p.y))
			bg := gray(bgImg.At(bb.Min.X+offX+p.x, bb.Min.Y+p.y))
			score += math.Abs(tg - bg)
		}
		score /= float64(len(mask))
		matches = append(matches, match{x: offX, score: score})

		es := 0.0
		for y := 0; y < limitY; y += 2 {
			l := gray(bgImg.At(bb.Min.X+offX, bb.Min.Y+y))
			r := gray(bgImg.At(bb.Min.X+offX+tw-1, bb.Min.Y+y))
			es += math.Abs(r - l)
		}
		edge[offX] = es
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].score < matches[j].score })

	tgtColEdge := make([]float64, tw)
	for x := 1; x < tw-1; x++ {
		s := 0.0
		for y := 0; y < limitY; y += 2 {
			l := gray(targetImg.At(tb.Min.X+x-1, tb.Min.Y+y))
			r := gray(targetImg.At(tb.Min.X+x+1, tb.Min.Y+y))
			s += math.Abs(r - l)
		}
		tgtColEdge[x] = s
	}
	bgColEdge := make([]float64, bw)
	for x := 1; x < bw-1; x++ {
		s := 0.0
		for y := 0; y < limitY; y += 2 {
			l := gray(bgImg.At(bb.Min.X+x-1, bb.Min.Y+y))
			r := gray(bgImg.At(bb.Min.X+x+1, bb.Min.Y+y))
			s += math.Abs(r - l)
		}
		bgColEdge[x] = s
	}
	profileMatches := make([]match, 0, maxX+1)
	for offX := 0; offX <= maxX; offX++ {
		s := 0.0
		n := 0
		for x := 1; x < tw-1; x++ {
			bx := offX + x
			if bx <= 0 || bx >= bw-1 {
				continue
			}
			s += math.Abs(tgtColEdge[x] - bgColEdge[bx])
			n++
		}
		if n == 0 {
			continue
		}
		profileMatches = append(profileMatches, match{x: offX, score: s / float64(n)})
	}
	sort.Slice(profileMatches, func(i, j int) bool { return profileMatches[i].score < profileMatches[j].score })

	edgeIdx := make([]int, 0, len(edge))
	for i := range edge {
		edgeIdx = append(edgeIdx, i)
	}
	sort.Slice(edgeIdx, func(i, j int) bool { return edge[edgeIdx[i]] > edge[edgeIdx[j]] })

	limit := minInt(12, len(matches))
	edgeLimit := minInt(8, len(edgeIdx))
	cands := make([]int, 0, 64)
	seen := make(map[int]struct{}, 64)
	add := func(v int) {
		if v < 0 || v > maxX {
			return
		}
		if _, ok := seen[v]; ok {
			return
		}
		seen[v] = struct{}{}
		cands = append(cands, v)
	}
	for i := 0; i < limit; i++ {
		base := matches[i].x
		for _, d := range []int{0, -1, 1, -2, 2, -3, 3, -5, 5, -8, 8} {
			add(base + d)
		}
	}
	profileLimit := minInt(8, len(profileMatches))
	for i := 0; i < profileLimit; i++ {
		base := profileMatches[i].x
		for _, d := range []int{0, -1, 1, -2, 2, -4, 4, -6, 6} {
			add(base + d)
		}
	}
	for i := 0; i < edgeLimit; i++ {
		base := edgeIdx[i]
		for _, d := range []int{0, -2, 2, -4, 4} {
			add(base + d)
		}
	}
	if len(cands) == 0 {
		return nil, errors.New("failed to locate slider x")
	}
	if len(cands) > 40 {
		cands = cands[:40]
	}
	return cands, nil
}

func gray(c color.Color) float64 {
	r, g, b, _ := c.RGBA()
	rf := float64(r >> 8)
	gf := float64(g >> 8)
	bf := float64(b >> 8)
	return 0.299*rf + 0.587*gf + 0.114*bf
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
