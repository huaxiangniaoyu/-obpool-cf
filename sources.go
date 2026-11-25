package main

// sources.go — 多源候选构建:
//   1. 订阅链接 (vless/trojan/vmess 分享链接, 明文或 base64) -> sing-box outbounds
//   2. bestcf.pages.dev 页面 -> 优选前置域名 (地区子域名 + cf.* 优选域名)
//   3. 端点 x 前置 组合: 每个唯一 Worker 端点再乘上每个 bestcf 前置, 扩大候选面

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func fetchText(rawURL string) (string, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(3 * time.Second)
		}
		body, err := fetchOnce(rawURL)
		if err == nil {
			return body, nil
		}
		lastErr = err
	}
	return "", lastErr
}

func fetchOnce(rawURL string) (string, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; obpool/2.0)")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("%s -> %s", rawURL, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return string(b), err
}

// 订阅体可能是 base64 包着的链接列表; 识别后解码。
func decodeSubBody(body string) string {
	trimmed := strings.TrimSpace(body)
	if strings.Contains(trimmed, "://") {
		return trimmed
	}
	compact := strings.Join(strings.Fields(trimmed), "")
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if dec, err := enc.DecodeString(compact); err == nil && strings.Contains(string(dec), "://") {
			return string(dec)
		}
	}
	return trimmed
}

// 一条分享链接 -> sing-box outbound 对象 (map)。不支持的形态返回 nil。
func parseShareLink(line string) map[string]any {
	line = strings.TrimSpace(line)
	switch {
	case strings.HasPrefix(line, "vless://"):
		return parseVLESS(line)
	case strings.HasPrefix(line, "trojan://"):
		return parseTrojan(line)
	case strings.HasPrefix(line, "vmess://"):
		return parseVMess(line)
	}
	return nil
}

// ws 传输: 处理 path 里内嵌的 ?ed=2048 早数据写法 (joey 系链接的惯例)。
func wsTransport(q url.Values) map[string]any {
	path := q.Get("path")
	edInline := 0
	if i := strings.Index(path, "?ed="); i >= 0 {
		if n, err := strconv.Atoi(strings.TrimPrefix(path[i:], "?ed=")); err == nil {
			edInline = n
		}
		path = path[:i]
	}
	if path == "" {
		path = "/"
	}
	tr := map[string]any{"type": "ws", "path": path}
	if host := q.Get("host"); host != "" {
		tr["headers"] = map[string]any{"Host": host}
	}
	if ed := q.Get("ed"); ed != "" {
		if n, err := strconv.Atoi(ed); err == nil {
			tr["max_early_data"] = n
			eh := q.Get("eh")
			if eh == "" {
				eh = "Sec-WebSocket-Protocol"
			}
			tr["early_data_header_name"] = eh
		}
	} else if edInline > 0 {
		tr["max_early_data"] = edInline
		tr["early_data_header_name"] = "Sec-WebSocket-Protocol"
	}
	return tr
}

func tlsBlock(q url.Values) map[string]any {
	sni := q.Get("sni")
	if sni == "" {
		sni = q.Get("peer")
	}
	tls := map[string]any{
		"enabled":     true,
		"server_name": sni,
		"insecure":    q.Get("allowInsecure") == "1" || q.Get("insecure") == "1",
	}
	fp := q.Get("fp")
	if fp == "" {
		fp = "chrome"
	}
	tls["utls"] = map[string]any{"enabled": true, "fingerprint": fp}
	if q.Get("security") == "reality" {
		tls["reality"] = map[string]any{
			"enabled":    true,
			"public_key": q.Get("pbk"),
			"short_id":   q.Get("sid"),
		}
	}
	if alpn := q.Get("alpn"); alpn != "" {
		tls["alpn"] = strings.Split(alpn, ",")
	}
	return tls
}

func transportBlock(q url.Values) map[string]any {
	switch q.Get("type") {
	case "ws":
		return wsTransport(q)
	case "grpc":
		return map[string]any{"type": "grpc", "service_name": q.Get("serviceName")}
	}
	return nil
}

