# OpenAI 数据类型化重构计划（减少类型断言）

## Context

项目 159 处 `.(map[string]any)` 断言中，大量集中在"明显是 OpenAI 格式"的入站数据上（chat 请求、messages、tools、images 请求）。这些数据从 server 层就以 map 流转，每层都要断言取值（`getMessagesList`、`resolveTools`、`getContentItemURL` 等辅助函数全是断言堆）。本次重构：**入站 OpenAI 数据一次性解析为类型化 struct，全链路传递；GLM 上游侧动态 JSON（SSE 事件、模型文本解析产物）保持 map**。

关键约束：
- import 链 server→glmclient→{translator,tools}，已验证 openai 叶子包无循环；GLM wire 类型放 translator 包
- 行为保持：GLM 侧 wire 输出不应变化（`GLMImageRef/GLMFileRef` tag 原样迁移，不加 omitempty）
- **基线预存失败**：translator_test.go 有 5 个测试因标注文案漂移失败（代码是 `（你未联网，要使用search相关工具）`，测试期望旧的 `（你没有联网）`），必须先修，否则无法区分重构引入的失败

## Step 0 — 修复基线测试（独立 commit）

`internal/translator/translator_test.go`：5 个失败测试的期望文案 `（你没有联网）` → `（你未联网，要使用search相关工具）`（约 L792/810/832/845/881）。
验收：`go test ./...` 全绿，作为后续基准。

## Step 1 — 新建 internal/openai 包 + logging 改进（纯增量）

新建 `internal/openai/openai.go`（零内部依赖，sonic 可用）：
- `ChatCompletionRequest{Model string; Messages []Message; Tools []Tool; ToolChoice json.RawMessage(未消费，仅透传 dump); Stream bool}`
- `Message{Role; Content Content; ToolCalls []ToolCall; ToolCallID; Name}`
- `Content` 多态（kind 枚举：Absent/Null/String/Parts/Raw）：
  - `UnmarshalJSON`：string→String；数组→Parts；null→Null；其他→Raw 存原始字节
  - `Text() string`：String 直返；Parts 拼 text（过滤空，`\n` join）；Raw→sonic 解码后 compact dump（不能用标准库 json，EscapeHTML 行为不同）；Null/Absent→""
  - 辅助构造器 `StringContent(s string)`
- `ContentPart{Type; Text omitempty; ImageURL *{URL,Detail omitempty}; FileURL *{URL}}`
- `Tool{Type; Function{Name,Description; Parameters json.RawMessage}}`
- `ToolCall{ID;Type;Function{Name;Arguments string}}`，`Arguments` 自定义 Unmarshal 兼容 string|object（object→sonic 紧凑字符串）
- `ImageGenerationRequest{Model,Prompt; N 柔性int(兼容 2.0); Size,ResponseFormat,Style,Scene}`
- `ImagesResponse{Created; Data []ImageData{URL/B64JSON/RevisedPrompt omitempty}}`
- `ChatCompletionChunk{ID,Object,Model; Created; Choices []ChunkChoice; Usage *Usage omitempty}`；`ChunkChoice{Index; Delta{Role,Content,ReasoningContent,ToolCalls omitempty}; FinishReason *string(null)}`；`ChunkToolCall{Index;ID omitempty;Type omitempty;Function *ToolCallFunction omitempty}`

`internal/logging/logging.go`：`SerializeForDebug` default 分支改为先 `sonic.MarshalString`、失败回退 `fmt.Sprintf("%v")`（否则 typed struct 的 DebugDump 是 Go 语法不可读）。

## Step 2 — 入站链路原子切换（tools+translator+glmclient+server，一个 commit）

- `internal/tools/protocol.go`：`ToolsToPrompt([]openai.Tool, map[string]bool)`；`SampleArgumentsFromSchema(name, json.RawMessage)`。Parameters 解码一次后走原逻辑。
- `internal/translator/translator.go`：
  - 新增 GLM wire 类型（自 failover.go L134-164 迁移 4 个 glm*Reference 结构体）：`GLMMessage{Role; Content []GLMContentPart}`、`GLMContentPart{Type text|image|file; Text omitempty; Image []GLMImageRef omitempty; File []GLMFileRef omitempty}`、`GLMImageRef/GLMFileRef`（**tag 原样、字段不加 omitempty**，保证 GLM wire 与 /files/references 响应形状不变）
  - `ConvertMessages([]openai.Message, []openai.Tool, map[string]bool) []GLMMessage`；`hasToolCalls` 改为 `len(msg.ToolCalls) > 0`；assistant 的 ToolCalls 转 map 形态喂给 SanitizeToolCalls（唯一残留适配点）；返回 `[]GLMMessage{{Role:"user", Content:[{Type:"text",Text:prompt+"\n\nAssistant: "}]}}`
  - `ExtractRecentUserURL([]openai.Message)`；`buildToolParamTypeMap/extractToolNames` 改 `[]openai.Tool`
  - 删除 `ExtractTextContent(content any)`（调用点全部改为 `Content.Text()`）
  - `SanitizeToolCalls` 第三参改 `[]openai.Tool`（输入保持 []map —— GLM 侧产物）；`NewGLMEventAccumulator.ToolsList` 改 `[]openai.Tool`；**ConsumeEvent/Finalize/chunkJSON 本步不动**
