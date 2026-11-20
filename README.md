# IPTV-ZT-Bridge

> 跨运营商 IPTV 组网桥接 · ZeroTier + 内置 rtp2httpd

## 项目介绍

IPTV-ZT-Bridge 把电信 IPTV 组播/单播源，经 ZeroTier 虚拟网络桥接到联通等其它运营商的群晖设备，无需公网 IP，直接向电视播放器输出标准 m3u。

套件拆分为 **两个独立的 SPK**：

| 套件 | 安装位置 | 组成 |
|------|----------|------|
| `IPTV-ZT-Bridge-Server` | 电信群晖 | 主程序 + ZeroTier 1.14.2 + **内置 rtp2httpd（无需另行安装）** |
| `IPTV-ZT-Bridge-Client` | 联通群晖 | 主程序 + ZeroTier 1.14.2 |

运行模式由安装的套件决定（SPK 启动脚本传入 `-mode server|client`），无需在 WebUI 中切换。

### 架构

```
┌──────────── 电信群晖（安装 Server 套件） ────────────┐
│  运营商频道源 M3U（rtp:// 组播 或 http:// 单播）     │
│       ↓                                             │
│  内置 rtp2httpd（127.0.0.1:8080）                   │
│       ↓  /playlist.m3u                              │
│  IPTV-ZT-Bridge                                     │
│  ├─ 绑定 ZeroTier 虚拟 IP                           │
│  ├─ 添加 Token 鉴权                                 │
│  ├─ 源存活检测，自动剔除失效节目                    │
│  └─ 对外输出带 Token 的 m3u                         │
└───────────────────┬─────────────────────────────────┘
                    │ ZeroTier 隧道
                    ↓
┌──────────── 联通群晖（安装 Client 套件） ────────────┐
│  IPTV-ZT-Bridge                                     │
│  ├─ 加入同一 ZeroTier 网络                          │
│  ├─ 拉取服务端 m3u → 重写为本地代理地址             │
│  ├─ 本地 HTTP 代理透传直播流                        │
│  └─ 输出可直接给电视播放的 m3u                      │
└─────────────────────────────────────────────────────┘
```

### 核心特性

- **Server 内置 rtp2httpd**：安装套件即可用，不需要再单独部署 rtp2httpd
- **双套件独立安装**：Server / Client 各自打包，模式由套件决定
- **ZeroTier 固定 1.14.2**：兼容 DSM 6.2.3，支持 tun 与用户态回落
- **Token 全程鉴权**：m3u 与流地址均带 Token，防止源泄露
- **m3u 自动重写**：Client 把服务端地址改写为本地代理地址
- **源存活检测**：自动剔除失效频道
- **隧道保活**：ZeroTier 进程守护，异常退出自动重启，状态实时展示
- **WebUI 管理**：网络 ID、在线状态、虚拟 IP、内置 rtp2httpd 状态、m3u 预览
- **多架构**：x86_64 / armv7 / arm64

---

## 快速开始（开发模式）

```bash
go mod tidy
make build

# Server
./build/x86_64/iptv-zt-bridge -mode server -web-root ./web/static -data-dir ./data

# Client
./build/x86_64/iptv-zt-bridge -mode client -web-root ./web/static -data-dir ./data
```

开发模式下内置二进制需放在主程序同目录（`zerotier-one`、`rtp2httpd`）。

WebUI：`http://<IP>:8089`，默认账号 `admin / admin`（首次登录后请立即修改）。

### 常用命令

```bash
make build-all           # 编译三架构
make download-zerotier   # 下载 ZeroTier 1.14.2（deb 解包）
make download-rtp2httpd  # 下载 rtp2httpd 3.9.1 静态二进制
make spk                 # 打包 Server + Client 共 6 个 SPK
make spk-server          # 只打包 Server 套件（含 rtp2httpd）
make spk-client          # 只打包 Client 套件
make lint                # go vet
make clean
```

---

## SPK 打包与安装

```bash
make spk
ls dist/
```

产物：

```
IPTV-ZT-Bridge-Server_0.1.0_x86_64.spk
IPTV-ZT-Bridge-Server_0.1.0_armv7.spk
IPTV-ZT-Bridge-Server_0.1.0_arm64.spk
IPTV-ZT-Bridge-Client_0.1.0_x86_64.spk
IPTV-ZT-Bridge-Client_0.1.0_armv7.spk
IPTV-ZT-Bridge-Client_0.1.0_arm64.spk
```

DSM 套件中心 → 手动安装 → 选择对应架构的 SPK。**Server 套件已内置 rtp2httpd，无需另行安装。**

安装并启动后，DSM 主菜单会出现套件图标，点击图标即可直接打开 WebUI（默认 `http://<NAS-IP>:8089/`）。

---

## 部署指南

### 前置条件

