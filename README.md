# my_ai_sum

my_ai_sum 是一个本地 OpenAI 兼容 API 聚合网关。它把外部中转站、DeepSeek、Grok/xAI、OpenAI 兼容接口等上游服务统一接入，再对本地或局域网客户端提供统一的 OpenAI 风格调用入口。

项目当前目标是一个前后端封装在一起的小型应用服务：可以开发运行，也可以打包成单个 Windows exe，双击后启动服务并通过浏览器管理配置。

## 功能概览

- 前后端一体：Go 后端通过 `embed` 内置前端静态页面。
- 单文件运行：支持构建为 `build\my-ai-sum.exe`。
- 外部服务管理：保存上游 Base URL、API Key、模型列表、请求协议、代理策略。
- 代理管理：支持 HTTP、HTTPS、SOCKS5，可设置默认代理，也可为外部服务指定代理。
- 本地 API Key：为客户端生成本地 Key，并绑定指定外部服务。
- 模型映射：把客户端传入的本地模型名转换为上游真实模型名。
- 协议转换：支持 `Responses` 与 `Chat Completions` 双向转换，包含流式与非流式。
- OpenAI 兼容接口：支持 `/v1/models`、`/models`、`/v1/chat/completions`、`/chat/completions`、`/v1/responses`、`/responses`。
- API 对话测试：可测试外部上游 API，也可测试本地兼容 API。
- Token 用量统计：记录输入、输出、总量，支持按外部服务、上游模型、日期和明细查看。
- 请求日志：记录请求来源、状态、耗时、错误、Token 等信息。
- 调试日志：可开关 API 调试日志，日志写入 `data\logs`，并在独立页面查看、实时刷新、编辑和保存。
- 系统设置重载：监听地址和端口保存后可通过重载按钮立即生效。

## 技术栈

- Go 1.20+
- SQLite，使用纯 Go 驱动 `modernc.org/sqlite`
- 原生 HTML、CSS、JavaScript
- PowerShell 构建脚本

## 快速启动

开发运行：

```powershell
.\scripts\dev.ps1
```

默认访问地址：

```text
http://127.0.0.1:8716/
```

首次启动会进入初始化页面，需要设置管理员账号密码。

## 构建 exe

```powershell
.\scripts\build.ps1
```

默认输出到当前目录：

```text
build\my-ai-sum.exe
```

也可以指定输出位置：

```powershell
.\scripts\build.ps1 -Output "D:\Tools\my-ai-sum.exe"
```

双击 exe 即可启动服务。默认使用 `http://127.0.0.1:8716/` 访问。如果没有初始化，则会先跳转到设置密码界面，需要先设置登录密码后再登录，如果已初始化，则可直接登录。

目前系统设置中的监听地址可以设置为固定 IP，也可以设置为 `0.0.0.0`。如果设置为固定 IP 只能通过该 IP 访问，如果设置为 `0.0.0.0` 则会监听全部网卡。

## 数据目录

默认数据目录为 exe 同级的 `data` 文件夹。主要文件包括：

```text
data\config.json
data\my_ai_sum.db
data\master.key
data\logs\api-debug-YYYY-MM-DD.log
```

`config.json` 为项目配置文件，其中 `data_dir` 为数据存放目录，默认值为 `data` 表示与 exe 同级的 `data` 文件夹；如需指定数据目录，也可使用环境变量：

```powershell
$env:MY_AI_SUM_DATA_DIR="D:\WorkSpace\Python\my_ai_sum\data"
```

`master.key` 用于加密保存外部服务 API Key、代理密码、本地 Key 等敏感信息，如需要备份数据，请将 `master.key` 也和数据库文件一起备份。

## 具体管理页面介绍

### 仪表盘

展示外部服务数量、本地 Key 数量、调用量、最近请求等概览信息。外部服务相关展示使用外部服务名称，便于区分同类型上游。

### 外部服务

外部服务用于配置上游 API。常见字段：

- 名称
- 类型：中转站、DeepSeek、Grok/xAI、OpenAI 兼容
- Base URL
- API Key
- 模型列表
- 请求协议：`Responses` 或 `Chat Completions`
- 代理模式：不使用、默认代理、指定代理
- 状态：启用或停用

请求协议决定直接测试外部上游 API 时使用哪个接口，也会作为本地 Key 的默认上游协议来源。

推荐配置：

