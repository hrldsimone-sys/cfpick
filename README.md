# cfpick — Cloudflare 优选 IP 挑选工具(经 BaiduTunnel 实测)

为 **x-tunnel(BaiduTunnel 前置代理方案)** 自动挑选最优 Cloudflare 边缘 IP 的命令行工具。

输入一个 Cloudflare 前置的域名,自动完成:**域名解析 → 邻居段扫描 → TCP 粗筛 → 经百度隧道实测(SNI 握手 + 5MB 测速)→ 输出 TOP N**,全程自动清理临时规则,不污染 BaiduTunnel 配置。

> 背景:直接连 Cloudflare 边缘不稳定时,用 BaiduTunnel(国内 CDN)做前置代理,x-tunnel 通过 `-ip 127.0.0.1:转发端口` 钉到本地转发端口。本工具负责找出"从百度边缘出口到目标 CF 边缘"最快的 IP。

---

## ✨ 功能

| 步骤 | 说明 |
|---|---|
| ① 域名解析 | 解析 `-domain` 得到当前 CF 边缘 IP 作为种子 |
| ② 邻居段扫描 | 在种子所在 /24 段内并发扫描 TCP 443(48 并发) |
| ③ 粗筛 | 直连 RTT ≤ `-rtt`(默认 150ms)的进入隧道实测 |
| ④ 隧道实测 | 经 BaiduTunnel 加临时转发规则 → **SNI=你的域名握手**(判据)+ **speed.cloudflare.com 下载 5MB 测速** |
| ⑤ 输出排行 | 按速度排序,输出 TOP N(含握手延迟) |
| ⑥ 自动清理 | 测试规则全部删除,配置还原 |
| ⑦ (可选)应用 | `-auto` 把 TOP N 直接写进 BaiduTunnel 转发规则 |

## 📦 编译

```bash
# 本地(Go 1.21+)
go build -trimpath -buildvcs=false -ldflags="-s -w -buildid=" -o cfpick .
```

GitHub Actions 已配置:推送 `v*` 标签自动编译 **linux/amd64 · linux/arm64 · darwin/amd64 · darwin/arm64 · windows/amd64 · freebsd/amd64** 并发布 Release。

## 🚀 使用

```bash
# 基本用法:给出 x-tunnel 域名,输出 TOP 10
./cfpick -domain jpx.zqsg.eu.org

# 指定 BaiduTunnel 后台(默认 http://127.0.0.1:8080,密码默认 123456)
./cfpick -domain jpx.zqsg.eu.org -web http://127.0.0.1:8080 -pass 你的密码

# 只要 5 个,更严格粗筛(100ms)
./cfpick -domain jpx.zqsg.eu.org -keep 5 -rtt 100

# 测完自动把 TOP N 写入 BaiduTunnel 转发规则
./cfpick -domain jpx.zqsg.eu.org -keep 5 -auto
```

### 参数

```
-domain <域名>  必填。x-tunnel 服务端域名(Cloudflare 前置)
-web <地址>     BaiduTunnel 管理后台(默认 http://127.0.0.1:8080)
-pass <密码>    后台密码(默认 123456)
-keep <N>       输出几个 IP(默认 10)
-rtt <ms>       粗筛阈值(默认 150ms)
-scan <N>       扫描候选上限(默认 384)
-con <N>        隧道并发测试数(默认 6)
-auto           测完自动应用 TOP N 到转发规则
```

## 🔧 使用建议

1. 在**跑 BaiduTunnel 的同一台机器**上运行(粗筛和隧道实测都依赖本机网络)
2. 百度边缘故障期间测出的结果无效,等恢复再跑
3. 选出的 IP 用于:
   - BaiduTunnel 转发规则 target(`IP:443`)
   - 或 x-tunnel 客户端 `-ip` 参数(配 `127.0.0.1:转发端口` 时指向本机)

## 📂 结构

```
cfpick/
├── main.go              # 全部逻辑(纯标准库,零依赖)
├── go.mod
└── .github/workflows/   # 多平台编译 + Release
```

## 📄 许可

未选择 LICENSE,私人使用。