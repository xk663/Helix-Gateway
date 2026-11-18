# Helix Gateway 独立版

**截断检测与结构修复（Truncation Detection & Structural Repair）**。Helix 在本地转发 OpenAI 兼容的 Chat Completions，检测上游的 `length` 和缺失的结束标记，还原已收到的正文并补齐可验证的结构。反复撞到输出上限时，Helix 可以调整后续请求的显式 token 上限。流式续写默认关闭。

## Windows 启动

默认上游是 `http://127.0.0.1:8317/v1`，Helix 只监听 `127.0.0.1:8318`。启动上游后双击 `run.bat`。在 SillyTavern 选择 Chat Completion → Custom (OpenAI-compatible)，地址填 `http://127.0.0.1:8318/v1`，API Key 仍填上游使用的密钥。按 Ctrl+C 停止。

上游地址只有主机时自动使用 `/v1`。自定义路径前缀会保留，末尾斜杠会统一处理，上游前缀和客户端路径里的百分号编码也会保留。`/v1/` 下的 API 共用转发路由，包括模型列表、嵌入、图像生成、语音和重排。启用处理时，网关为模型列表、聊天补全、文本补全和嵌入保留本地 `OPTIONS` 预检，其余端点的预检交给上游。网关只为经过合成工具处理的聊天响应设置环回 CORS，其他上游响应保留原有 CORS 与 `Vary`。

读取客户端请求头最多等待 10 秒，读取整个请求最多等待 60 秒。空闲 keep-alive 连接最多等待 60 秒。每次实际 socket 写入最多阻塞 30 秒，HTTP 缓冲区最终刷出时也适用。一次写入结束后立即清除期限，避免干扰等待上游 SSE 和后续请求。

```powershell
.\helix-gateway.exe -upstream https://your-provider.example/v1 -port 8318
Invoke-RestMethod http://127.0.0.1:8318/healthz
Invoke-RestMethod http://127.0.0.1:8318/stats
Invoke-RestMethod 'http://127.0.0.1:8318/stats?by=model'
```

## 模式与模型

- **`adaptive`**（默认）按上游、模型和认证指纹独立学习。从 `repair-only` 开始，最近八次完整观测中累计三次上游 `length` 后，下一轮试用 `synthetic`。纯聊天连续三次未命中合成工具后，回到 `repair-only`。状态最多保留 256 个组合，保存在当前进程内。
- **`synthetic`** 给兼容的请求附加固定函数 `agw_emit_response`，从工具参数中增量还原正文。纯聊天默认通过 `tool_choice` 指定这个函数，客户端带工具时采用 `auto`。约束并入已有 system 内容或用户轮次。Gemini 请求以 assistant 结束时，仍会追加原有的 `Continue from the last assistant message.` 用户轮次。实际工具调用与前缀缓存命中由上游决定。显式工具选项（`auto` 除外）、结构化输出、语音输出及同名客户端工具按原样转发。
- **`repair-only`** 适合 RP。正文增量按原样传递，只在结尾追加可验证的 JSON 闭合符，在相应修复级别下也能闭合 Markdown 围栏或 XML 标签。请求保留原消息和工具配置，显式 token 上限可以按下述规则调整。
- **`off`** 直接转发 API 请求和预检。上游的 CORS、`Vary` 和压缩响应原样转发，常规 HTTP 代理规则仍适用。`aggressive` 是旧模式名，等同于合成工具模式并默认选用更积极的修复级别。

模型默认放行，包括 DeepSeek、Qwen、GLM、Kimi、Grok、`lmstudio/*`，以及 Bedrock 的 `anthropic.claude…` 和 ARN。常见嵌入、重排、语音、图像、视频及已知的纯 completion 模型会跳过。黑名单匹配完整 ID 和去掉提供方前缀的模型名，例如 `claude-*` 也匹配相应的 Bedrock Claude ID。模型 ID 按客户端传入值转发。

| 环境变量 | 默认值与用途 |
| --- | --- |
| `TRUNCATIONFIX_ENABLED` | `true`，总开关 |
| `TRUNCATIONFIX_WRAPPER_MODE` | `adaptive`，可选 `synthetic`、`repair-only`、`off`、`aggressive` |
| `TRUNCATIONFIX_ASSUME_NO_TOOLS` | `false`。设为 `1` 时，`adaptive` 停留在 `repair-only`，手动指定的合成工具模式直接转发原始请求 |
| `TRUNCATIONFIX_FORCE_TOOL_CHOICE` | `true`。设为 `0` 时，纯聊天也采用 `auto` |
| `TRUNCATIONFIX_MAX_TOKENS_CEILING` | `8192`，网关主动提高显式输出上限的最大值。设为 `off` 或 `0` 停止调整 |
| `TRUNCATIONFIX_CONTINUE` | `0`，关闭流式续写。设为 1 至 8 可限制附加请求的最大次数 |
| `TRUNCATIONFIX_CONTINUE_TOKENS` | `8192`，初始生成与续写共用的输出 token 预算 |
| `TRUNCATIONFIX_DISABLED_MODELS` | 空，逗号分隔的模型 ID 或通配符黑名单 |
| `TRUNCATIONFIX_MAX_LENGTH` | `65536` 字节，上限 4 MiB |
| `TRUNCATIONFIX_BUFFER_SIZE` | `4096` 字节 |
| `TRUNCATIONFIX_REPAIR_LEVEL` | `conservative` 修复 JSON，`moderate` 增加 Markdown，`aggressive` 增加 XML |

```powershell
$env:TRUNCATIONFIX_WRAPPER_MODE = 'repair-only'
$env:TRUNCATIONFIX_REPAIR_LEVEL = 'moderate'
.\run.bat
```

## 自适应输出上限

