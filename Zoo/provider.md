# DeepSeek Provider 模块设计文档

> 位置：`internal/provider`（含 `internal/provider/retry`）
> 状态：**完整实现**，已通过单测（含 `-race`）与 `go vet`。

## 1. 定位与职责

Provider 是**传输层**：负责对 DeepSeek 兼容接口（OpenAI 兼容协议）的 Chat Completions 封装，向上层提供"非流式 / 流式"两种能力。重试、超时、SSE 解析、错误归一化全部在这一层收敛，业务层完全不感知。

依赖方向（严格单向，无循环）：

```
main(组合根) → provider → config
                   ↘ retry（provider 的子包）
```

- 配置以 `*config.Config` **构造注入**，不在 provider 内部 `os.Getenv`。
- retry 是 provider 的辅助实现，放在 `provider/retry`（刻意去掉了 internal 嵌套，扁平化）。

## 2. 整体轮廓

```
internal/provider/
├── types.go        # DTO：ChatRequest/Message/Thinking/ChatResponse/Usage/APIError + 内部 chatPayload
├── callbacks.go    # StreamCallbacks（nil-safe 回调字段）
├── provider.go     # Completer/Streamer 接口 + Client + New + Option + Complete + 编译期断言
├── stream.go       # Stream（SSE 解析 + 空闲超时 + streamCtx 贯穿）
├── retry/
│   └── retry.go    # Policy/Default/Do（建连阶段重试）
├── provider_test.go
└── retry/retry_test.go
```

## 3. 核心抽象

### 3.1 双层能力接口（降级关系）

```text
type Completer interface { Complete(ctx, req *ChatRequest) (*ChatResponse, error) }
type Streamer interface { Completer; Stream(ctx, req *ChatRequest, cbs StreamCallbacks) error }
var _ Completer = (*Client)(nil)   // 编译期断言（"虚实现"）
var _ Streamer = (*Client)(nil)
```

- `Completer` 是最小能力面；`Streamer` 是它的超集（内嵌）。
- **降级语义**：接口是给**消费方**的依赖声明，不是给实现的。消费方声明 `Completer`，未来换一个"只支持非流式"的后端时无需改动；想用流式的消费方用类型断言 `p.(Streamer)` 探测。
- 当前唯一实现 `*Client` 两种能力都有，所以降级"看不见"——这是接口设计层属性，等能力不全的实现出现才显形。

### 3.2 Client 与构造注入

`New(cfg *config.Config, opts ...Option) *Client`，配置以函数式选项扩展而不破坏签名：

| Option | 作用 |
|---|---|
| `WithBaseURL` | 覆盖接口基址 |
| `WithHTTPClient` | 注入自定义 client（测试、自定义 Transport） |
| `WithRetryPolicy` | 覆盖重试策略 |
| `WithRequestTimeout` | 非流式整请求超时（默认 60s） |
| `WithIdleTimeout` | 流式每读空闲超时（默认 30s） |

### 3.3 DTO 打包

- `ChatRequest`（model/messages/thinking/reasoning_effort/max_tokens/temperature/…）——字段与请求体一一对应。
- `Message` 含 `ReasoningContent`（非流式响应的思考过程）。
- `ThinkingEnabled()/ThinkingDisabled()` 辅助构造。
- `APIError{Status, Message}`——从 OpenAI 兼容错误格式 `{"error":{"message":...}}` 归一化，调用方可 `errors.As`。
- **内部 `chatPayload`**：嵌入 `ChatRequest` 并补 `stream` 开关——对外 DTO 保持纯净，发送时注入，不暴露。

## 4. 流式设计

- **回调形态**：`StreamCallbacks`（`OnThinking`/`OnContent`/`OnDone`，nil-safe），回调返回非 nil error 立即中止流；**不设 `OnError`**，流中错误只走 `Stream` 返回值，避免状态分散。
- **SSE 解析**：`bufio.Scanner` 按行读，识别 `data:` 前缀与 `[DONE]` 结束标记；EOF 无 `[DONE]` 宽容视为正常结束（触发 `OnDone`）。
- **streamCtx 贯穿建连与流中**（关键设计，见踩坑记录）：流式请求必须绑定 `streamCtx`，空闲超时/外部取消才能中断 transport 的读。
- **空闲超时**：`time.AfterFunc` + 每次读到数据 `Reset`，触发则取消整个流上下文，返回 `ErrStreamIdle`（用 `atomic.Bool` 标记区分空闲超时与外部取消）。

## 5. 重试设计（retry 子包）

- **两阶段原则**：`retry.Do` 只覆盖**建连**（build → send → 2xx 状态检查）。拿到 2xx 进入流中后**绝不重试**——否则已吐出的 token 会重复。
- **全抖动退避**：`sleep = random(0, min(BaseDelay * 2^(attempt-1), MaxDelay))`，尊重服务端 `Retry-After`（受 `MaxDelay` 封顶），退避期间可被 `ctx` 取消。
- **默认策略** `Default()`：4 次尝试、500ms 起退、单次最长 30s；可重试 = 408/429/全部 5xx（529 是 Cloudflare 私有扩展码，天然落在 5xx 区间，无需单列）。
- **网络错误不重试**：重试条件严格限定为状态码判定（`ShouldRetry` 谓词可配置）。
- 耗尽返回 `ExhaustedError{Status}`，保留最后一次状态码。

