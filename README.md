# my_ai_sum

本项目是一个本地 OpenAI 兼容 API 聚合网关。它可以管理外部中转站、DeepSeek、Grok/xAI 等上游 API Key，并统一暴露 `/v1/models` 和 `/v1/chat/completions`。

## 功能

- 前后端一体，Go embed 内置管理界面。
- 支持 Provider Key、代理、模型映射、本地 API Key 管理。
- 支持 API 对话测试，分别测试外部上游和本地兼容 API。
- 保存请求日志和 Token 用量，包含输入、输出和总量。
- 支持流式 Chat Completions 透传。

## 开发启动

```powershell
.\scripts\dev.ps1
```

默认地址：

```text
http://127.0.0.1:8716/
```

首次启动会进入初始化页面。

## 构建 exe

```powershell
.\scripts\build.ps1
```

输出文件：

```text
build\my-ai-sum.exe
```

## 数据目录

默认数据目录在 exe 同级 `data` 文件夹。可通过环境变量指定：

```powershell
$env:MY_AI_SUM_DATA_DIR="D:\WorkSpace\Python\my_ai_sum\data"
```