| 机器 | 安装套件 | 要求 |
|------|----------|------|
| 电信群晖（Server） | `IPTV-ZT-Bridge-Server` | DSM 6.2.3；能访问运营商 IPTV 频道源 |
| 联通群晖（Client） | `IPTV-ZT-Bridge-Client` | DSM 6.2.3；与 Server 加入同一 ZeroTier 网络 |

### Step 1：创建 ZeroTier 网络

1. 访问 https://my.zerotier.com/network/new 创建网络
2. 记录 **Network ID**（如 `8056c2e21c000001`）
3. 网络 Settings 保持默认

### Step 2：Server 端（电信群晖）

1. 安装 `IPTV-ZT-Bridge-Server` 套件，WebUI：`http://<ServerIP>:8089`
2. 「通用」填入 ZeroTier Network ID
3. 「Server 配置」填写：
   - **频道源 M3U**：在 WebUI 中直接**选择本地 m3u 文件上传**（内容可使用 `rtp://239.x.x.x:port` 组播地址）
   - **rtp2httpd 监听**：`127.0.0.1:8080`（只本机，由套件对外转发）
   - **对外监听**：`0.0.0.0:8081`（也可填 ZeroTier 虚拟 IP）
4. 点击顶部「启动服务」（唯一开关，会依次启动内置 rtp2httpd 与 ZeroTier）
5. 到 ZeroTier 后台（my.zerotier.com）**手动批准（Authorize）** 该节点，WebUI 会显示提示
6. 内置 rtp2httpd 自动输出 `/playlist.m3u`，Server 据此生成对外 m3u

> 所有带 `*` 的参数为必填。未填写完整时「启动服务」按钮不可点击，页面会列出缺少的参数。

Server 对外端点：

| 端点 | 说明 |
|------|------|
| `http://<ZT_IP>:8081/live.m3u?token=xxx` | 带 Token 的服务端 m3u |
| `http://<ZT_IP>:8081/stream/{encoded}?token=xxx` | 流代理端点 |
| `http://<LAN_IP>:8089` | WebUI 管理界面 |

### Step 3：Client 端（联通群晖）

1. 安装 `IPTV-ZT-Bridge-Client` 套件，WebUI：`http://<ClientIP>:8089`
2. 「通用」填入**同一个** ZeroTier Network ID
3. 「Client 配置」填写：
   - **服务端地址**：`<Server 的 ZT 虚拟 IP>:8081`（如 `10.144.0.1:8081`）
   - **本地监听**：`0.0.0.0:8082`
4. 保存配置 → 点击「启动服务」→ 在 ZeroTier 后台批准节点
5. 确认 Server / Client 在 ZeroTier 后台均为 **Authorized**

> ⚠️ 两端 `token` 必须一致（Client 用该 Token 拉取服务端 m3u，并签本地播放地址）。可在 WebUI「安全」中查看/重新生成。

### Step 4：电视播放

Client 上线后自动拉取并重写 m3u：

```
http://<Client 局域网 IP>:8082/live.m3u?token=<Token>
```

把该地址填进电视播放器即可。

### Step 5：验证

| 检查项 | 方法 |
|--------|------|
| Server ZeroTier 在线 | WebUI 绿色状态 |
| 内置 rtp2httpd 运行 | WebUI「内置 rtp2httpd」卡片，PID/监听地址正常 |
| Client ZeroTier 在线 | WebUI 绿色状态 |
| 隧道连通 | DSM 终端 `ping <Server ZT IP>` |
| m3u 可加载 | 播放器导入 Client m3u |

---

## 配置文件说明

Server 套件配置路径：

```
/var/packages/IPTV-ZT-Bridge-Server/etc/config.yaml
```

Client 套件：

```
/var/packages/IPTV-ZT-Bridge-Client/etc/config.yaml
```

示例（Server）：

```yaml
# 模式由 SPK 启动参数决定，配置文件中的值会被 -mode 覆盖
mode: server

# 鉴权 Token（两端必须一致，不要提交到仓库）
token: "iptv-zt-xxxxx"

zerotier:
  network_id: ""                 # WebUI 或此处填写
  api_port: 9993
  enable_tun: true               # tun 不可用时自动回落用户态
  fallback_userspace: true

rtp2httpd:                       # 内置 rtp2httpd（仅 Server 套件）
  listen_addr: "127.0.0.1:8080"
  # 由 WebUI 上传后自动写入，位于 <数据目录>/uploads/channels.m3u
  source_m3u: ""
  # binary_path / config_path 默认自动解析，无需填写

server:
  source_m3u: ""                 # 留空则自动使用内置 rtp2httpd 的 /playlist.m3u
  listen_addr: "0.0.0.0:8081"
  m3u_path: "/live.m3u"
  stream_path: "/stream/"
  health_check_interval: 300
  health_check_timeout: 5

client:
  server_addr: "10.144.0.1:8081"
  server_m3u_path: "/live.m3u"
  listen_addr: "0.0.0.0:8082"
  m3u_path: "/live.m3u"
  stream_path: "/stream/"
  upstream_timeout: 30

web:
  listen_addr: ":8089"
  username: "admin"
  password: "admin"              # 部署后立即修改
  session_key: ""                # 留空自动生成
```

