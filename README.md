# iCloud 别名批量创建与管理系统 (iCloud Alias Manager)

参考开源项目 **[hme-manager](https://github.com/lw20011216-bot/hme-manager)** 与 **[iCloud-Privacy-Mail](https://github.com/q1953258942/iCloud-Privacy-Mail)** 的核心优势深度整合开发的独立高效管理服务。

采用纯 Go 语言实现，内置轻量现代 Web 控制台（基于 Vue 3 + Pico CSS），零第三方外部依赖，单个二进制开箱即用。

---

## 🌟 核心功能特性

### 1. 结合两家之长的全协议与快捷导入
- **cURL / Cookie 免密免 2FA 导入（参考 hme-manager）**：
  - 支持直接粘贴从浏览器 F12 复制的 `Copy as cURL (bash)` 或 Cookie 字符串。
  - 无需输入 Apple ID 密码，无需处理 2FA，直接接管创建隐藏邮件别名（最快捷安全）。
- **Apple 全协议登录与 2FA 验证（参考 iCloud-Privacy-Mail）**：
  - 原生内置 Apple SRP 6a 加密认证与受信任设备 2FA 验证流程。
  - 支持批量多行格式导入：`账号----密码` 或 `账号----密码----备注`。
- **双引擎自动创建**：
  - 优先调用 Apple Account 新接口，账号未提供管理态凭据时自动回落至 iCloud Web 经典接口 (`generate` + `reserve`)。

### 2. 完备的别名创建上限与频控调度
- **单账号别名上限 (Per-Account Limit)**：
  - 可对每个账号单独设置上限（或统一继承任务上限），账号达到创建上限后自动标记完成并安全跳过。
- **本次任务目标上限 (Target Batch Limit)**：
  - 支持设定本次批量任务总生成别名数（例如生成 50 个后自动停止），达到目标即停，防止无节制生成。
- **生成间隔与防封频控 (Rate Limiting & Cooldown)**：
  - 支持自定义每次生成别名之间的休眠间隔（如 3~5 秒）。
  - 智能捕获 Apple HTTP 429 与 `-41015`（达到地址上限/频率限制）响应，自动将该账号置入冷却队列并自动轮转下一个可用账号。

### 3. 实时可视化监控与别名管理
- **指标仪表盘**：
  - 实时展示已导入账号总数（可用/冷却/失效）、累计别名总数、今日创建数。
  - 实时任务进度条与动态百分比（如 `42 / 50 (84%)`）。
- **毫秒级 SSE (Server-Sent Events) 实时流水日志**：
  - 前端无需刷新，实时推流展示每一个账号的候选地址生成、保留提交、成功入库及频控告警日志。
- **别名资产实时列表与一键导出**：
  - 自动归集所有账号生成的别名，展示所属主账号、标签、备注及创建时间。
  - 支持按关键词实时检索、一键复制全部别名、一键导出为 `TXT` 或 `CSV` 格式。

---

## 🚀 快速启动

### 方式一：直接运行编译好的二进制（Windows）

项目根目录已为您编译好单文件可执行程序：

```powershell
.\icloud-alias-manager.exe -port 8788
```

启动后在浏览器打开：
👉 **http://127.0.0.1:8788**

### 方式二：使用 Go 源码编译/启动

```powershell
# 运行源码
go run ./cmd/server

# 或重新编译为可执行程序
go build -o icloud-alias-manager.exe ./cmd/server
```

---

## 📖 导入格式指南

在 Web 控制台右上角点击 **【➕ 导入邮箱】**，支持在文本框中批量粘贴以下任意格式（智能自动识别）：

### 格式 1：带取码接口的账号格式（最推荐，全自动登录与自动 2FA）
市面上批量购买的 Apple 账号通常附带提取验证码的 API（如您提到的 lava-group 等接码接口），直接以如下格式多行粘贴：
```text
your_apple_id@icloud.com----YourPassword123----https://devices.example.com/api/getcode?id=100001
your_second_id@icloud.com----YourPassword456----https://devices.example.com/api/getcode?id=100002
```
> **工作原理**：系统导入后会标记为 `⚡ 自动取码`。当启动任务或点击【🤖 自动登录】时，程序发起 Apple SRP 登录触发 2FA，同时**在后台自动轮询该 URL 提取 6 位验证码并自动提交**，全程无需人工干预！

### 格式 2：浏览器 cURL 导入（免密免 2FA）
1. 使用电脑浏览器访问 [iCloud+](https://www.icloud.com/icloudplus/) 或 [iCloud+ 中国](https://www.icloud.com.cn/icloudplus/)。
2. 按 `F12` 打开开发者工具，切换到 `Network (网络)` 标签。
3. 点击“隐藏我的电子邮件”，找到任意包含 `/v2/hme/list` 的请求。
4. 右键该请求选择 **`Copy` -> `Copy as cURL (bash)`** 并直接粘贴。

### 格式 3：普通账号密码批量导入（手动 2FA）
```text
user1@icloud.com----Password123
user2@icloud.com----Password456----备注信息
```
导入后点击账号行的 **【手动 2FA】**，输入 Apple 发送到受信任设备弹出的 6 位验证码即可生成登录态。

---

## 📂 项目架构

```text
E:\weapp\icloud\
├── cmd/
│   └── server/
│       └── main.go          # 服务入口与命令行配置
├── internal/
│   ├── model/
│   │   └── types.go         # 核心数据模型 (Account, Alias, TaskConfig, Logs)
│   ├── apple/
│   │   ├── srp.go           # Apple SRP 6a 加密算法
│   │   ├── auth.go          # Apple ID 认证与 2FA 流程
│   │   ├── client.go        # HME 别名生成/保留双引擎客户端
│   │   └── importer.go      # cURL、Cookie、账号密码智能解析器
│   ├── store/
│   │   └── store.go         # 线程安全持久化存储 (data/store.json)
│   ├── engine/
│   │   └── runner.go        # 批量队列执行器、上限控制与频控冷却
│   └── web/
│       ├── server.go        # HTTP API、SSE 实时推流、文件导出
│       └── static/
│           └── index.html   # 现代化单页 Web 控制台 (Vue 3)
├── icloud-alias-manager.exe # 编译产物
└── README.md
```
