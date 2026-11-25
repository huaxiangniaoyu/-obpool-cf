// obpool — 从 Open-Box 的 sing-box 配置提取节点, 分批实测延迟与下载速度,
// 筛出前 N 名生成优选节点池 (mihomo YAML / 分享链接 / sing-box outbounds / 报告)。
//
// 设计要点:
//   - 只依赖 Go 标准库, 静态编译单文件, 目标平台 linux/arm64 (路由器) 与 amd64。
//   - 测试实例复用 Open-Box 自带的 sing-box 二进制, 独立端口/数据目录, 不碰主实例。
//   - 程序跑在路由器本机, 本机流量天然直连 (不经过 Open-Box 的 TUN), 测的是真实线路。
//   - 延迟阶段分批 (默认每批 200 个节点一个实例) 压内存; 下载阶段只测延迟前 60 名, 逐个测。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------- 配置 ----------

type Options struct {
	ConfigPath   string // 可选: Open-Box config.json (给了才提取)
	SubsFile     string // 订阅 URL 列表文件 (一行一个)
	BestCF       string // bestcf 页面 URL ("" = 不用前置组合)
	Limit        int    // 候选上限 (冒烟测试用, 0 = 不限)
	SingBoxPath  string
	WorkDir      string
	Batch        int
	Concurrency  int
	LatencyURL   string
	LatencyTO    int // ms
	Rounds       int
	Finalists    int
	Top          int
	DLBytes      int64
	DLTimeout    int // seconds
	APIAddr      string
	MixedAddr    string
	OverallLimit time.Duration
}

func defaultOptions() *Options {
	return &Options{
		ConfigPath:   "",
		BestCF:       "https://bestcf.pages.dev/",
		SingBoxPath:  "/opt/open-box/bin/sing-box",
		WorkDir:      "/opt/open-box/pool",
		Batch:        100,
		Concurrency:  10,
		LatencyURL:   "https://www.gstatic.com/generate_204",
		LatencyTO:    5000,
		Rounds:       2,
		Finalists:    60,
		Top:          20,
		DLBytes:      5_000_000,
		DLTimeout:    12,
		APIAddr:      "127.0.0.1:19095",
		MixedAddr:    "127.0.0.1:17895",
		OverallLimit: 45 * time.Minute,
	}
}

// ---------- 节点模型 (保留原始 outbound JSON 以便原样复用) ----------

type Node struct {
	Tag     string
	Type    string // vless / trojan
	Server  string
	Port    int
	Raw     map[string]any // 原始 outbound 对象
	Key     string         // 去重键 (连接身份)
	Latency int            // ms, -1 = 未测/失败
	Speed   float64        // MB/s, 下载阶段
	Alive   bool
}