### 命令行参数

```
./iptv-zt-bridge [flags]

  -mode string       运行模式: server | client（SPK 启动脚本自动传入）
  -config string     配置文件路径
  -data-dir string   数据目录
  -web-root string   Web 静态文件目录
  -version           显示版本
  -arch              显示当前架构
```

---

## 端口说明

| 端口 | 套件 | 说明 |
|------|------|------|
| 8089 | 两者 | WebUI 管理界面 |
| 8081 | Server | 对外 m3u + 流代理（建议仅 ZeroTier 子网可访问） |
| 8082 | Client | 本地代理，供电视访问 |
| 8080 | Server | 内置 rtp2httpd，仅监听 127.0.0.1，不对外 |
| 9993 | 两者 | ZeroTier 本地 API；UDP 9993 为 ZeroTier 通信 |

### DSM 防火墙

如启用了防火墙，请放行：

- Server：TCP 8081、8089；UDP 9993
- Client：TCP 8082、8089；UDP 9993

---

## DSM 6.2.3 特殊处理

### Tun 设备

安装脚本会创建 `/dev/net/tun` 并尝试加载内核模块：

```
mknod /dev/net/tun c 10 200
chmod 666 /dev/net/tun
modprobe tun sit gre vxlan
```

若 tun 仍不可用，ZeroTier 自动回落用户态（`--no-tap --allow-gateway`），功能不受影响。

### 开机自启

DSM 包管理器在开机时自动调用 start-stop-status 启动服务。

### 手动管理

```bash
synopkg status IPTV-ZT-Bridge-Server
synopkg start  IPTV-ZT-Bridge-Server
synopkg stop   IPTV-ZT-Bridge-Server
```

---

## 故障排查

| 现象 | 可能原因 | 解决 |
|------|----------|------|
| ZeroTier 一直 offline | 节点未批准 | 登录 my.zerotier.com → Authorize |
| 内置 rtp2httpd 启动失败 | 未上传频道源 M3U / 二进制缺失 | 在 WebUI「Server 配置」上传 m3u；确认 Server 套件完整安装 |
| rtp2httpd 起来了但无频道 | 频道源内容有误或不可访问 | 检查上传的 m3u 是否包含有效的 rtp/http 地址 |
| Client 拉不到 m3u | 隧道不通 / Token 不一致 | ping Server ZT IP；核对两端 Token |
| 电视无法播放流 | Client 未转发 / 超时 | 查看 Client 日志；增大 `upstream_timeout` |
| SPK 安装失败 | 架构不匹配 | 确认 DSM 架构，下载对应 SPK |

### 日志

```
/var/packages/IPTV-ZT-Bridge-Server/var/log/
/var/packages/IPTV-ZT-Bridge-Client/var/log/
```

---

## 项目结构

```
iptv-zt-bridge/
├── cmd/iptv-zt-bridge/       # 主程序入口（-mode 区分 Server/Client）
├── internal/
│   ├── config/               # 配置管理
│   ├── zerotier/             # ZeroTier 进程管理 + 本地 API
│   ├── rtp2httpd/            # 内置 rtp2httpd 进程管理（Server）
│   ├── m3u/                  # m3u 解析 / 重写
│   ├── auth/                 # Token 鉴权 + Session
│   ├── proxy/                # 流代理
│   ├── health/               # 隧道与源存活检测
│   └── web/                  # WebUI 路由
├── web/static/               # WebUI 前端
├── spk/
│   ├── INFO.tmpl / PACKAGE.tmpl
│   ├── build-spk.sh          # <arch> <server|client>
│   ├── fetch-zerotier.sh     # ZeroTier 1.14.2
│   └── scripts/              # preinst/postinst/preun/postun/start-stop-status
├── Makefile
└── README.md
```

---

## 技术栈

| 组件 | 版本 | 来源 |
|------|------|------|
| 主程序 | Go 1.21 | 自研 |
| ZeroTier | 1.14.2 | https://github.com/zerotier/ZeroTierOne |
| rtp2httpd | 3.9.1（内置静态二进制） | https://github.com/stackia/rtp2httpd |
| gorilla/mux | 1.8.1 | HTTP 路由 |
| gopkg.in/yaml.v3 | 3.0.1 | YAML 解析 |

---

## 安全注意事项

1. **不要硬编码网络 ID 和 Token**，全部通过 WebUI / 配置文件设置
2. 部署后立即修改默认密码 `admin / admin`
3. WebUI 建议只监听内网地址
4. Server 的 8081 仅对 ZeroTier 子网开放
5. Token 泄露会导致源被随意访问，可在 WebUI「安全」中一键重新生成（两端需同步更新）

---

## License

MIT
