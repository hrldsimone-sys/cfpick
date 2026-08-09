// cfpick — 为 x-tunnel 挑选最优 Cloudflare 边缘 IP(经 BaiduTunnel 实测)
// 用法:
//
//	cfpick -domain jpx.zqsg.eu.org [-web http://127.0.0.1:8080] [-pass 123456] [-keep 10] [-rtt 150] [-auto]
package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type opts struct {
	domain string
	web    string
	pass   string
	keep   int
	rttMax int
	max    int // 扫描候选上限
	auto   bool
	con    int // 并发数
}

type rule struct {
	ID, Name, Listen, Target, NodeID string
}

type testResult struct {
	ip        string
	rttMs     int64
	hsMs      int64
	speedMBps float64
	ok        bool
}

var (
	reNodeID  = regexp.MustCompile(`(?s)name="nodeId"[^>]*>\s*<option value="([^"]+)"`)
	rePickID  = regexp.MustCompile(`(?s)name="id" value="([^"]+)".*?name="name" value="(pick-[^"]+)"`)
	reRuleRow = regexp.MustCompile(`(?s)<form method="post" action="/forward/update".*?</form>`)
	reIDv     = regexp.MustCompile(`name="id" value="([^"]+)"`)
	reNamev   = regexp.MustCompile(`name="name" value="([^"]*)"`)
	reListenv = regexp.MustCompile(`name="listen" value="([^"]*)"`)
	reTargetv = regexp.MustCompile(`name="target" value="([^"]*)"`)
)

func main() {
	var o opts
	flag.StringVar(&o.domain, "domain", "", "必填:x-tunnel 服务端域名(Cloudflare 前置),如 jpx.zqsg.eu.org")
	flag.StringVar(&o.web, "web", "http://127.0.0.1:8080", "BaiduTunnel 管理后台地址")
	flag.StringVar(&o.pass, "pass", "123456", "BaiduTunnel 后台密码")
	flag.IntVar(&o.keep, "keep", 10, "输出多少个 IP")
	flag.IntVar(&o.rttMax, "rtt", 150, "粗筛阈值(ms): TCP 443 直连 RTT 超过则淘汰")
	flag.IntVar(&o.max, "scan", 384, "扫描候选上限(邻居段内)")
	flag.IntVar(&o.con, "con", 6, "隧道并发测试数")
	flag.BoolVar(&o.auto, "auto", false, "测完后把 TOP N 写进 BaiduTunnel 转发规则")
	flag.Parse()
	if o.domain == "" {
		fmt.Println("❌ 必须指定 -domain (x-tunnel 域名)")
		flag.Usage()
		return
	}
	start := time.Now()
	fmt.Printf("▶ 域名: %s | 后台: %s | 阈值: %dms | 需求: %d 个\n\n", o.domain, o.web, o.rttMax, o.keep)

	// ① 解析域名 → 种子 IP
	seeds := resolveSeeds(o.domain)
	if len(seeds) == 0 {
		fmt.Println("❌ 域名解析失败:", o.domain)
		return
	}
	fmt.Printf("① 域名解析到 %d 个种子 IP: %s\n", len(seeds), join(seeds))

	// ② 邻居段并发粗筛
	cands := scanNeighbors(seeds, o.rttMax, o.max, 48)
	if len(cands) == 0 {
		fmt.Println("❌ 粗筛后无候选(阈值太严?或网络不通)")
		return
	}
	fmt.Printf("② 邻居段粗筛(≤%dms): %d 个\n", o.rttMax, len(cands))

	// ③ 登录后台 + 加规则 + 隧道实测
	admin := newAdmin(o.web, o.pass)
	if err := admin.login(); err != nil {
		fmt.Println("❌ 登录 BaiduTunnel 失败:", err)
		return
	}
	fmt.Printf("③ 后台登录成功\n")
	results := admin.tunnelTest(cands, o.domain, o.con)
	results = filterOK(results)
	if len(results) == 0 {
		fmt.Println("❌ 隧道实测全部失败(百度边缘异常? 或 SNI 不可达)")
		admin.cleanup()
		return
	}
	admin.cleanup()

	// ④ 排序输出
	sort.SliceStable(results, func(a, b int) bool {
		ra, rb := results[a], results[b]
		if ra.speedMBps != rb.speedMBps {
			return ra.speedMBps > rb.speedMBps
		}
		return ra.hsMs < rb.hsMs
	})
	n := len(results)
	if n > o.keep { n = o.keep }
	fmt.Printf("\n④ TOP %d(按速度排序,含握手延迟):\n", n)
	for _, r := range results[:n] {
		fmt.Printf("   %-16s 握手%4dms | %5.2f MB/s\n", r.ip, r.hsMs, r.speedMBps)
	}

	if o.auto {
		n := admin.apply(results)
		fmt.Printf("✔ 已应用 %d 条转发规则\n", n)
	}
	fmt.Printf("\n耗时 %s ✓\n", time.Since(start).Round(time.Millisecond))
}