- `internal/glmclient/chat.go`：`StreamChatCompletion(ctx, req *openai.ChatCompletionRequest)`；`req.Model==""` 返回错误（替代原 L271 未检查断言 panic→500 变 400）；`resolveTools` 只算一次传参；refs 合并简化为 `convertedMessages[0].Content = append(refs, convertedMessages[0].Content...)`；`getMessagesList` 调用点删除
- `internal/glmclient/failover.go`：删 `glm*Reference` 4 个结构体、`getMessagesList`、`getContentItemURL`；`uploadReferencedFiles(ctx, []openai.Message) []*translator.GLMContentPart`（遍历 `msg.Content.Parts`，image_url→`part.ImageURL.URL`，file→`part.FileURL.URL`）；`UploadFileReference` 返回 `*translator.GLMContentPart`
- `internal/server/server.go`：`handleChatCompletions` 改 `io.ReadAll`+`sonic.Unmarshal` 进 typed struct（DebugDump 保真原始字节；gin binder 对自定义 UnmarshalJSON 兼容但统一走 sonic），400 错误格式不变
- 测试适配：`TestConvertMessagesAnnotatesUserURL`（typed 输入，断言 `result[0].Content[0].Text`）；`TestExtractTextContentFiltersEmpty` 改测 `Content.Text()`

## Step 3 — 出站 chunk 类型化（仅 translator，独立 commit）

- `chunkJSON(patch map)` → `marshalSSE(chunk openai.ChatCompletionChunk)`（`data: {...}\n\n`，失败回退 `data: {}\n\n`）
- `ConsumeEvent` 三处、`Finalize` 四处 patch 改 typed（intervene/role-only/tool_calls 循环/收尾+usage）；`serverSideToolCalls`/sanitize 输出仍是 map，在构建 delta 处转 `openai.ChunkToolCall`
- 已知且可接受：SSE 键序从 sonic map 字母序变为字段声明序（JSON 键序无语义，测试均按解析断言）

## Step 4 — 图片链路类型化

- `server.go` handleImagesGenerations 同 Step 2 模式
- `image.go`：`GenerateImages(ctx, *openai.ImageGenerationRequest) (*openai.ImagesResponse, error)`；`openImageStream/buildImagesResponse/resolveImageStyle/resolveImageScene` 全 typed；`n` clamp [1,10]；删 `getModelName`、`coercePositiveInt`（确认无调用者）

## Step 5 — 清理

- 删 `var _ = sort.Strings` 及 sort import（failover.go）
- `rg` 确认 `getMessagesList|resolveTools|getContentItemURL|ExtractTextContent|coercePositiveInt|getModelName` 清零
- `gofmt -l .`；确认 openai 包零内部 import

## 验证

1. 每步：`go build ./... && go vet ./... && go test ./...` 全绿
2. wire 回归：开 DEBUG_DUMP_ALL，重构前后二进制对同一请求 dump"转发到 GLM 的 chat 原始请求体"，`jq -S` 归一化后 diff 应为空
3. 手工 curl：基础流式（首 chunk 含 role、收尾含 finish_reason+usage）；image_url parts 触发上传且 ref 在 content 首位；工具回路（tool_calls arguments 用 object 形态 + tool 结果消息 → [function_calls]/tool_result 块）；`/files/references` 响应 `{"type":"image","image":[...]}` 形状不变；`/images/generations` url 与 b64_json 两种格式；边界 `{"model":123}`→400、`"content":null` assistant+tool_calls 正常转换
4. DEBUG 日志确认 typed 结构 dump 为 JSON 可读

## 风险与回滚

每步独立 commit，可单独 `git revert`；Step 2 面最大（5 文件+测试），提交前做 wire diff。绑定严格化使原"静默容错"的畸形请求变 400（如 messages 非数组），属预期改善。仓库根目录已有重构前编译产物 `glm2api` 可作热备。
