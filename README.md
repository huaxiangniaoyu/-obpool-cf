# obpool — Cloudflare Worker 节点优选池

把你手里所有订阅的节点全拉下来, 再乘上 bestcf 的优选前置域名, 在路由器本机
逐个实测**延迟 + 真实下载带宽**, 按加权分数选出最强的一批, 生成一个可以直接
导入代理软件的 mihomo YAML 节点池。每天定时重跑, 优胜劣汰。

## 它做什么

1. **汇集候选**
   - 读取订阅列表文件里的每条订阅 (支持 vless / trojan / vmess 分享链接,
     明文或 base64; 也支持 mihomo YAML 格式订阅, 如 CFNext 按 UA 返回的那种)
   - 可选: 提取 Open-Box 的 `config.json` 里已有的出站节点
   - 抓取 [bestcf.pages.dev](https://bestcf.pages.dev) 的优选前置域名
     (地区子域名 + cf.* 优选域名), 与每个唯一 Worker 端点交叉组合出新候选
   - 全部按连接身份去重 (实测: 9 条订阅 1052 个节点其实只有 19 个不同端点)
2. **延迟测试** — 分批起独立的 sing-box 测试实例 (复用 Open-Box 自带内核),
   并发 10、每批 100 个, 两轮取平均; 失败的直接丢弃
3. **带宽测试** — 延迟前 60 名逐个切换, 每个实拉 5 MB 测真实下载速度
   (延迟好但拉不动大文件的"假活"节点在这一步现形)
4. **加权排名** — 分数 = 带宽 / (1 + 延迟/200) × (1 + 0.05 × 历史进前50晚数),
   取前 20 名成池; 历史按晚累积, 长期稳定的节点有加成, 每晚重排优胜劣汰
5. **输出** (在工作目录):
   - `pool.yaml` — 完整 mihomo 配置 (节点 + 优选池分组 + MATCH 规则), 可直接导入
   - `pool-proxies.yaml` — 只有 proxies 段, 适合做 proxy-provider
   - `pool.txt` — 分享链接
   - `pool-singbox.json` — sing-box outbounds
   - `report.txt` — 本轮报告 (延迟/带宽/稳定性)
   - `history.jsonl` — 每晚成绩存档

## 为什么跑在路由器上

- 路由器本机流量天然直连 (不经过 Open-Box 的 TUN), 测的是你家线路的真实质量
- 温和模式: 延迟并发 10、测试实例内存上限 128 MiB、带宽只测 60 个 × 5 MB,
  全程流量约 300 MB、十几分钟, 不影响家里设备上网

## 部署 (OpenWrt / ImmortalWrt, aarch64)

### 方式一: ipk 安装包 (推荐)

Releases 里有打好的 `obpool_1.0.0_aarch64_generic.ipk`, 传到路由器后:

```sh
opkg install obpool_1.0.0_aarch64_generic.ipk
```

安装会自动: 把程序放进 `/opt/open-box/pool/`、从示例生成 `subs.txt`
(升级不覆盖)、注册每晚 20:40 的定时任务。装完把你的订阅填进
`/opt/open-box/pool/subs.txt`, 再手工跑一次 `/opt/open-box/pool/run.sh` 即可。
卸载 `opkg remove obpool` 会自动摘掉定时任务。

自己打 ipk: `./build-ipk.sh` (需要 Go 环境交叉编译)。

### 方式二: 手工放文件

```sh
# 1. 把二进制和订阅文件放到路由器
scp obpool-arm64 subs.txt root@192.168.8.1:/opt/open-box/pool/
ssh root@192.168.8.1 'chmod +x /opt/open-box/pool/obpool-arm64'

# 2. 手工跑一轮
cd /opt/open-box/pool && ./obpool-arm64 -subs subs.txt
```

没有预编译二进制时自己编 (纯 Go 标准库, 无第三方依赖):

```sh
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o obpool-arm64 .
```

## 配置

### 订阅列表 `subs.txt`

一行一个订阅地址, `#` 开头是注释。把你自己的订阅填进去即可:

```
# 我的订阅
https://example.com/sub?token=xxxx
https://example.org/xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx/sub
```

### 定时运行

用自带的 `run.sh` (带日志), 在路由器上加一条 crontab。比如每晚 20:40
(晚高峰测出来的池子最经得起用):

```sh
echo '40 20 * * * /opt/open-box/pool/run.sh' >> /etc/crontabs/root
/etc/init.d/cron enable && /etc/init.d/cron start
```

改时间就改 crontab 里的时间字段: 前两位是「分 时」。

### 常用参数

| 参数 | 默认 | 说明 |
|---|---|---|
| `-subs` | (无) | 订阅列表文件路径 |
| `-config` | (空) | 可选: Open-Box config.json 路径, 给了会一并提取其中节点 |
| `-bestcf` | `https://bestcf.pages.dev/` | bestcf 页面; 设为空字符串则不做前置组合 |
| `-workdir` | `/opt/open-box/pool` | 输出目录 |
| `-singbox` | `/opt/open-box/bin/sing-box` | sing-box 二进制路径 |
| `-top` | 20 | 入池节点数 |
| `-finalists` | 60 | 进入带宽测试的数量 |
| `-batch` | 100 | 每批延迟测试的节点数 |
| `-conc` | 10 | 延迟测试并发 |
| `-limit` | 0 | 候选上限 (调试/冒烟用) |

## 注意

- 测试实例用 127.0.0.1:19095 (API) 和 127.0.0.1:17895 (mixed 入口),
  与 Open-Box 主实例 (9095) 不冲突
- 订阅里的节点凭据只存在你自己的 `subs.txt` 和输出文件里, 
- 本工具只做测速与筛选, 不修改 Open-Box 的任何配置

## License

MIT