// ---------- ① 域名解析 ----------
func resolveSeeds(domain string) []string {
	addrs, err := net.LookupHost(domain)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, a := range addrs {
		if !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	return out
}

// ---------- ② 邻居段粗筛 ----------
func scanNeighbors(seeds []string, rttMax, cap, threads int) []string {
	seen := map[string]bool{}
	var cands []string
	for _, s := range seeds {
		sp := strings.Split(s, ".")
		if len(sp) != 4 {
			continue
		}
		prefix := sp[0] + "." + sp[1] + "." + sp[2] + "."
		for i := 1; i < 256; i++ {
			ip := prefix + strconv.Itoa(i)
			if !seen[ip] {
				seen[ip] = true
				cands = append(cands, ip)
			}
		}
	}
	if len(cands) > cap {
		cands = cands[:cap]
	}

	var mu sync.Mutex
	var good []string
	var wg sync.WaitGroup
	sem := make(chan struct{}, threads)
	for _, ip := range cands {
		wg.Add(1)
		sem <- struct{}{}
		go func(ip string) {
			defer wg.Done()
			defer func() { <-sem }()
			if tcpRTT(ip, 443, 4*time.Second) != -1 {
				t := tcpRTTms(ip, 443, 4*time.Second)
				if t <= int64(rttMax) {
					mu.Lock()
					good = append(good, ip)
					mu.Unlock()
				}
			}
		}(ip)
	}
	wg.Wait()
	sort.Strings(good)
	return good
}

func tcpRTT(host string, port int, to time.Duration) int64 {
	t0 := time.Now()
	c, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), to)
	if err != nil {
		return -1
	}
	c.Close()
	return time.Since(t0).Milliseconds()
}
func tcpRTTms(host string, port int, to time.Duration) int64 { return tcpRTT(host, port, to) }

// ---------- ③ BaiduTunnel 管理客户端 ----------
type admin struct {
	base string
	pass string
	hc   *http.Client
	ids  []string
}

func newAdmin(base, pass string) *admin {
	jar, _ := cookiejar.New(nil)
	return &admin{base: base, pass: pass, hc: &http.Client{
		Jar: jar, Timeout: 15 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse // 不自动跟随 302,登录/表单全靠 302 判断
		},
	}}
}
func (a *admin) post(path string, form map[string]string) (int, error) {
	f := url.Values{}
	for k, v := range form {
		f.Set(k, v)
	}
	resp, err := a.hc.PostForm(a.base+path, f)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}
func (a *admin) get(page string) (string, error) {
	resp, err := a.hc.Get(a.base + page)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}
func (a *admin) login() error {
	code, err := a.post("/login", map[string]string{"password": a.pass})
	if err != nil || code != 302 {
		if err == nil {
			err = fmt.Errorf("HTTP %d", code)
		}
		return fmt.Errorf("登录失败: %w", err)
	}
	return nil
}
func (a *admin) nodeID() string {
	html, err := a.get("/")
	if err != nil {
		return ""
	}
	m := reNodeID.FindStringSubmatch(html)
	if len(m) > 1 {
		return m[1]
	}
	return ""
}
func (a *admin) tunnelTest(cands []string, domain string, _ int) []testResult {
	node := a.nodeID()
	if node == "" {
		fmt.Println("⚠️ 未取到节点ID,使用默认")
		node = "w0E6oWAtGam2"
	}
	// 加规则
	basePort := 20310
	ports := make(map[string]int, len(cands))
	for i, ip := range cands {
		port := basePort + i
		ports[ip] = port
		a.post("/forward/add", map[string]string{
			"name": "pick-" + ip, "listen": strconv.Itoa(port),
			"target": ip + ":443", "nodeId": node, "enabled": "true",
		})
	}
	// 页面取规则 id + 启用
	html, _ := a.get("/")
	for _, m := range rePickID.FindAllStringSubmatch(html, -1) {
		a.ids = append(a.ids, m[1])
		a.post("/forward/toggle", map[string]string{"id": m[1], "enabled": "true"})
	}
	time.Sleep(1 * time.Second)

	// 并发隧道实测
	var mu sync.Mutex
	var out []testResult
	var wg sync.WaitGroup
	sem := make(chan struct{}, 6)
	for _, ip := range cands {
		wg.Add(1)
		sem <- struct{}{}
		go func(ip string) {
			defer wg.Done()
			defer func() { <-sem }()
			r := testOne("127.0.0.1", ports[ip], domain)
			r.ip = ip
			mu.Lock()
			out = append(out, r)
			mu.Unlock()
		}(ip)
	}
	wg.Wait()
	return out
}