func parseVLESS(line string) map[string]any {
	u, err := url.Parse(line)
	if err != nil || u.User == nil {
		return nil
	}
	q := u.Query()
	port, _ := strconv.Atoi(u.Port())
	if port == 0 {
		port = 443
	}
	o := map[string]any{
		"type": "vless", "tag": u.Fragment,
		"server": u.Hostname(), "server_port": port,
		"uuid": u.User.Username(),
	}
	if q.Get("security") == "tls" || q.Get("security") == "reality" {
		o["tls"] = tlsBlock(q)
	}
	if tr := transportBlock(q); tr != nil {
		o["transport"] = tr
	}
	if flow := q.Get("flow"); flow != "" {
		o["flow"] = flow
	}
	return o
}

func parseTrojan(line string) map[string]any {
	u, err := url.Parse(line)
	if err != nil || u.User == nil {
		return nil
	}
	q := u.Query()
	port, _ := strconv.Atoi(u.Port())
	if port == 0 {
		port = 443
	}
	o := map[string]any{
		"type": "trojan", "tag": u.Fragment,
		"server": u.Hostname(), "server_port": port,
		"password": u.User.Username(),
		"tls":      tlsBlock(q),
	}
	if tr := transportBlock(q); tr != nil {
		o["transport"] = tr
	}
	return o
}

func parseVMess(line string) map[string]any {
	raw := strings.TrimPrefix(line, "vmess://")
	dec, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		if dec2, err2 := base64.RawStdEncoding.DecodeString(raw); err2 == nil {
			dec = dec2
		} else {
			return nil
		}
	}
	var v map[string]any
	if json.Unmarshal(dec, &v) != nil {
		return nil
	}
	port := num(v["port"])
	if port == 0 {
		port = 443
	}
	o := map[string]any{
		"type": "vmess", "tag": str(v["ps"]),
		"server": str(v["add"]), "server_port": port,
		"uuid": str(v["id"]), "security": "auto", "alter_id": num(v["aid"]),
	}
	if str(v["tls"]) == "tls" {
		o["tls"] = map[string]any{
			"enabled": true, "server_name": str(v["sni"]),
			"utls": map[string]any{"enabled": true, "fingerprint": "chrome"},
		}
	}
	if str(v["net"]) == "ws" {
		tr := map[string]any{"type": "ws", "path": str(v["path"])}
		if str(v["path"]) == "" {
			tr["path"] = "/"
		}
		if str(v["host"]) != "" {
			tr["headers"] = map[string]any{"Host": str(v["host"])}
		}
		o["transport"] = tr
	}
	return o
}

// 解析一个订阅体为节点列表 (来源名仅用于日志)。
func parseSubscription(body, source string) []*Node {
	body = decodeSubBody(body)
	if strings.Contains(body, "proxies:") {
		return parseYAMLSubscription(body, source)
	}
	var nodes []*Node
	for _, line := range strings.Split(body, "\n") {
		o := parseShareLink(line)
		if o == nil {
			continue
		}
		if str(o["server"]) == "" {
			continue
		}
		t := str(o["type"])
		if t != "vless" && t != "trojan" && t != "vmess" {
			continue
		}
		nodes = append(nodes, &Node{
			Tag: str(o["tag"]), Type: t, Server: str(o["server"]),
			Port: num(o["server_port"]), Raw: o, Latency: -1,
		})
	}
	logf("来源 %s: 解析出 %d 个节点", source, len(nodes))
	return nodes
}

// 从 bestcf 导航页提取优选前置域名: *.bestcf.eu.cc 地区域名 + cf.* 优选域名。
var domainRe = regexp.MustCompile(`\b(?:[a-zA-Z0-9-]+\.)+(?:xyz|top|com|net|io|dev|site|online|vip|cloud|org|cc|me|pro|fun|icu|shop|tech)\b`)