- OpenAI 或支持 Responses 的中转站：请求协议选择 `Responses`
- DeepSeek 这类 Chat Completions 上游：请求协议选择 `Chat Completions`
- 如果代理模式为“不使用”，指定代理选择框会禁用

说明：界面已经统一命名为“外部服务”。后端接口和数据库字段中仍保留 `provider_key`、`provider_key_id` 这类内部命名，用于兼容已有代码和数据结构。

### 代理

代理支持：

- HTTP
- HTTPS
- SOCKS5

新增代理时用户名和密码默认为空。没有认证的本地代理，例如：

```text
http://127.0.0.1:7890
```

只需要填写主机 `127.0.0.1`、端口 `7890`，用户名和密码保持空即可。

### 模型映射

模型映射用于把客户端使用的模型名转换为上游真实模型名。

示例：

```text
本地模型：gpt-5.5
上游模型：deepseek-chat
外部服务：DeepSeek 官方
```

当客户端使用本地模型名调用本地兼容 API 时，网关会在转发到对应外部服务前转换为上游模型名。Token 用量统计保存的是上游真实模型名，同时保留本地模型名，方便排查和统计。

### 本地 API Key

本地 API Key 是客户端真正调用 my_ai_sum 时使用的 Key。创建或编辑本地 Key 时需要选择绑定的外部服务，并配置：

- 客户端协议
- 上游协议
- 是否启用协议转换

客户端协议默认为 `Responses`。选择外部服务后，上游协议会默认带出该外部服务的请求协议，但仍可手动调整。

如果客户端协议和上游协议不一致，并且没有启用协议转换，保存时会提示风险。通常建议在协议不一致时启用协议转换。

典型场景：Codex 使用 Responses，本地 Key 绑定 DeepSeek Chat Completions 上游：

```text
客户端协议：Responses
上游协议：Chat Completions
启用协议转换：启用
```

本地 Key 页面会展示本地兼容 API Base URL，通常为：

```text
http://localhost:8716/v1
```

客户端使用方式：

```text
Base URL: http://localhost:8716/v1
API Key: 本地 Key 页面生成的 Key
Model: 本地模型名或外部服务支持的模型名
```

### API 对话测试

API 对话测试支持两种目标：

- 外部上游 API：使用外部服务中配置的请求协议。
- 本地兼容 API：使用本地 Key 中配置的客户端协议。

测试外部上游 API 时不需要选择本地 Key。测试本地兼容 API 时需要选择本地 Key，模型列表会根据该本地 Key 绑定的外部服务带出，也可以手动输入模型名。

如果模型命中了模型映射，最终转发到上游时会自动转换为上游模型名。

### Token 用量

Token 用量页面支持：

- 按外部服务统计
- 按上游模型统计
- 按日期统计
- 查看明细
- 日期筛选
- 快捷日期：本周、上周、本月、上月
- 大数字自动显示为 K/M

明细时间按本地服务器时间格式展示：

```text
2026-07-15 11:09:22
```

### 请求日志

请求日志记录网关请求的关键结果，包括：

- 来源：外部上游测试或本地兼容 API
- 外部服务
- 本地模型
- 上游模型
- 状态码
- Token
- 耗时
- 错误信息

页面内部使用滚动区域，避免数据较多时带动整个系统菜单一起滚动。

### 调试日志

登录后，页面右上角“调试日志”按钮会打开独立页面：

```text
http://localhost:8716/debug-logs.html
```

调试日志支持：

- 开启或关闭 API 调试日志
- 设置日志级别：`error`、`info`、`debug`、`trace`
- 开启或关闭请求体记录
- 开启或关闭响应体记录
- 设置 Body 最大记录字符数
- 设置查看行数，默认后 100 行
- 开启或关闭实时日志刷新
- 自动换行显示，且不影响保存内容
- 放大或还原日志编辑区
- 编辑当前加载的日志片段，并按 `Ctrl+S` 保存

保存日志时会用当前编辑内容替换原日志文件中本次加载的开始行到结束行。如果保存期间日志文件尾部有新增内容，新增内容会保留。

敏感字段会自动脱敏，例如：

- `api_key`
- `Authorization`
- `password`
- `secret`
- `token`

### 系统设置

系统设置包含：

- 监听地址
- 监听端口
- 是否启动后自动打开浏览器
- 系统日志级别

监听地址含义：

- `127.0.0.1`：只允许本机访问。
- `0.0.0.0`：监听所有网卡，局域网内其他设备可通过服务器 IP 访问。