## 6. 超时分层

| 场景 | 策略 |
|---|---|
| 非流式 | `http.Client.Timeout` 整请求超时（默认 60s） |
| 流式 | 刻意 `cloneClientWithoutTimeout`（Timeout=0），否则长生成会被整请求超时误杀；改由两层兜底：① 每读空闲超时；② 外部 ctx 取消 |

## 7. 重点取舍（决策记录）

| 问题 | 决策 | 理由 |
|---|---|---|
| 接口组织 | 双层能力接口（Completer ⊂ Streamer） | 按能力分级，消费方按最小依赖面声明，天然支持降级 |
| 流式 API 形态 | 回调 struct（nil-safe 字段）而非 channel | 错误传播自然、无需手动管理 channel、避免泄漏 |
| 重试状态码 | 408/429/5xx（529 归 5xx） | 529 非 IANA 注册码，是 Cloudflare 私有扩展，属 5xx 区间 |
| 回调错误语义 | 返回 error 中止流，无 OnError | 流中错误单一通道（返回值），不设回调双通道 |
| 重试边界 | retry 只包建连，2xx 后零重试 | 避免重复已吐 token |
| 流式超时 | 去掉整请求 Timeout，用空闲超时 + ctx | 长生成（数分钟）不被整请求超时误杀 |
| 组织 | config/provider 入 `internal/`，retry 扁平化到 `provider/retry` | 应用非库，internal 零成本；扁平更直观 |
| 编译期保证 | `var _ Completer/Streamer = (*Client)(nil)` | 实现漂移在编译期暴露，而非运行时断言 |

## 8. 测试设计与踩坑记录

### 8.1 测试方法

- `httptest.Server` 全链路：真实 HTTP 请求/响应，服务端 handler 断言请求头与 payload。
- 重试测试注入 `noRetry`（`MaxAttempts:1`）保证确定性；退避测试用小 delay。
- 流中类测试（取消/空闲超时/回调中止）用**挂起的 handler**（`<-r.Context().Done()`）锁定时序。
- 白盒测试 retry（`package retry`）以访问未导出的 `backoff`。

### 8.2 踩坑记录（必读）

**现象**：`TestStream_IdleTimeout` 挂死，`panic: test timed out after 30s`，`Scanner.Scan()` 永久阻塞。

**根因**：HTTP 请求绑定的是**外层 `ctx`**，而 `streamCtx` 是拿到 2xx 响应后**才创建**的。`http.Transport` 只监听 `req.Context()`——空闲超时取消 `streamCtx` 时，transport 的读毫无感知，永远等不到数据。

**修复**：把 `streamCtx` 的创建提前到发请求之前，请求绑定 `streamCtx`（它继承外部 ctx，外部取消依然生效；空闲计时器仍只在 2xx 后启动，不影响建连/退避）。

**教训（后续不踩坑）**：
1. **要中断 HTTP 读，取消必须作用在 transport 监听的 ctx（即 `req.Context()`）上**——"建一个新的 context 再 cancel"是无效操作。
2. 超时/取消类测试必须用挂起的 handler 控制时序，否则 server 先 EOF 会让断言飘。
3. 流式场景坚守"2xx 后零重试"，失败只能透传错误由调用方决定重试整个对话。
4. 测试要覆盖边界路径（超时、取消、非法 chunk、流中断），不能只写 happy path——正是这个测试逼出了真实 bug。

## 9. 后续开发标准（工程一致性）

1. **依赖注入**：所有外部依赖（配置、HTTP client、策略）经构造函数注入，组合根（main）只做装配；禁止包级可变全局状态、禁止隐式 `os.Getenv`。
2. **接口定义在使用方**：provider 只导出自己的能力接口，消费方按需声明最小接口。
3. **DTO 打包**：请求/响应一律 struct 化，内部字段（如 `stream` 开关）不进对外 DTO。
4. **错误处理**：只在系统边界（HTTP 响应、输入）防御；内部契约（如"err 非 nil 时 resp 为 nil"）信任而非重复防御；错误用 `errors.Is/As` 判等。
5. **Option 模式**：新增配置用函数式选项，不破坏已有签名。
6. **测试**：`go test -race ./...`、`go vet`、`gofmt` 全绿；断言用 `errors.Is/As`；确定性优先（禁用真实网络/长退避）。
7. **注释中文**，关键设计点（为什么这么做）写进注释，而非只写"是什么"。

## 10. 已知扩展点

- 网络错误重试（`ShouldRetryErr` 谓词）。
- 完整工具调用（function calling）字段。
- `ResponseFormat`（json_object）等生成式能力的完整覆盖。