func parseBestCFFronts(html string) []string {
	skipSuffix := []string{"cloudflare.com", "cloudflarestatus.com", "cloudflare.dev", "cloudflareclient.com", "jsdelivr.net", "github.com", "google.com", "pages.dev", "cloudflare-cn.com"}
	seen := map[string]bool{}
	var fronts []string
	for _, d := range domainRe.FindAllString(html, -1) {
		d = strings.ToLower(d)
		skip := false
		for _, s := range skipSuffix {
			if d == s || strings.HasSuffix(d, "."+s) {
				skip = true
				break
			}
		}
		if skip || seen[d] {
			continue
		}
		first := strings.Split(d, ".")[0]
		if strings.Contains(first, "cf") || strings.HasSuffix(d, "bestcf.eu.cc") {
			seen[d] = true
			fronts = append(fronts, d)
		}
	}
	sortStrings(fronts)
	if len(fronts) > 48 {
		fronts = fronts[:48]
	}
	return fronts
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// 端点身份 (与前置无关的部分): 协议+凭据+SNI+Host+路径+端口。
func endpointKey(o map[string]any) string {
	parts := []string{str(o["type"]), str(o["uuid"]), str(o["password"])}
	if tls, ok := o["tls"].(map[string]any); ok {
		parts = append(parts, str(tls["server_name"]))
	}
	if tr, ok := o["transport"].(map[string]any); ok {
		parts = append(parts, str(tr["type"]), str(tr["path"]), str(tr["service_name"]))
		if h, ok := tr["headers"].(map[string]any); ok {
			parts = append(parts, str(h["Host"]))
		}
	}
	parts = append(parts, fmt.Sprint(num(o["server_port"])))
	return strings.Join(parts, "|")
}

func cloneMap(o map[string]any) map[string]any {
	b, _ := json.Marshal(o)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

// 构建候选集: 订阅原节点 (按连接身份去重) + 端点 x bestcf 前置 的新组合。
// 返回 (候选, 端点数, 前置数)。
func buildCandidates(subURLs []string, bestcfURL string, extra []*Node) ([]*Node, int, int) {
	var all []*Node
	all = append(all, extra...)
	for _, u := range subURLs {
		body, err := fetchText(u)
		if err != nil {
			logf("拉取订阅失败 %s: %v", maskURL(u), err)
			continue
		}
		all = append(all, parseSubscription(body, maskURL(u))...)
	}

	// 去重 (连接身份) + 标签唯一化
	seen := map[string]bool{}
	usedTags := map[string]bool{}
	var nodes []*Node
	endpoints := map[string]*Node{}
	for _, n := range all {
		n.Key = nodeKey(n.Raw)
		if seen[n.Key] {
			continue
		}
		seen[n.Key] = true
		tag := n.Tag
		if tag == "" {
			tag = n.Server
		}
		base := tag
		for i := 2; usedTags[tag]; i++ {
			tag = fmt.Sprintf("%s #%d", base, i)
		}
		usedTags[tag] = true
		n.Tag = tag
		n.Raw["tag"] = tag
		nodes = append(nodes, n)
		ek := endpointKey(n.Raw)
		if _, ok := endpoints[ek]; !ok {
			endpoints[ek] = n
		}
	}
	logf("订阅原节点去重后 %d 个, 唯一端点 %d 个", len(nodes), len(endpoints))

	// bestcf 前置组合
	var fronts []string
	if bestcfURL != "" {
		if html, err := fetchText(bestcfURL); err == nil {
			fronts = parseBestCFFronts(html)
			logf("bestcf 前置域名 %d 个", len(fronts))
		} else {
			logf("拉取 bestcf 失败: %v", err)
		}
	}
	combos := 0
	for _, ep := range endpoints {
		sni := ""
		if tls, ok := ep.Raw["tls"].(map[string]any); ok {
			sni = str(tls["server_name"])
		}
		for _, f := range fronts {
			if f == ep.Server || f == sni {
				continue
			}
			raw := cloneMap(ep.Raw)
			raw["server"] = f
			tag := ep.Tag + " @" + f
			if usedTags[tag] {
				continue
			}
			raw["tag"] = tag
			nn := &Node{Tag: tag, Type: ep.Type, Server: f, Port: ep.Port, Raw: raw, Latency: -1}
			nn.Key = nodeKey(raw)
			if seen[nn.Key] {
				continue
			}
			seen[nn.Key] = true
			usedTags[tag] = true
			nodes = append(nodes, nn)
			combos++
		}
	}
	logf("前置组合新增 %d 个, 候选总数 %d", combos, len(nodes))
	return nodes, len(endpoints), len(fronts)
}

// 日志里隐去订阅 URL 的令牌部分。
func maskURL(u string) string {
	parsed, err := url.Parse(u)
	if err != nil {
		return u
	}
	if parsed.RawQuery != "" {
		parsed.RawQuery = "token=***"
	}
	parts := strings.Split(parsed.Path, "/")
	for i, p := range parts {
		if len(p) >= 24 { // UUID/令牌样子的路径段
			parts[i] = p[:6] + "***"
		}
	}
	parsed.Path = strings.Join(parts, "/")
	return parsed.String()
}

// ---------- mihomo YAML 订阅解析 (CFNext 等按 UA 返回这种格式) ----------

// 行式解析 proxies 列表: 只取 ws/tcp 的 vless/trojan/vmess, xhttp 等跳过。
func parseYAMLSubscription(body, source string) []*Node {
	var nodes []*Node
	skipped := 0
	var cur map[string]string

	flush := func() {
		if cur == nil {
			return
		}
		defer func() { cur = nil }()
		t := cur["type"]
		if t != "vless" && t != "trojan" && t != "vmess" {
			skipped++
			return
		}
		net := cur["network"]
		if net != "" && net != "ws" && net != "tcp" {
			skipped++
			return
		}
		port, _ := strconv.Atoi(cur["port"])
		if port == 0 {
			port = 443
		}
		o := map[string]any{
			"type": t, "tag": cur["name"],
			"server": cur["server"], "server_port": port,
		}
		if t == "trojan" {
			o["password"] = cur["password"]
		} else {
			o["uuid"] = cur["uuid"]
		}
		if t == "vmess" {
			o["security"] = "auto"
			o["alter_id"] = 0
		}
		if cur["tls"] != "false" {
			fp := cur["client-fingerprint"]
			if fp == "" {
				fp = "chrome"
			}
			sni := cur["servername"]
			if sni == "" {
				sni = cur["sni"]
			}
			o["tls"] = map[string]any{
				"enabled": true, "server_name": sni,
				"insecure": cur["skip-cert-verify"] == "true",
				"utls":     map[string]any{"enabled": true, "fingerprint": fp},
			}
		}
		if net == "ws" {
			path := cur["path"]
			if path == "" {
				path = "/"
			}
			tr := map[string]any{"type": "ws", "path": path}
			if cur["host"] != "" {
				tr["headers"] = map[string]any{"Host": cur["host"]}
			}
			o["transport"] = tr
		}
		if str(o["server"]) == "" {
			skipped++
			return
		}
		nodes = append(nodes, &Node{
			Tag: str(o["tag"]), Type: t, Server: str(o["server"]),
			Port: num(o["server_port"]), Raw: o, Latency: -1,
		})
	}

	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "- name:") || strings.HasPrefix(trimmed, "-  name:") {
			flush()
			cur = map[string]string{}
			cur["name"] = yamlScalar(strings.TrimSpace(strings.SplitN(trimmed, ":", 2)[1]))
			continue
		}
		if cur == nil {
			continue
		}
		i := strings.Index(trimmed, ":")
		if i <= 0 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(trimmed[:i]))
		val := yamlScalar(strings.TrimSpace(trimmed[i+1:]))
		switch key {
		case "name", "type", "server", "port", "uuid", "password", "network",
			"servername", "sni", "client-fingerprint", "tls", "skip-cert-verify", "path":
			if cur[key] == "" {
				cur[key] = val
			}
		case "host":
			cur["host"] = val
		}
	}
	flush()
	logf("来源 %s: YAML 解析出 %d 个节点 (跳过 %d)", source, len(nodes), skipped)
	return nodes
}

func yamlScalar(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}