// testOne: SNI=domain 握手(判据) + speed.cloudflare.com 5MB 测速
func testOne(host string, port int, domain string) testResult {
	var r testResult
	dial := func() (net.Conn, error) {
		return net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 10*time.Second)
	}
	// 1) SNI 握手
	t0 := time.Now()
	c, err := tlsHandshake(dial, domain, 8*time.Second)
	if err != nil {
		return r
	}
	r.hsMs = time.Since(t0).Milliseconds()
	c.Close()
	// 2) 5MB 测速
	c2, err := tlsHandshake(dial, "speed.cloudflare.com", 10*time.Second)
	if err != nil {
		return r
	}
	defer c2.Close()
	c2.SetDeadline(time.Now().Add(20 * time.Second))
	req := "GET /__down?bytes=5242880 HTTP/1.1\r\nHost: speed.cloudflare.com\r\nConnection: close\r\n\r\n"
	if _, err := c2.Write([]byte(req)); err != nil {
		return r
	}
	t1 := time.Now()
	var n int64
	buf := make([]byte, 64<<10)
	for {
		k, err := c2.Read(buf)
		n += int64(k)
		if err != nil {
			break
		}
	}
	dt := time.Since(t1).Seconds()
	r.ok = n > 1<<20 // 至少 1MB 才算有效
	if dt > 0 {
		r.speedMBps = float64(n) / 1024 / 1024 / dt
	}
	return r
}

func tlsHandshake(dial func() (net.Conn, error), sni string, to time.Duration) (*tls.Conn, error) {
	raw, err := dial()
	if err != nil {
		return nil, err
	}
	tc := tls.Client(raw, &tls.Config{ServerName: sni, InsecureSkipVerify: true})
	tc.SetDeadline(time.Now().Add(to))
	if err := tc.Handshake(); err != nil {
		raw.Close()
		return nil, err
	}
	return tc, nil
}

// ---------- ④ 规则清理 + -auto 应用 ----------
func (a *admin) cleanup() {
	for _, id := range a.ids {
		a.post("/forward/delete", map[string]string{"id": id})
	}
}

func (a *admin) apply(res []testResult) int {
	html, err := a.get("/")
	if err != nil {
		fmt.Println("⚠️ 读取规则失败")
		return 0
	}
	rows := reRuleRow.FindAllString(html, -1)
	n := 0
	for i := 0; i < len(rows) && i < len(res); i++ {
		row := rows[i]
		id := reIDv.FindStringSubmatch(row)
		name := reNamev.FindStringSubmatch(row)
		listen := reListenv.FindStringSubmatch(row)
		target := reTargetv.FindStringSubmatch(row)
		if len(id) > 1 && len(listen) > 1 {
			tgt := res[i].ip + ":443"
			// 保持原名/监听,只改 target
			form := map[string]string{"id": id[1], "listen": listen[1], "target": tgt}
			if len(name) > 1 {
				form["name"] = name[1]
			}
			a.post("/forward/update", form)
			fmt.Printf("   [应用] %-6s → %s\n", listen[1], tgt)
			n++
		}
		_ = target
	}
	return n
}

// ---------- 工具 ----------
func join(s []string) string { return strings.Join(s, ", ") }

func filterOK(in []testResult) []testResult {
	var out []testResult
	for _, r := range in {
		if r.ok {
			out = append(out, r)
		}
	}
	return out
}

func init() {
	net.DefaultResolver = &net.Resolver{PreferGo: false}
}
