# Helix Gateway 独立版

**截断检测与结构修复（Truncation Detection & Structural Repair）**。Helix 在本地转发 OpenAI 兼容的 Chat Completions，检测上游的 `length` 和缺失的结束标记，尽量从已收到的回复中还原正文，按配置补上能安全闭合的结构。它**不提升 `max_tokens`、不续写**。

## Windows 启动

默认上游是 `http://127.0.0.1:8317/v1`，Helix 只监听 `127.0.0.1:8318`。先按下方命令构建，启动上游后再双击 `run.bat`。在 SillyTavern 选择 Chat Completion → Custom (OpenAI-compatible)，地址填 `http://127.0.0.1:8318/v1`，API Key 仍填上游使用的密钥。按 Ctrl+C 停止。

上游地址只有主机时自动使用 `/v1`。自定义路径前缀会保留，末尾斜杠会统一处理，上游前缀和客户端路径里的百分号编码也会保留。`GET /v1/models`、`POST /v1/completions` 和 `POST /v1/embeddings` 直接转发。启用处理时，网关为环回来源提供 `OPTIONS` 预检。合成工具模式可为环回请求设置 CORS 响应头，仅修复模式保留实际上游响应的 CORS 与 `Vary`。

读取客户端请求头最多等待 10 秒，读取整个请求最多等待 60 秒。空闲 keep-alive 连接最多等待 60 秒。每次实际 socket 写入最多阻塞 30 秒，HTTP 缓冲区最终刷出时也适用。一次写入结束后立即清除期限，避免干扰等待上游 SSE 和后续请求。

```powershell
.\helix-gateway.exe -upstream https://your-provider.example/v1 -port 8318
Invoke-RestMethod http://127.0.0.1:8318/healthz
Invoke-RestMethod http://127.0.0.1:8318/stats
```

## 模式与模型

- **`synthetic`**（默认）给兼容的请求附加固定函数 `agw_emit_response`，从工具参数中增量还原正文。约束并入已有 system 内容或用户轮次。Gemini 请求以 assistant 结束时，仍会追加原有的 `Continue from the last assistant message.` 用户轮次。固定工具名和定义可以让重复请求保留相同的前缀，实际缓存命中由上游决定。显式工具选项、结构化输出、语音输出及同名客户端工具会按原样转发。
- **`repair-only`** 适合 RP。原样发送请求和正文增量，只在结尾追加可验证的 JSON 闭合符，在相应修复级别下也能闭合 Markdown 围栏或 XML 标签。这个模式不注入工具。
- **`off`** 直接转发 API 请求和预检。上游的 CORS、`Vary` 和压缩响应原样转发，常规 HTTP 代理规则仍适用。`aggressive` 是旧模式名，等同于合成工具模式并默认选用更积极的修复级别。

模型默认放行，包括 DeepSeek、Qwen、GLM、Kimi、Grok、`lmstudio/*`，以及 Bedrock 的 `anthropic.claude…` 和 ARN。常见嵌入、重排、语音、图像、视频及已知的纯 completion 模型会跳过。黑名单匹配完整 ID 和去掉提供方前缀的模型名，例如 `claude-*` 也匹配相应的 Bedrock Claude ID。模型 ID 按客户端传入值转发。

| 环境变量 | 默认值与用途 |
| --- | --- |
| `TRUNCATIONFIX_ENABLED` | `true`，总开关 |
| `TRUNCATIONFIX_WRAPPER_MODE` | `synthetic`，可选 `repair-only`、`off`、`aggressive` |
| `TRUNCATIONFIX_ASSUME_NO_TOOLS` | `false`。设为 `1` 时，合成工具模式直接转发原始请求，`repair-only` 仍可正常修复 |
| `TRUNCATIONFIX_DISABLED_MODELS` | 空，逗号分隔的模型 ID 或通配符黑名单 |
| `TRUNCATIONFIX_MAX_LENGTH` | `65536` 字节，上限 4 MiB |
| `TRUNCATIONFIX_BUFFER_SIZE` | `4096` 字节 |
| `TRUNCATIONFIX_REPAIR_LEVEL` | `conservative` 修复 JSON，`moderate` 增加 Markdown，`aggressive` 增加 XML |

```powershell
$env:TRUNCATIONFIX_WRAPPER_MODE = 'repair-only'
$env:TRUNCATIONFIX_REPAIR_LEVEL = 'moderate'
.\run.bat
```

## 失败处理和诊断

网关支持解码 gzip。注入合成工具后，仅当上游返回 400 并明确表示不支持工具调用或 `tools` 参数时，才用原始请求重试一次，并直接转发第二次响应。探测最多读取约 64 KiB 并恢复读过的原始字节。参数无效、`tool_choice` 错误和函数不存在均不触发重试。

第一次重试会记入 `upstream_rejected_tools`。若第二次也返回 400，接下来两分钟内相同请求会直接透传并记入 `upstream_tool_retry_suppressed`。缓存最多保留 256 条，仅存请求指纹和到期时间。已知上游不兼容时可预先设置 `TRUNCATIONFIX_ASSUME_NO_TOOLS=1`，网关保留客户端已有的工具定义，并记入 `assume_no_tools`。其他非 200 和未知响应类型在提交响应前返回 502，避免回显内部工具。

`repair-only` 保留上游的 4xx 等非 200 响应，以及它无法处理的 Content-Type。对进入处理流程的 200 响应，gzip 头损坏（`upstream_gzip`）、编码不支持（`upstream_encoding`）或解码后的 JSON 超过 32 MiB（`upstream_body_limit`）仍返回 502。读取或解析失败也会返回网关错误。

SSE 已开始后遇到坏数据会依次发送错误事件、带 `finish_reason:"length"` 的 choice 和 `[DONE]`。若上游正常关闭但缺少 `[DONE]`，网关补发标记并单独累计 `stream_missing_done`。真实工具的下游索引按首次出现的顺序稳定分配。隐藏调用产生的空增量会抑制，usage 会保留。

工具参数中的顶层 `content` 位于后续字段时也会增量还原。较高修复级别识别三个以上反引号或波浪线包裹的 JSON。非流式回复会保留普通前言，再追加从工具参数还原出的正文。

`GET /stats` 返回当前进程的累计聊天请求数、介入次数、还原的 UTF-8 字节数、结构修复次数和降级原因。`upstream_length_decisions` 统计上游明确返回的 `length`，`gateway_length_decisions` 统计网关改写或合成的 `length`，按选项计数。`length_decisions` 保留为两者之和。`guard_trips` 单独统计字节或缓冲上限、异常响应拦截和流协议保护等触发事件。一次请求可能触发多个防护。

网关补发的 SSE 终止统计在下游写入成功后计入。`stream_missing_done` 独立于 `fallback_reasons`。标准错误流记录同样的计数，重启会重置统计。

## 构建与测试

需要 Go 1.26。

```powershell
go test ./...
go test ./cmd/gateway -run '^$' -fuzz '^FuzzChunk$' -fuzztime 20s
go build -o helix-gateway.exe ./cmd/gateway
```