func nodeKey(o map[string]any) string {
	parts := []string{
		str(o["type"]), str(o["server"]), fmt.Sprint(num(o["server_port"])),
		str(o["uuid"]), str(o["password"]),
	}
	if tls, ok := o["tls"].(map[string]any); ok {
		parts = append(parts, str(tls["server_name"]))
	}
	if tr, ok := o["transport"].(map[string]any); ok {
		parts = append(parts, str(tr["path"]))
		if h, ok := tr["headers"].(map[string]any); ok {
			parts = append(parts, str(h["Host"]))
		}
	}
	return strings.Join(parts, "|")
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func num(v any) int {
	if f, ok := v.(float64); ok {
		return int(f)
	}
	return 0
}

// 从 Open-Box config.json 提取代理节点 (vless/trojan 且带 server), 按连接身份去重。
func extractNodes(configPath string) ([]*Node, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}
	var cfg struct {
		Outbounds []map[string]any `json:"outbounds"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析 config.json: %w", err)
	}
	seen := map[string]bool{}
	var nodes []*Node
	for _, o := range cfg.Outbounds {
		t := str(o["type"])
		if t != "vless" && t != "trojan" {
			continue
		}
		if str(o["server"]) == "" {
			continue
		}
		k := nodeKey(o)
		if seen[k] {
			continue
		}
		seen[k] = true
		nodes = append(nodes, &Node{
			Tag: str(o["tag"]), Type: t, Server: str(o["server"]),
			Port: num(o["server_port"]), Raw: o, Key: k, Latency: -1,
		})
	}
	return nodes, nil
}

// ---------- sing-box 测试实例 ----------

type tester struct {
	opt     *Options
	cmd     *exec.Cmd
	client  *http.Client
	tmpDir  string
	workDir string
}

// 生成测试实例配置: 本批节点原样出站 + TEST 选择器 + clash API + mixed 入口。
func (t *tester) writeConfig(batch []*Node) (string, error) {
	outs := make([]any, 0, len(batch)+2)
	tags := make([]string, 0, len(batch))
	for _, n := range batch {
		outs = append(outs, n.Raw)
		tags = append(tags, n.Tag)
	}
	outs = append(outs, map[string]any{
		"type": "selector", "tag": "TEST", "outbounds": tags, "default": tags[0],
	})
	outs = append(outs, map[string]any{"type": "direct", "tag": "DIRECT"})
	cfg := map[string]any{
		"log":   map[string]any{"level": "error", "timestamp": false},
		"dns":   map[string]any{"servers": []any{map[string]any{"type": "local", "tag": "local"}}, "final": "local"},
		"inbounds": []any{
			map[string]any{"type": "mixed", "tag": "in", "listen": "127.0.0.1", "listen_port": portOf(t.opt.MixedAddr)},
		},
		"outbounds": outs,
		"route":     map[string]any{"final": "TEST", "auto_detect_interface": true},
		"experimental": map[string]any{
			"clash_api": map[string]any{"external_controller": t.opt.APIAddr, "secret": ""},
		},
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	p := filepath.Join(t.tmpDir, "tester.json")
	return p, os.WriteFile(p, b, 0o600)
}

func portOf(addr string) int {
	var p int
	fmt.Sscanf(addr[strings.LastIndex(addr, ":")+1:], "%d", &p)
	return p
}

func (t *tester) start(cfgPath string) error {
	dataDir := filepath.Join(t.tmpDir, "data")
	_ = os.MkdirAll(dataDir, 0o755)
	cmd := exec.Command(t.opt.SingBoxPath, "run", "-c", cfgPath, "-D", dataDir)
	cmd.Env = append(os.Environ(), "GOMEMLIMIT=160MiB", "GOGC=60")
	logFile, _ := os.Create(filepath.Join(t.tmpDir, "singbox.log"))
	if logFile != nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	t.cmd = cmd
	// 等 clash API 就绪
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := t.apiGet("/proxies"); err == nil {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("sing-box 测试实例启动超时")
}

func (t *tester) stop() {
	if t.cmd != nil && t.cmd.Process != nil {
		_ = t.cmd.Process.Kill()
		_, _ = t.cmd.Process.Wait()
		t.cmd = nil
	}
}

func (t *tester) apiGet(path string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", "http://"+t.opt.APIAddr+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("api %s: %s", path, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// 单节点延迟 (ms), 失败返回 -1。走 clash API 的 delay 接口, 与真实连接路径一致。
func (t *tester) delay(tag string) int {
	q := url.Values{}
	q.Set("url", t.opt.LatencyURL)
	q.Set("timeout", fmt.Sprint(t.opt.LatencyTO))
	body, err := t.apiGet("/proxies/" + url.PathEscape(tag) + "/delay?" + q.Encode())
	if err != nil {
		return -1
	}
	var r struct {
		Delay int `json:"delay"`
	}
	if err := json.Unmarshal(body, &r); err != nil || r.Delay <= 0 {
		return -1
	}
	return r.Delay
}

// 把 TEST 选择器切到指定节点 (下载测速前调用)。
func (t *tester) selectNode(tag string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	payload := fmt.Sprintf(`{"name":%s}`, mustJSON(tag))
	req, err := http.NewRequestWithContext(ctx, "PUT", "http://"+t.opt.APIAddr+"/proxies/TEST", strings.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 204 {
		return fmt.Errorf("select %s: %s", tag, resp.Status)
	}
	return nil
}

func mustJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// 通过 mixed 入口下载测速文件, 返回有效速度 MB/s (超时按已收字节算)。
func (t *tester) downloadSpeed() float64 {
	proxyURL, _ := url.Parse("http://" + t.opt.MixedAddr)
	client := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)},
	}
	dlURL := fmt.Sprintf("https://speed.cloudflare.com/__down?bytes=%d", t.opt.DLBytes)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(t.opt.DLTimeout)*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", dlURL, nil)
	if err != nil {
		return 0
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	n, _ := io.Copy(io.Discard, resp.Body)
	elapsed := time.Since(start).Seconds()
	if elapsed <= 0 || n < 100_000 {
		return 0
	}
	return float64(n) / elapsed / 1_000_000
}

// ---------- 延迟阶段 ----------

// 对一批节点测延迟 (多轮取平均), 结果写回 Node.Latency / Alive。
func (t *tester) measureBatch(batch []*Node) {
	sem := make(chan struct{}, t.opt.Concurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	samples := map[string][]int{}
	for _, n := range batch {
		for r := 0; r < t.opt.Rounds; r++ {
			wg.Add(1)
			sem <- struct{}{}
			go func(n *Node) {
				defer wg.Done()
				defer func() { <-sem }()
				if d := t.delay(n.Tag); d > 0 {
					mu.Lock()
					samples[n.Tag] = append(samples[n.Tag], d)
					mu.Unlock()
				}
			}(n)
		}
	}
	wg.Wait()
	for _, n := range batch {
		if s := samples[n.Tag]; len(s) > 0 {
			sum := 0
			for _, v := range s {
				sum += v
			}
			n.Latency = sum / len(s)
			n.Alive = true
		}
	}
}

// ---------- 历史 (按晚累积) ----------

type histRec struct {
	Date    string  `json:"date"`
	Key     string  `json:"key"`
	Tag     string  `json:"tag"`
	Latency int     `json:"latency"`
	Speed   float64 `json:"speed,omitempty"`
}

func loadHistory(path string) []histRec {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []histRec
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var r histRec
		if json.Unmarshal([]byte(line), &r) == nil {
			out = append(out, r)
		}
	}
	return out
}

// 每个节点在历史各晚进入当日延迟前 50 的晚数 (含今晚), 作为稳定性指标。
func stabilityNights(hist []histRec, tonight []histRec) map[string]int {
	byDate := map[string][]histRec{}
	for _, r := range append(append([]histRec{}, hist...), tonight...) {
		if r.Latency > 0 {
			byDate[r.Date] = append(byDate[r.Date], r)
		}
	}
	nights := map[string]int{}
	for _, recs := range byDate {
		sort.Slice(recs, func(i, j int) bool { return recs[i].Latency < recs[j].Latency })
		for i, r := range recs {
			if i >= 50 {
				break
			}
			nights[r.Key]++
		}
	}
	return nights
}

// ---------- 导出 ----------

func yamlQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// sing-box outbound -> mihomo proxy 条目 (仅 vless/trojan + ws + tls 形态)。
func toMihomo(n *Node) string {
	o := n.Raw
	var b strings.Builder
	b.WriteString("- name: " + yamlQuote(n.Tag) + "\n")
	b.WriteString("  type: " + n.Type + "\n")
	b.WriteString(fmt.Sprintf("  server: %s\n  port: %d\n", n.Server, n.Port))
	if n.Type == "vless" || n.Type == "vmess" {
		b.WriteString("  uuid: " + yamlQuote(str(o["uuid"])) + "\n")
		if n.Type == "vmess" {
			b.WriteString("  alterId: 0\n  cipher: auto\n")
		}
	} else {
		b.WriteString("  password: " + yamlQuote(str(o["password"])) + "\n")
	}
	b.WriteString("  udp: true\n  tls: true\n")
	tls, _ := o["tls"].(map[string]any)
	if sni := str(tls["server_name"]); sni != "" {
		b.WriteString("  servername: " + yamlQuote(sni) + "\n")
	}
	if utls, ok := tls["utls"].(map[string]any); ok && str(utls["fingerprint"]) != "" {
		b.WriteString("  client-fingerprint: " + str(utls["fingerprint"]) + "\n")
	}
	tr, _ := o["transport"].(map[string]any)
	b.WriteString("  network: ws\n  ws-opts:\n")
	b.WriteString("    path: " + yamlQuote(str(tr["path"])) + "\n")
	if h, ok := tr["headers"].(map[string]any); ok && str(h["Host"]) != "" {
		b.WriteString("    headers:\n      Host: " + yamlQuote(str(h["Host"])) + "\n")
	}
	if ed := num(tr["max_early_data"]); ed > 0 {
		b.WriteString(fmt.Sprintf("    max-early-data: %d\n", ed))
		b.WriteString("    early-data-header-name: " + yamlQuote(str(tr["early_data_header_name"])) + "\n")
	}
	return b.String()
}

// sing-box outbound -> 分享链接 (vless:// / trojan://)。
func toShareLink(n *Node) string {
	o := n.Raw
	tls, _ := o["tls"].(map[string]any)
	tr, _ := o["transport"].(map[string]any)
	host := ""
	if h, ok := tr["headers"].(map[string]any); ok {
		host = str(h["Host"])
	}
	q := url.Values{}
	q.Set("security", "tls")
	q.Set("type", "ws")
	if sni := str(tls["server_name"]); sni != "" {
		q.Set("sni", sni)
	}
	if host != "" {
		q.Set("host", host)
	}
	q.Set("path", str(tr["path"]))
	if ed := num(tr["max_early_data"]); ed > 0 {
		q.Set("eh", str(tr["early_data_header_name"]))
		q.Set("ed", fmt.Sprint(ed))
	}
	frag := url.PathEscape(n.Tag)
	if n.Type == "vless" {
		q.Set("encryption", "none")
		q.Set("fp", "chrome")
		return fmt.Sprintf("vless://%s@%s:%d?%s#%s", str(o["uuid"]), n.Server, n.Port, q.Encode(), frag)
	}
	q.Set("fp", "chrome")
	return fmt.Sprintf("trojan://%s@%s:%d?%s#%s", url.PathEscape(str(o["password"])), n.Server, n.Port, q.Encode(), frag)
}

// ---------- 主流程 ----------

func logf(format string, args ...any) {
	fmt.Printf("%s %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
}

func run(opt *Options) error {
	start := time.Now()
	var extra []*Node
	if opt.ConfigPath != "" {
		ob, err := extractNodes(opt.ConfigPath)
		if err != nil {
			return err
		}
		logf("Open-Box 配置提取节点 %d 个", len(ob))
		extra = ob
	}
	var subURLs []string
	if opt.SubsFile != "" {
		data, err := os.ReadFile(opt.SubsFile)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(data), "\n") {
			if s := strings.TrimSpace(line); s != "" && !strings.HasPrefix(s, "#") {
				subURLs = append(subURLs, s)
			}
		}
	}
	nodes, _, _ := buildCandidates(subURLs, opt.BestCF, extra)
	if opt.Limit > 0 && len(nodes) > opt.Limit {
		nodes = nodes[:opt.Limit]
		logf("候选上限生效: 只测前 %d 个", opt.Limit)
	}
	if len(nodes) == 0 {
		return fmt.Errorf("候选集为空")
	}
	if err := os.MkdirAll(opt.WorkDir, 0o755); err != nil {
		return err
	}
	tmpDir := filepath.Join(opt.WorkDir, "tmp")
	_ = os.RemoveAll(tmpDir)
	_ = os.MkdirAll(tmpDir, 0o755)
	defer os.RemoveAll(tmpDir)

	tr := &tester{opt: opt, client: &http.Client{}, tmpDir: tmpDir, workDir: opt.WorkDir}

	// 阶段 1: 分批延迟
	for i := 0; i < len(nodes); i += opt.Batch {
		end := min(i+opt.Batch, len(nodes))
		batch := nodes[i:end]
		cfgPath, err := tr.writeConfig(batch)
		if err != nil {
			return err
		}
		if err := tr.start(cfgPath); err != nil {
			return fmt.Errorf("批次 %d-%d: %w", i, end, err)
		}
		tr.measureBatch(batch)
		tr.stop()
		alive := 0
		for _, n := range batch {
			if n.Alive {
				alive++
			}
		}
		logf("延迟批次 %d-%d 完成, 存活 %d", i, end, alive)
		if time.Since(start) > opt.OverallLimit {
			return fmt.Errorf("超过总时长上限, 中止")
		}
	}

	var alive []*Node
	for _, n := range nodes {
		if n.Alive {
			alive = append(alive, n)
		}
	}
	sort.Slice(alive, func(i, j int) bool { return alive[i].Latency < alive[j].Latency })
	logf("延迟阶段完成: 存活 %d / %d", len(alive), len(nodes))
	if len(alive) == 0 {
		return fmt.Errorf("没有存活节点")
	}
	finalists := alive
	if len(finalists) > opt.Finalists {
		finalists = finalists[:opt.Finalists]
	}

	// 阶段 2: 下载测速 (逐个切换选择器)
	cfgPath, err := tr.writeConfig(finalists)
	if err != nil {
		return err
	}
	if err := tr.start(cfgPath); err != nil {
		return fmt.Errorf("下载阶段启动: %w", err)
	}
	for i, n := range finalists {
		if err := tr.selectNode(n.Tag); err != nil {
			logf("  [%d/%d] %s 切换失败: %v", i+1, len(finalists), n.Tag, err)
			continue
		}
		time.Sleep(300 * time.Millisecond)
		n.Speed = tr.downloadSpeed()
		logf("  [%d/%d] %s 延迟 %dms 下载 %.2f MB/s", i+1, len(finalists), n.Tag, n.Latency, n.Speed)
	}
	tr.stop()

	// 历史先行 (排名要用稳定性加权)
	today := time.Now().Format("2006-01-02")
	var tonight []histRec
	for _, n := range nodes {
		if n.Latency > 0 {
			tonight = append(tonight, histRec{Date: today, Key: n.Key, Tag: n.Tag, Latency: n.Latency, Speed: n.Speed})
		}
	}
	histPath := filepath.Join(opt.WorkDir, "history.jsonl")
	hist := loadHistory(histPath)
	nights := stabilityNights(hist, tonight)

	// 加权排名: 分数 = 带宽 / (1 + 延迟/200) x (1 + 0.05 x 进前50晚数, 上限5晚)
	// 有带宽成绩的排前面; 没有的按延迟补齐。分数高者入池 — 每晚重排, 优胜劣汰。
	score := func(n *Node) float64 {
		if n.Speed <= 0 {
			return 0
		}
		stable := nights[n.Key]
		if stable > 5 {
			stable = 5
		}
		return n.Speed / (1 + float64(n.Latency)/200) * (1 + 0.05*float64(stable))
	}
	var withSpeed, noSpeed []*Node
	for _, n := range finalists {
		if n.Speed > 0 {
			withSpeed = append(withSpeed, n)
		} else {
			noSpeed = append(noSpeed, n)
		}
	}
	sort.Slice(withSpeed, func(i, j int) bool { return score(withSpeed[i]) > score(withSpeed[j]) })
	pool := withSpeed
	if len(pool) < opt.Top {
		pool = append(pool, noSpeed...)
	}
	if len(pool) > opt.Top {
		pool = pool[:opt.Top]
	}
	logf("入池 %d 个", len(pool))

	// 历史落盘
	if f, err := os.OpenFile(histPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		for _, r := range tonight {
			b, _ := json.Marshal(r)
			_, _ = f.Write(append(b, '\n'))
		}
		_ = f.Close()
	}

	// 导出
	var proxiesB strings.Builder
	proxiesB.WriteString("proxies:\n")
	var linksB strings.Builder
	rawOuts := make([]any, 0, len(pool))
	names := make([]string, 0, len(pool))
	for _, n := range pool {
		proxiesB.WriteString(toMihomo(n))
		linksB.WriteString(toShareLink(n) + "\n")
		rawOuts = append(rawOuts, n.Raw)
		names = append(names, n.Tag)
	}
	_ = os.WriteFile(filepath.Join(opt.WorkDir, "pool-proxies.yaml"), []byte(proxiesB.String()), 0o644)
	// 完整 mihomo 配置: 可直接当配置文件导入
	var fullB strings.Builder
	fullB.WriteString("# 优选节点池 — obpool 每晚自动更新 (加权: 带宽+延迟+稳定性)\n")
	fullB.WriteString(fmt.Sprintf("# 更新时间: %s\n", time.Now().Format("2006-01-02 15:04:05")))
	fullB.WriteString("mixed-port: 7890\nallow-lan: false\nmode: rule\nlog-level: warning\n")
	fullB.WriteString(proxiesB.String())
	fullB.WriteString("proxy-groups:\n")
	// 全局策略 (照 Open-Box: 总开关在自动/手动/直连之间切)
	fullB.WriteString("  - name: \"优选池\"\n    type: select\n    proxies:\n      - \"优选池-自动\"\n      - \"优选池-手动\"\n      - \"DIRECT\"\n")
	fullB.WriteString("  - name: \"优选池-自动\"\n    type: url-test\n    url: https://www.gstatic.com/generate_204\n    interval: 300\n    tolerance: 50\n    proxies:\n")
	for _, nm := range names {
		fullB.WriteString("      - " + yamlQuote(nm) + "\n")
	}
	// 参照 Open-Box 的策略模型: 手动/自动两个总组 + 每服务一个四选一 selector
	// (优选池-手动 / 优选池-自动 / DIRECT / REJECT), 默认值照 Open-Box 面板现状
	fullB.WriteString("  - name: \"优选池-手动\"\n    type: select\n    proxies:\n")
	for _, nm := range names {
		fullB.WriteString("      - " + yamlQuote(nm) + "\n")
	}
	type svc struct {
		name   string
		direct bool // 默认直连 (如 Apple)
	}
	for _, s := range []svc{
		{"AI", false}, {"YouTube", false}, {"TikTok", false}, {"Netflix", false},
		{"Telegram", false}, {"Github", false}, {"Google", false}, {"Apple", true},
	} {
		fullB.WriteString("  - name: \"" + s.name + "\"\n    type: select\n    proxies:\n")
		if s.direct {
			fullB.WriteString("      - \"DIRECT\"\n      - \"优选池-手动\"\n      - \"优选池-自动\"\n      - \"REJECT\"\n")
		} else {
			fullB.WriteString("      - \"优选池-手动\"\n      - \"优选池-自动\"\n      - \"DIRECT\"\n      - \"REJECT\"\n")
		}
	}
	// 分流规则: 局域网直连 → 广告拦截 → 服务分组 → 国内直连 → 其余走优选池
	fullB.WriteString(`rules:
  - GEOSITE,private,DIRECT
  - GEOIP,private,DIRECT,no-resolve
  - GEOSITE,category-ads-all,REJECT
  - GEOSITE,openai,AI
  - GEOSITE,chatgpt,AI
  - GEOSITE,claude,AI
  - GEOSITE,google-deepmind,AI
  - GEOSITE,youtube,YouTube
  - GEOSITE,tiktok,TikTok
  - GEOSITE,netflix,Netflix
  - GEOSITE,telegram,Telegram
  - GEOIP,telegram,Telegram,no-resolve
  - GEOSITE,github,Github
  - GEOSITE,google,Google
  - GEOSITE,apple,Apple
  - GEOSITE,cn,DIRECT
  - GEOIP,CN,DIRECT,no-resolve
  - MATCH,优选池
`)
	_ = os.WriteFile(filepath.Join(opt.WorkDir, "pool.yaml"), []byte(fullB.String()), 0o644)
	_ = os.WriteFile(filepath.Join(opt.WorkDir, "pool.txt"), []byte(linksB.String()), 0o644)
	if b, err := json.MarshalIndent(map[string]any{"outbounds": rawOuts}, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(opt.WorkDir, "pool-singbox.json"), b, 0o644)
	}

	var rep strings.Builder
	rep.WriteString(fmt.Sprintf("优选节点池报告  %s\n", time.Now().Format("2006-01-02 15:04")))
	rep.WriteString(fmt.Sprintf("节点总数 %d, 延迟存活 %d, 下载入围 %d, 入池 %d, 全程 %s\n\n",
		len(nodes), len(alive), len(finalists), len(pool), time.Since(start).Round(time.Second)))
	rep.WriteString(fmt.Sprintf("%-4s %-34s %8s %10s %s\n", "#", "节点", "延迟ms", "下载MB/s", "进前50晚数"))
	for i, n := range pool {
		name := n.Tag
		if len([]rune(name)) > 32 {
			name = string([]rune(name)[:32])
		}
		rep.WriteString(fmt.Sprintf("%-4d %-34s %8d %10.2f %d\n", i+1, name, n.Latency, n.Speed, nights[n.Key]))
	}
	_ = os.WriteFile(filepath.Join(opt.WorkDir, "report.txt"), []byte(rep.String()), 0o644)
	logf("完成, 报告与池子已写入 %s", opt.WorkDir)
	return nil
}

func main() {
	opt := defaultOptions()
	flag.StringVar(&opt.ConfigPath, "config", opt.ConfigPath, "可选: Open-Box config.json 路径")
	flag.StringVar(&opt.SubsFile, "subs", opt.SubsFile, "订阅 URL 列表文件 (一行一个)")
	flag.StringVar(&opt.BestCF, "bestcf", opt.BestCF, "bestcf 页面 URL (空 = 不做前置组合)")
	flag.IntVar(&opt.Limit, "limit", opt.Limit, "候选上限 (冒烟测试用)")
	flag.StringVar(&opt.SingBoxPath, "singbox", opt.SingBoxPath, "sing-box 二进制路径")
	flag.StringVar(&opt.WorkDir, "workdir", opt.WorkDir, "输出目录")
	flag.IntVar(&opt.Batch, "batch", opt.Batch, "每批延迟测试节点数")
	flag.IntVar(&opt.Concurrency, "conc", opt.Concurrency, "延迟并发")
	flag.IntVar(&opt.Finalists, "finalists", opt.Finalists, "进入下载测速的数量")
	flag.IntVar(&opt.Top, "top", opt.Top, "入池数量")
	flag.Parse()
	if err := run(opt); err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		os.Exit(1)
	}
}