`synthetic` 和 `repair-only` 以端点、模型及认证指纹区分策略。同一实际预算下连续两次收到上游 `length` 后，后续请求的 `max_tokens` 或 `max_completion_tokens` 最多翻倍，受配置上限约束。仅当请求恰好包含一个有效的上限字段时才调整。结构化 400 明确给出输出上限时会记录该限制。上游报上下文窗口不足时，网关下调自己加出的预算，最多回到客户端原值。这类错误学习用于后续请求。

输出上限约束生成量。模型提前结束时，未生成的部分通常不会产生输出费用。模型用到了新增预算时，实际输出费用和等待时间可能增加，部分服务商还会依据请求上限预留额度。希望严格采用客户端预算时，将 `TRUNCATIONFIX_MAX_TOKENS_CEILING` 设为 `off`。

## 流式续写

设置 `TRUNCATIONFIX_CONTINUE=1` 可允许一次附加请求。只对单选项纯文本 SSE 生效，需要客户端指定有效的输出上限。上游明确返回 `length` 且正文已开始交付时，网关用已交付正文和续写指令请求下一段，保留原请求的输出上限、`stop` 和 `temperature`。各段接缝会去重，最后只发一次结束原因和 `[DONE]`。协议损坏、字符捕获超限、预算不足、超过 90 秒或续写上游拒绝时，网关保留已交付文本并用 `length` 收尾。

输出预算按上游返回的 completion tokens 扣减。缺少用量时，按该跳请求的输出上限扣减。只有每一跳都提供用量，且客户端请求了 `include_usage`，网关才会发合并后的用量。流结束时的 HTTP trailer `X-Helix-Continued` 回报已打开的续写流数，`/stats` 的 `continuation_hops` 可查看累计次数。

每次续写都会重发输入和已生成正文作为上下文。这部分输入可能重复计费，也可能增加等待时间或触发限流。输出预算只约束生成量。

## 失败处理和诊断

网关支持解码 gzip。合成模式下只对明确的结构化能力拒绝重试一次。上游拒绝网关添加的强制 `tool_choice` 时，同一轮保留工具和约束并改用 `auto`，记入 `upstream_rejected_forced_choice`。当上游明确表示不支持工具时，网关用原始请求重试并直接转发第二次响应。探测最多读取约 64 KiB，并恢复读过的原始字节。HTML、纯文本、无效工具参数和函数不存在均不触发重试。

同一上游、模型和认证指纹会缓存上述能力十分钟。拒绝强制选择时，后续纯聊天直接使用 `tools + auto`。拒绝工具并成功透传时，后续跳过合成工具注入，`adaptive` 仍执行结构修复。原始请求重试也返回 400 时，十分钟内只对相同请求停止重试。跳过注入会记入 `upstream_tool_retry_suppressed`。缓存最多保留 256 条指纹和到期时间。其他非 200 和未知响应类型在提交处理过的响应前返回 502，以避免回显内部工具。

`repair-only` 保留上游的 4xx 等非 200 响应，以及它无法处理的 Content-Type。对进入处理流程的 200 响应，gzip 头损坏（`upstream_gzip`）、编码不支持（`upstream_encoding`）或解码后的 JSON 超过 32 MiB（`upstream_body_limit`）仍返回 502。读取或解析失败也会返回网关错误。

SSE 已开始后遇到坏数据会依次发送错误事件、带 `finish_reason:"length"` 的 choice 和 `[DONE]`。若上游正常关闭但缺少 `[DONE]`，网关补发标记并单独累计 `stream_missing_done`。真实工具的下游索引按首次出现的顺序稳定分配。隐藏调用产生的空增量会抑制，usage 会保留。

工具参数中的顶层 `content` 位于后续字段时也会增量还原。较高修复级别识别三个以上反引号或波浪线包裹的 JSON。合并普通正文和工具正文时，网关检查完整开头的重复，以及 8 至 512 个 Unicode 字符的最长尾首重叠。普通正文可以提及内部工具名，保留名称出现在工具调用结构中仍会触发防护。

`GET /stats` 返回当前进程的累计聊天请求数、介入次数、还原的 UTF-8 字节数、结构修复次数和降级原因。`upstream_length_decisions` 统计上游明确返回的 `length`，`gateway_length_decisions` 统计网关改写或合成的 `length`，按选项计数。`length_decisions` 保留为两者之和。`guard_trips` 单独统计字节或缓冲上限、异常响应拦截和流协议保护等触发事件。一次请求可能触发多个防护。

网关补发的 SSE 终止统计在下游写入成功后计入。`stream_missing_done` 独立于 `fallback_reasons`。标准错误流记录同样的计数，重启会重置统计。

`GET /stats?by=model` 另外返回 `by_model`，当前进程最多保留 256 个模型。合成工具命中和未使用按 choice 计入 `tool_hits` 和 `tool_unused`。`upstream_calls_per_request` 是 `upstream_calls / requests`，可衡量重试放大。`upstream_length_rate` 是 `upstream_length_requests / observed_requests`，衡量完整观测到的回复中发生上游截断的请求比例。同一个请求的多个 choice 只计一次。分母为零时比例显示 0。直接透传的回复不计入 `observed_requests`。

`client_cap_suspected` 标记仍按客户端指定的上限（不超过 512 token）转发并收到上游 `length` 的请求，也会写入日志。`token_cap_increases` 记录尝试使用更高上限的请求。`recovered_bytes` 只记去重后新增的还原字节，`deduplicated_bytes` 记录裁掉的重复字节。上游未生成的字节数无法单凭响应估计。

## 构建与测试

需要 Go 1.26。

```powershell
go test ./...
go test ./cmd/gateway -run '^$' -fuzz '^FuzzChunk$' -fuzztime 20s
go build -o helix-gateway.exe ./cmd/gateway
```