保存监听地址或端口后，可以点击重载按钮立即切换监听配置，不需要重启 exe。如果端口被占用，重载会返回错误，原有服务继续保持运行。

## 本地兼容 API

客户端请求需要带本地 API Key：

```http
Authorization: Bearer <local-api-key>
```

支持的主要路径：

```text
GET  /v1/models
GET  /models
POST /v1/chat/completions
POST /chat/completions
POST /v1/responses
POST /responses
```

使用 `/v1` 作为 Base URL 时，OpenAI 兼容客户端通常会自动拼接具体路径。

## 协议转换说明

my_ai_sum 把协议配置拆成三个层次：

- 外部服务请求协议：描述该上游适合用哪个协议直接请求。
- 本地 Key 客户端协议：描述客户端用哪个协议请求 my_ai_sum。
- 本地 Key 上游协议：描述 my_ai_sum 转发到上游时用哪个协议。

当本地 Key 启用协议转换时，网关会根据客户端协议和上游协议做转换：

```text
Responses -> Responses
Responses -> Chat Completions
Chat Completions -> Chat Completions
Chat Completions -> Responses
```

流式与非流式都会处理。对于上游返回的 SSE 流式响应，如果当前请求按非流式处理，系统会尝试聚合出可用 JSON；如果无法聚合，会返回更明确的错误，提示是 SSE、HTML、空响应还是其他非 JSON 响应。

## Codex 配置示例

Codex 使用本地服务时，可以将 Base URL 指向 my_ai_sum：

```toml
model_provider = "custom"
model = "gpt-5.5"

[model_providers.custom]
name = "custom"
wire_api = "responses"
requires_openai_auth = true
base_url = "http://localhost:8716"
```

如果 my_ai_sum 监听局域网地址，也可以使用服务器 IP：

```toml
base_url = "http://192.168.x.x:8716"
```

同时需要让 Codex 使用 my_ai_sum 生成的本地 API Key。Codex 使用 `wire_api = "responses"` 时，本地 Key 的客户端协议建议配置为 `Responses`。

## DeepSeek 上游建议

DeepSeek 官方接口通常使用 Chat Completions 协议。推荐：

```text
外部服务请求协议：Chat Completions
本地 Key 客户端协议：Responses
本地 Key 上游协议：Chat Completions
启用协议转换：启用
```

这样 Codex 等 Responses 客户端可以通过 my_ai_sum 调用 DeepSeek 上游。

## Grok/xAI 说明

如果 Grok/xAI API Key 测试返回类似 `permission-denied`、`doesn't have any credits or licenses yet`，通常表示 xAI 控制台团队没有可用额度或许可证。这属于上游账号权限问题，不是 my_ai_sum 的协议转换或代理问题。

## 常见排查

### 外部服务返回 HTML

如果报错提示上游返回 HTML，通常表示 Base URL 填成了官网或控制台页面，而不是 OpenAI 兼容 API 地址。可尝试检查是否需要使用带 `/v1` 的 API 地址。

### 外部服务返回 SSE

如果非流式请求收到 `text/event-stream`，可能是上游强制返回流式，或请求协议、模型、参数不匹配。可以在 API 对话测试中开启流式，或调整外部服务请求协议。

### 本地代理测试失败

无账号密码的代理不需要填写用户名和密码。代理地址 `http://127.0.0.1:7890` 应配置为：

```text
类型：http
主机：127.0.0.1
端口：7890
用户名：空
密码：空
```

### Codex 连接本地服务失败

检查：

- my_ai_sum 是否已启动。
- Codex 的 `base_url` 是否指向 `http://localhost:8716` 或局域网 IP。
- 本地 Key 是否启用。
- Codex 使用的 API Key 是否为 my_ai_sum 生成的本地 Key。
- 本地 Key 客户端协议是否与 Codex `wire_api` 一致。
- 如果上游是 DeepSeek，是否启用了 `Responses -> Chat Completions` 协议转换。

## 开发验证

前端脚本语法检查：

```powershell
@'
const fs = require('fs');
for (const file of ['internal/web/static/app.js','internal/web/static/debug-logs.js']) {
  const code = fs.readFileSync(file,'utf8');
  new Function(code);
  console.log(`${file} syntax ok`);
}
'@ | node -
```

Go 测试：

```powershell
go test ./...
```

构建：

```powershell
go build -o build\my-ai-sum.exe ./cmd/my-ai-sum
```
