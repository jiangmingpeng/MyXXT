# 学不通(XBT)

> 一套面向学习通签到场景的三端协同系统：`Web 管理端` + `Go 后端` + `Android 原生壳`

[![Web](https://img.shields.io/badge/Web-React%2019%20%2B%20Vite-61dafb)](#)
[![Server](https://img.shields.io/badge/Server-Go%201.25%20%2B%20Gin-00add8)](#)
[![Database](https://img.shields.io/badge/DB-PostgreSQL-336791)](#)
[![Android](https://img.shields.io/badge/Android-Compose%20%2B%20CameraX-3ddc84)](#)
[![License](https://img.shields.io/badge/License-Private-lightgrey)](#)

---

## 项目简介

学不通  是一个围绕课程签到管理与执行的工具型项目，核心能力包括：

- 学习通账号登录与鉴权
- 课程同步与监控课程选择
- 签到活动聚合展示（按课程分组）
- 多人代签与状态跟踪
- 多签到类型支持（普通 / 二维码 / 手势 / 位置 / 签到码）
- 管理员白名单维护（单条添加、批量导入、删除）
- 多账号本地切换

---

## 核心特性

### 1) 三端协同架构

- `Web`：主业务 UI，负责课程配置、活动查看、签到执行、白名单管理
- `Server`：鉴权、课程与活动数据聚合、签到执行、权限控制
- `Android`：WebView 容器 + 原生相机桥接，提升扫码体验与机型兼容性

### 2) 多类型签到统一流程

- 普通签到：直接执行
- 手势 / 签到码：输入 `sign_code` 执行
- 位置签到：选择预设地点后提交经纬度与地点描述
- 二维码签到：扫码后自动解析 `enc/c` 并并发执行

### 3) 受限并发 + 重试机制

- 执行前先调用 `/api/sign/check` 过滤已签用户
- 待签用户按**受限并发 + 随机抖动**执行（默认同时 2 个），不再一次性打满
- 后端再叠加一层账号级节流与风控熔断，双重限速
- 内置失败重试与进度可视化，支持中断；触发风控熔断的账号会立即停止重试

### 4) 权限模型清晰

- `permission = 1`：普通用户
- `permission = 2`：管理员
- 管理端白名单接口仅管理员可访问

### 5) 圆心校准可跨重载续传

- 学习通只下发签到地址、不下发圆心坐标，且每次「超出范围」的提交会返回精确距离（`errorLocation2_<米>`），前端用三个不共线的探测点做三边定位反解圆心
- 刷新二维码只能用一次，手机拍照 / 相册导入模式下每探测一次都要拍一张新码；页面被相机或系统回收重载时，已采集的探测距离会从 `localStorage` 恢复，点「继续校准」从断点接着跑，不必重新拍前面的码
- 校准进度按活动 id 隔离，并在超过 `sign.calibration_session_ttl_minutes` 后自动失效，避免把上一场活动的距离套到别的签到圆心上

### 6) 手动拖动签到圈（校准兜底）

- 刷新二维码的 `enc` 只有约 5~10 秒有效期，而一次探测在慢代理下要 8~16 秒：enc 往往在 `stuSignajax` 之前就过期，学习通只回「二维码已过期(2001)」、不给距离，自动校准会卡在第一次探测
- 地图页右上角新增「手动圈」：点地图或拖动橙色圆点即可把签到圈搬到老师位置，半径可用滑杆微调（20~3000 米，默认取后端下发的签到范围，没下发时 500 米），全程不发任何网络请求
- 手动圈会标记 `signSource=manual`，迟到的地址反查不会再把圆和选点拽回偏差几百米的兜底坐标
- 自动校准第一次就失败时，底部会直接弹出「拿不到距离？直接手动拖动签到圈更稳」的入口，并提示拖好后点「用签到点」把提交坐标也落到圈内

---

## 技术栈

### Web (`/Web`)

- React 19 + TypeScript
- Vite 8
- TailwindCSS 4
- Zustand（本地多账号状态管理）
- Axios（统一请求/鉴权拦截）
- Framer Motion（动效）
- html5-qrcode（浏览器扫码）

### Server (`/Server`)

- Go 1.25
- Gin
- GORM
- PostgreSQL
- JWT 鉴权
- YAML 配置加载

### Android (`/Android`)

- Kotlin + Jetpack Compose
- WebView 容器化
- CameraX + ML Kit（原生扫码）
- JavaScript Bridge（原生相机与 Web 页面通信）

---

## 系统架构

```mermaid
flowchart LR
  U["用户"] --> W["Web 前端 (React)"]
  U --> A["Android App (WebView + CameraX)"]
  W --> S["Go Server (Gin)"]
  A --> S
  S --> D["PostgreSQL"]
  S --> X["学习通相关接口"]
```

---

## 目录结构

```text
XBT2.0
├── Web/                  # React 前端
│   ├── src/pages/        # 业务页面（登录、课程、签到、白名单、账号管理、扫码）
│   ├── src/components/   # 组件（签到输入、下拉刷新、路由保护等）
│   ├── src/store/        # Zustand 状态（账号/Token）
│   └── config.yaml       # 前端配置（API、位置预设）
├── Server/               # Go 后端
│   ├── cmd/server/       # 入口
│   ├── internal/         # 业务逻辑（handler/service/middleware/...）
│   ├── config.yaml       # 后端配置
│   ├── init.sql          # PostgreSQL 初始化脚本
│   └── API.md            # 接口文档
├── Android/              # Android 原生壳
└── README.md
```

---

## 快速开始（本地开发）

### 0) 环境要求

- Node.js 20+
- Go 1.25+
- PostgreSQL 14+（项目脚本按 PostgreSQL 18+ 编写，低版本一般也可用）
- Android Studio（如需构建移动端）

### 1) 初始化数据库

```bash
# 示例：创建数据库后执行
psql -U <user> -d <dbname> -f Server/init.sql
```

### 2) 启动后端

```bash
cd Server
# 首次运行：从模板生成配置并填入数据库密码 / 密钥
cp config.example.yaml config.yaml
go mod download
go run ./cmd/server
```

> `config.yaml` 已被 `.gitignore` 忽略（避免提交真实密钥）。
> 其中 `antiban` 段是反风控配置，**部署到公网前必须配置 `proxy_pool`**，详见「反风控设计」。

默认监听：`http://localhost:3030`

健康检查：

```bash
curl http://localhost:3030/api/health
```

### 3) 启动前端

```bash
cd Web
npm install
npm run dev
```

默认地址：`http://localhost:5173`

开发模式下，Vite 已将 `/api` 代理到 `http://localhost:3030`。

### 4) 启动 Android（可选）

```bash
cd Android
./gradlew assembleDebug
```

Android 壳默认目标地址来自 `Android/app/src/main/res/values/strings.xml` 中的 `target_url`。

---

## 配置说明

### 后端配置：`Server/config.yaml`

关键字段：

- `app_env`：运行环境标识（`dev/development` -> Gin `debug`，`prod/production` -> Gin `release`，`test/testing` -> Gin `test`）
- `http_addr`：服务监听地址（默认 `:3030`）
- `jwt_secret`：JWT 签名密钥（生产必须修改）
- `credential_secret`：账号凭据加密密钥（生产必须修改）
- `postgres_dsn`：PostgreSQL 连接串
- `activity_list_limit`：每门课返回活动上限（默认 `5`）
- `allow_insecure_tls`：是否允许不安全 TLS（**默认关闭**；只有证书链异常的私有环境才需要打开）
- `antiban`：反风控配置段，详见下方「反风控设计」

### 前端配置：`Web/config.yaml`

关键字段：

- `api.base_url`：默认 `/api`
- `api.timeout`：普通接口（列表 / 查询 / 检查）超时，默认 `10000` 毫秒
- `api.sign_timeout`：签到类接口（`/sign/execute`、`/sign/photo`）单独的超时，默认 `25000` 毫秒。
  一次二维码签到在后端要串行走完「预签到 → 验证码识别 → stuSignajax」，经代理往返后正常就要 6~15s，
  用 10s 会出现「后端还在正常执行、前端已经报 timeout」
- `sign.max_concurrency`：批量签到并发上限（**建议与后端 `antiban.max_concurrency` 一致**，默认 `2`）
- `sign.stagger_min_ms` / `sign.stagger_max_ms`：每个账号发起请求前的随机等待区间
- `sign.calibration_session_ttl_minutes`：圆心校准进度的本地保留时长（默认 `120` 分钟），超时视为上一场签到并丢弃
- `sign.location_presets`：位置签到预设点（名称 / 经纬度 / 描述）

---

## 业务流程（简版）

1. 用户通过手机号/密码登录（`/api/auth/login`）
2. 同步课程（`/api/courses/sync`）
3. 选择需要监控的课程（`/api/courses/selection`）
4. 首页拉取签到活动（`/api/sign/activities`）
5. 进入活动详情，勾选代签同学
6. 执行签到前检查状态（`/api/sign/check`）
7. 并发执行签到（`/api/sign/execute`）

---

## API 文档

完整接口说明见：

- [Server/API.md](./Server/API.md)

接口前缀：`/api`  
统一返回结构：

```json
{
  "code": 0,
  "message": "ok",
  "data": {}
}
```

---

## 数据库表概览

核心表（详见 `Server/init.sql`）：

- `users`：用户与加密凭据
- `whitelists`：白名单（普通/管理员）
- `courses`：课程基础信息
- `user_courses`：用户-课程关系与选课状态
- `sign_activities`：活动缓存
- `sign_records`：签到记录
- `account_profiles`：账号设备画像（UA / deviceCode / 代理出口），保证同一账号跨重启仍是"同一台手机"

---

## 反风控设计

学习通判断"脚本批量签到"，靠的不是加密参数（那部分已经对齐官方客户端），
而是**行为模式**。本项目的风控风险主要来自四条可观测特征，`Server/internal/antiban`
针对每一条都做了对应处理：

| 风险特征 | 具体表现 | 现在的处理 |
| --- | --- | --- |
| 同一出口 IP | 一个数据中心 IP 同时服务 N 个不同学号 | `proxy_pool` 按账号哈希固定分配出口，每个代理独立连接池 |
| 同一设备指纹 | 所有账号共用同一条 `Redmi K20 Pro` UA | `device_templates` 指纹池 + 账号级稳定 `deviceCode`（落库，不复位） |
| 同一时刻爆发请求 | `Promise.all` 让 10 个账号的请求在 1ms 内发出 | 前端受限并发 + 随机抖动；后端全局并发闸门 + 账号级随机间隔 |
| 验证码穷举 | 一张滑块图连续试 40 个坐标 | `captcha_max_attempts` 限量（默认 8）+ 每次失败后随机停顿 |

### 关键机制

- **设备指纹稳定化**：`deviceCode` 由 `sha256(mobile + 机型)` 派生，不再每次登录随机生成；
  `account_profiles` 表持久化后，服务重启、会话过期、重新登录都不会换"手机"。
- **账号级熔断**（`breaker_threshold` / `breaker_cooldown_sec`）：
  连续出现「请求过于频繁 / 需要验证码 / 账号异常」时，该账号进入冷却期，
  冷却期内直接返回 `code=2004`，**不会再向学习通发任何请求**（越重试越难解封）。
  重复触发时冷却时间指数增长，上限 30 分钟。
- **节流闸门**：所有出站请求都经过 `Client.do()/doRead()`，统一执行"账号级间隔 → 全局并发槽位 → 发请求 → 上报信号"。
  账号级间隔是"先睡再占槽"，不会因为某个账号等待而堵住其它账号。
  读（活动列表/详情）与写（登录/预签到/签到）分成两档：写默认 `300~1200ms`，读默认 `80~240ms`，
  既能压住签到爆发，又不会让活动列表首屏慢到不可用。
- **日志脱敏**：`enc`、`location`、`deviceCode` 默认只记录掩码；完整参数需要显式打开 `log_payloads`。

### 部署建议（按重要性排序）

1. **必须配置 `proxy_pool`**。没有代理时所有账号共用一个出口 IP，这是最容易被识别的特征。
   建议出口数量 ≥ 账号数 / 3，并保证出口归属地与学生所在城市一致。
   自检方式（确认代理可用且各出口 IP 不同）：

   ```bash
   # 直接问代理自己的出口 IP，不依赖目标站点的可达性
   curl -sS --max-time 10 -x 'socks5h://user:pass@host:port' https://ifconfig.me/ip
   ```

   注意参考 `config.yaml` 里实测过的两点：`api.ipify.org` 在部分国内线路/代理上会直接断连，
   所以 `exit_ip_probe_url` 用 `https://ifconfig.me/ip`；便宜的代理节点偶发超时属正常，
   熔断 + 有限重试就是为这种抖动准备的，但频繁超时的节点应换掉（该节点上的账号会一直失败）。
2. `max_concurrency` 保持 `2`（最多 `4`）。把它调大等于把刚收敛的并发又放开。
3. `client_ip_mode` 默认 `empty`。配置了稳定代理后可改为 `auto`，
   让 `clientip` 与真实出口一致，避免"参数说 A 地、IP 在 B 地"的矛盾；
   改 `auto` 前先确认 `exit_ip_probe_url` 经代理可达（探测失败会静默退回 `empty` 行为，并缓存 30 分钟）。
4. 生产环境保持 `allow_insecure_tls: false`、`log_payloads: false`。
5. 校验配置是否生效：启动日志会打印
   `[antiban] enabled=... proxies=... max_concurrency=... fingerprints=...`,
   并对"未配置代理 / 关闭熔断 / 关闭证书校验"直接给出警告。

### 排障

- 想看完整请求参数：临时把 `antiban.log_payloads` 设为 `true`，排查完立刻改回。
- 想完全关闭节流（仅本地调试）：`antiban.enabled: false`。
- **签到慢 / 前端报 timeout**：先看后端日志里的两行耗时——
  - `[captcha] prefetch activity=... elapsed=... ok=...`：验证码识别（元信息 + 背景图 + 滑块图 + 校验）的总耗时；
  - `[timing] qr sign activity=... preSign=... prefetched=... sign=... total=...`：二维码签到的分段耗时，
    `preSign` 含 `preSign → analysis → analysis2` 三个请求，`sign` 是最后的 `stuSignajax`。
  两段并行执行，`total ≈ max(preSign, captcha) + sign`。`total` 长期 > 12s 说明代理往返太慢
  （每个请求都要付一次 RTT，减少请求数已做到极限），换低延迟节点是唯一有效手段；
  `prefetched=false` 且 `sign` 明显变长，说明验证码还没算完、`Sign` 在等同一个单飞任务（正常行为，不会重复求解）。
- 需要重置某账号的指纹：删除 `account_profiles` 中对应 `mobile` 的记录后重新登录。
- 某个账号一直超时/异常：先确认它命中的那个代理是否可用——出口是按 mobile 哈希固定分配的，
  首次分配后随指纹一起落在 `account_profiles.proxy_url`，**只改 `proxy_pool` 不会搬动已有账号**。
  需要换掉坏节点时，清空指向旧节点的记录并重启服务，账号会按新代理池重新绑定：

  ```sql
  UPDATE account_profiles SET proxy_url = '', exit_ip = '' WHERE proxy_url LIKE '%旧节点地址%';
  ```

  （档案里的设备指纹不受影响，账号仍是同一台"手机"。）

---

## 安全与合规提示

- 本项目包含账号登录、课程与签到数据处理能力，部署前请确认符合法律法规与学校/平台使用规范。
- 仓库中的示例配置（如密钥、DSN）仅用于开发演示，生产环境务必替换。
- 建议使用 HTTPS、最小权限数据库账号、独立密钥管理和访问审计。

---

## 常见问题

### 1) 为什么前端请求不到后端？

- 检查后端是否运行在 `:3030`
- 检查 `Web/vite.config.ts` 代理配置
- 若为生产环境，确认网关将 `/api` 正确转发到后端

### 2) 登录后提示无权限？

- 白名单非空时，仅白名单用户可登录
- 白名单为空时，首次登录用户会自动成为管理员

### 3) 二维码签到在部分设备不稳定？

- Web 端会走 `html5-qrcode`
- Android 壳可借助 CameraX + ML Kit + 原生桥接增强稳定性

---

## 开发建议

- 提交前执行：

```bash
# Web
cd Web && npm run lint && npm run build

# Server
cd Server && go test ./...
```

---

## 免责声明

本项目仅用于技术研究与学习交流，请勿用于任何违反平台规则、学校管理规定或法律法规的场景。使用者需自行承担相应责任。
