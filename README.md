# Helix Gateway

Windows x64 本地代理。默认上游是 `http://127.0.0.1:8317/v1`，代理只监听 `127.0.0.1:8318`。先按下方命令构建，启动上游后双击 `run.bat`。SillyTavern 选择 Chat Completion → Custom (OpenAI-compatible)，地址填 `http://127.0.0.1:8318/v1`，API Key 填上游本来使用的密钥。网关转发客户端凭据。按 Ctrl+C 停止。

其他上游可在 PowerShell 中启动。

```powershell
.\helix-gateway.exe -upstream https://your-provider.example/v1 -port 8318
Invoke-RestMethod http://127.0.0.1:8318/healthz
```

网关处理 `POST /v1/chat/completions` 并转发 `GET /v1/models`。如果上游 CLIProxyAPI 自带抗截断，使用普通模型 ID 以避免重复包装。上游模型需要支持函数工具。

| 环境变量 | 默认值 |
| --- | --- |
| `TRUNCATIONFIX_ENABLED` | `true` |
| `TRUNCATIONFIX_WRAPPER_MODE` | `synthetic`，也支持 `off`、`aggressive` |
| `TRUNCATIONFIX_ENABLED_MODELS` | `gemini-*,claude-*,gpt-*,o1-*` |
| `TRUNCATIONFIX_DISABLED_MODELS` | 可填写逗号分隔的模型名或通配符。常见非文本模型始终跳过 |
| `TRUNCATIONFIX_MAX_LENGTH` | `65536` 字节，最高 4 MiB |
| `TRUNCATIONFIX_BUFFER_SIZE` | `4096` 字节 |
| `TRUNCATIONFIX_REPAIR_LEVEL` | `conservative`。支持 `moderate`、`aggressive` |

在启动程序的 PowerShell 里用 `$env:TRUNCATIONFIX_ENABLED_MODELS='gemini-*,我的别名'` 修改白名单。禁用规则优先。显式固定工具的请求、结构化输出请求和常见非文本模型按原样转发。普通回复也按原样转发。修复级别依次增加 JSON 闭合、Markdown 围栏闭合、XML 风格标签闭合。可恢复的正文会尽早发出，达到长度上限或传输参数不完整时以 `length` 标记。实际生成长度仍由上游模型和接口决定。

独立版用 Go 1.26 编译，源码与测试同目录。从仓库根目录运行以下命令。

```powershell
go test ./...
go build -o helix-gateway.exe ./cmd/gateway
```
