package persona

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// visionFetchTimeout 是一批图片下载共用的时间上限。
const visionFetchTimeout = 10 * time.Second

// newVisionClient 由注入的 HTTP 客户端派生图片下载客户端。
//
// transport 优先克隆注入客户端的 *http.Transport，否则克隆 http.DefaultTransport；
// 两种情况下都替换 DialContext 为地址限制拨号器，并清空 Proxy（否则经代理可绕过限制）。
// 不设 Client.Timeout：超时由每次请求的 ctx 控制（visionFetchTimeout）。
func newVisionClient(base *http.Client) *http.Client {
	tr := &http.Transport{}
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		tr = t.Clone()
	}
	if base != nil {
		if t, ok := base.Transport.(*http.Transport); ok && t != nil {
			tr = t.Clone()
		}
	}
	tr.Proxy = nil
	tr.DialContext = dialVisionSafe
	// 宿主自定义的 DialTLS/DialTLSContext 会绕过拨号地址校验，图片下载一律
	// 走受限的 DialContext。
	tr.DialTLS = nil //nolint:staticcheck // 仍使用已弃用 DialTLS 的宿主客户端同样不能绕过地址校验
	tr.DialTLSContext = nil
	return &http.Client{Transport: tr}
}

// blockedVisionPrefixes 是 netip 方法未覆盖的特殊用途地址段（CGNAT、保留、
// 文档、NAT64/6to4 等）。图片 URL 来自聊天消息，必须按不可信输入处理，
// 否则可用 100.100.100.200 一类地址访问云元数据服务。
var blockedVisionPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // 本网络
	netip.MustParsePrefix("100.64.0.0/10"),   // CGNAT，含部分云元数据地址
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF 协议分配
	netip.MustParsePrefix("192.0.2.0/24"),    // TEST-NET-1
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmark
	netip.MustParsePrefix("198.51.100.0/24"), // TEST-NET-2
	netip.MustParsePrefix("203.0.113.0/24"),  // TEST-NET-3
	netip.MustParsePrefix("240.0.0.0/4"),     // 保留，含 255.255.255.255
	netip.MustParsePrefix("100::/64"),        // discard-only
	netip.MustParsePrefix("2001::/32"),       // Teredo
	netip.MustParsePrefix("2001:2::/48"),     // benchmark
	netip.MustParsePrefix("2001:db8::/32"),   // 文档
	netip.MustParsePrefix("2002::/16"),       // 6to4
	netip.MustParsePrefix("64:ff9b::/96"),    // NAT64
}

// blockedVisionAddr 判定地址是否禁止访问：回环、私网、链路本地单播/组播、
// 接口本地组播、组播、未指定、非法地址及特殊用途段一律拒绝。
func blockedVisionAddr(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() ||
		a.IsLinkLocalMulticast() || a.IsInterfaceLocalMulticast() || a.IsMulticast() || a.IsUnspecified() {
		return true
	}
	for _, p := range blockedVisionPrefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// dialVisionSafe 解析主机名并逐个校验解析结果，只拨号到通过校验的地址。
//
// 校验发生在拨号时刻（而非 URL 解析时刻），因此重定向目标同样受限。
func dialVisionSafe(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("persona: 图片地址非法: %w", err)
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("persona: 图片地址解析失败: %w", err)
	}
	if len(addrs) == 0 {
		return nil, errors.New("persona: 图片地址无解析结果")
	}
	for _, a := range addrs {
		if blockedVisionAddr(a) {
			return nil, fmt.Errorf("persona: 拒绝访问的图片地址 %s", a.Unmap())
		}
	}
	var d net.Dialer
	return d.DialContext(ctx, network, net.JoinHostPort(addrs[0].Unmap().String(), port))
}

// dataURL 把图片字节编码为 data URL。
func dataURL(mime string, data []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// mediaType 取 MIME 的类型部分（去掉 "; charset=..." 等参数）。
func mediaType(v string) string {
	if i := strings.IndexByte(v, ';'); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

// imageFormat 归一化图片格式名：去参数与 "image/" 前缀、转小写、jpg 归一到 jpeg。
// 配置项与响应 MIME 都走这一条归一，故 jpeg/JPEG/image/jpeg/jpg 等价。
func imageFormat(v string) string {
	f := strings.ToLower(mediaType(strings.TrimSpace(v)))
	f = strings.TrimPrefix(f, "image/")
	if f == "jpg" {
		f = "jpeg"
	}
	return f
}

// formatSet 把配置中的格式名归一为集合；归一后为空时返回 nil（nil 表示不过滤）。
func formatSet(list []string) map[string]bool {
	out := make(map[string]bool, len(list))
	for _, v := range list {
		if f := imageFormat(v); f != "" {
			out[f] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// debugSkipImage 记录一张被丢弃的图片；attrs 含 reason、host，fetch_error 时含 err。
func (p *Plugin) debugSkipImage(raw, reason string, err error) {
	attrs := []any{"reason", reason, "host", hostOf(raw)}
	if err != nil {
		attrs = append(attrs, "err", err)
	}
	p.log.Debug("persona 图片跳过", attrs...)
}

// fetchImageDataURLs 按序下载图片并返回等长结果：成功为 data URL，
// 失败/超限/非图片/格式不在白名单的位置为 ""。urls 为空时返回 nil（不建 ctx、不发请求）。
func (p *Plugin) fetchImageDataURLs(ctx context.Context, urls []string) []string {
	if len(urls) == 0 {
		return nil
	}
	fctx, cancel := context.WithTimeout(ctx, visionFetchTimeout)
	defer cancel()

	out := make([]string, len(urls))
	for i, raw := range urls {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			p.debugSkipImage(raw, "bad_url", nil)
			continue
		}
		req, err := http.NewRequestWithContext(fctx, http.MethodGet, raw, nil)
		if err != nil {
			p.debugSkipImage(raw, "bad_url", err)
			continue
		}
		resp, err := p.imgClient.Do(req)
		if err != nil {
			p.debugSkipImage(raw, "fetch_error", err)
			continue
		}
		data, rerr := io.ReadAll(io.LimitReader(resp.Body, int64(p.cfg.llmVisionMaxImageBytes)+1))
		_ = resp.Body.Close()
		if rerr != nil {
			p.debugSkipImage(raw, "fetch_error", rerr)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			p.debugSkipImage(raw, "status", nil)
			continue
		}
		if int64(len(data)) > int64(p.cfg.llmVisionMaxImageBytes) {
			p.debugSkipImage(raw, "too_large", nil)
			continue
		}
		mime := mediaType(resp.Header.Get("Content-Type"))
		if !strings.HasPrefix(mime, "image/") {
			mime = mediaType(http.DetectContentType(data))
		}
		if !strings.HasPrefix(mime, "image/") {
			p.debugSkipImage(raw, "not_image", nil)
			continue
		}
		if p.cfg.llmVisionAllowedFormats != nil && !p.cfg.llmVisionAllowedFormats[imageFormat(mime)] {
			p.debugSkipImage(raw, "format", nil)
			continue
		}
		out[i] = dataURL(mime, data)
	}
	return out
}

// fetchVisionSlots 下载所有非空槽位并写回原槽位；下载失败的槽位保持空串（渲染时回落 "[图片]"）。
// 无任何非空槽位时原样返回（不建 ctx、不发请求）。整批共用 fetchImageDataURLs 的 10s 上限。
func (p *Plugin) fetchVisionSlots(ctx context.Context, slots [][]string) [][]string {
	var urls []string
	type pos struct{ i, j int }
	var poss []pos
	for i, row := range slots {
		for j, u := range row {
			if u != "" {
				urls = append(urls, u)
				poss = append(poss, pos{i, j})
			}
		}
	}
	if len(urls) == 0 {
		return slots
	}
	got := p.fetchImageDataURLs(ctx, urls)
	for k, pp := range poss {
		slots[pp.i][pp.j] = got[k]
	}
	return slots
}
