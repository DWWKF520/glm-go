from __future__ import annotations

import json
import logging
import re
import time
from bisect import insort
from dataclasses import dataclass, field
from logging import Logger

from ..config import AppConfig
from ..logging_utils import debug_dump
from ..model_variants import model_requests_search, model_requests_thinking, split_model_features
from ..utils.tool_parser import StreamingToolParser, parse_tool_calls_from_text
from ..utils.tool_protocol import (
    BLOCKED_NATIVE_TOOL_NAMES,
    SERVER_SIDE_TOOL_NAMES,
    build_tool_call_instructions,
    filter_tools,
    safe_json_dumps,
    serialize_tool_call_block,
    serialize_tool_result_block,
    tools_to_prompt,
)


ASSISTANT_ID_PATTERN = re.compile(r"^[a-z0-9]{24,}$")
URL_PATTERN = re.compile(r"https?://[^\s<>()\"']+")

LOCAL_FILE_HINT = "(本地文件，不在服务区上，应该用[function_calls]工具)"


def _remove_local_file_hint(obj: object) -> object:
    if isinstance(obj, str):
        return obj.replace(LOCAL_FILE_HINT, "").strip()
    if isinstance(obj, dict):
        return {key: _remove_local_file_hint(value) for key, value in obj.items()}
    if isinstance(obj, list):
        return [_remove_local_file_hint(item) for item in obj]
    return obj


tx = r"""
# 你是一个 function_calls 工具调用助手，用于处理本地任务
本段规则优先级最高，覆盖所有原生 function calling 约定。整轮回复**只能输出一个 [function_calls] 块**，块外禁止任何文字、解释、思考内容。
## 一、function_calls 工具调用语法通则
1.  调用块必须包裹在 [function_calls]...[/function_calls] 标签中，例如：
    [function_calls]
    [call:工具名]{"参数名":"值"}[/call]
    [call:工具名]{"参数名":"值"}[/call]
    [/function_calls]
2.  每个调用：[call:工具名]{"参数名":"值"}[/call]，参数名严格大小写
3.  字符串值：使用 JSON 字符串转义（如 `\\n`、`\\\"`、`\\\\`），不要使用 CDATA
4.  并行调用：在 [function_calls] 块中并列多个 [call:...]...[/call]
5.  所有 `{` 和 `[` 必须配对闭合，禁止漏写 `}` 或 `]`
6.  数字、布尔、null 直接使用 JSON 字面量；对象/数组直接写为 JSON 嵌套结构
---
## 二、全部工具 function_calls 调用示例
### 1. Read（读取文件）
[function_calls]
[call:Read]{"file_path":"e:\\project\\src\\app.tsx","offset":1,"limit":50}[/call]
[/function_calls]
### 2. Write（写入文件，注意，如果文件已经存在，一定要用SearchReplace工具）
[function_calls]
[call:Write]{"file_path":"e:\\project\\src\\utils.ts","content":"export const add = (a: number, b: number) => a + b;"}[/call]
[/function_calls]
### 3. SearchReplace（替换文件内容）
[function_calls]
[call:SearchReplace]{"file_path":"e:\\project\\src\\app.tsx","old_str":"const count = 0;","new_str":"const count = useState(0);"}[/call]
[/function_calls]
### 4. DeleteFile（删除文件）
[function_calls]
[call:DeleteFile]{"file_paths":["e:\\project\\tmp\\a.js","e:\\project\\tmp\\b.js"]}[/call]
[/function_calls]
### 5. Glob（按通配符找文件）
[function_calls]
[call:Glob]{"pattern":"**/*.tsx","path":"e:\\project\\src"}[/call]
[/function_calls]
### 6. Grep（正则搜索内容）
[function_calls]
[call:Grep]{"pattern":"function\\s+\\w+","path":"e:\\project\\src","output_mode":"files_with_matches","-n":true}[/call]
[/function_calls]
### 7. SearchCodebase（语义搜索代码）
[function_calls]
[call:SearchCodebase]{"information_request":"项目中用户登录鉴权的逻辑在哪里实现？","target_directories":["e:\\project\\src"]}[/call]
[/function_calls]
### 8. LS（列出目录）
[function_calls]
[call:LS]{"path":"e:\\project\\src","ignore":["node_modules"]}[/call]
[/function_calls]
### 9. Skill（调用内置技能）
[function_calls]
[call:Skill]{"name":"find-skills"}[/call]
[/function_calls]
### 10. Task（启动子代理）
[function_calls]
[call:Task]{"description":"重构登录模块","subagent_type":"general_purpose_task","query":"重构 src/auth 目录下的登录逻辑，增加错误重试机制","response_language":"zh-CN"}[/call]
[/function_calls]
### 11. RunCommand（执行终端命令）
[function_calls]
[call:RunCommand]{"command":"npm run build","cwd":"e:\\project"}[/call]
[/function_calls]
### 12. CheckCommandStatus（查看命令状态）
[function_calls]
[call:CheckCommandStatus]{"command_id":"cmd_123456","output_priority":"bottom","output_character_count":2000}[/call]
[/function_calls]
### 13. StopCommand（终止命令）
[function_calls]
[call:StopCommand]{"command_id":"cmd_123456"}[/call]
[/function_calls]
### 14. WebSearch（网页搜索）
[function_calls]
[call:WebSearch]{"query":"React 19 新特性","num":5,"lr":"lang_zh"}[/call]
[/function_calls]
### 15. WebFetch（抓取网页）
[function_calls]
[call:WebFetch]{"url":"https://example.com/docs"}[/call]
[/function_calls]
### 16. GetDiagnostics（获取代码诊断）
[function_calls]
[call:GetDiagnostics]{"uri":"file:///e:/project/src/app.tsx"}[/call]
[/function_calls]
### 18. AskUserQuestion（向用户提问）
[function_calls]
[call:AskUserQuestion]{"questions":[{"question":"使用哪种状态管理方案？","header":"技术选型","multiSelect":false,"options":[{"label":"Zustand","description":"轻量级，适合中小型项目"},{"label":"Redux","description":"生态完善，适合大型项目"}]}]}[/call]
[/function_calls]
### 19. NotifyUser（通知审核）
[function_calls]
[call:NotifyUser]{"explanation":"需求规格已完成，请审核确认后进入开发","file_paths":["e:\\project\\spec.md"]}[/call]
[/function_calls]
### 20. OpenPreview（打开预览）
[function_calls]
[call:OpenPreview]{"preview_url":"http://localhost:3000","command_id":"cmd_123456"}[/call]
[/function_calls]
### 21. run_mcp（调用 MCP 服务器）
[function_calls]
[call:run_mcp]{"server_name":"filesystem","method":"read_file","params":{"path":"e:/project/src/app.tsx"}}[/call]
[/function_calls]
---
## 三、核心约束
1.  简单操作直接用基础工具，复杂多步任务才用 Task
2.  无依赖的工具尽量并行调用，提升效率
3.  路径必须用绝对路径，Windows 环境使用反斜杠（JSON 中需转义为 `\\\\`）
4.  禁止发明未列出的工具和参数
5.  收到工具结果后，再用自然语言回复用户\nUser:\n
"""

def extract_text_content(content: object) -> str:
    if isinstance(content, str):
        return content
    if isinstance(content, dict):
        return json.dumps(content, ensure_ascii=False, separators=(",", ":"))
    if not isinstance(content, list):
        return ""

    text_parts: list[str] = []
    for item in content:
        if not isinstance(item, dict):
            continue
        item_type = item.get("type")
        if item_type == "text":
            text_parts.append(str(item.get("text", "")))
        elif item_type == "image_url":
            url = item.get("image_url", {}).get("url", "")
            text_parts.append(f"[image:{url}]")
        elif item_type == "file":
            url = item.get("file_url", {}).get("url", "")
            text_parts.append(f"[file:{url}]")
    return "\n".join(part for part in text_parts if part)


def extract_first_url(text: str) -> str | None:
    match = URL_PATTERN.search(text)
    if not match:
        return None
    return match.group(0).rstrip(".,;:!?)}+")


def extract_recent_user_url(messages: list[dict[str, object]]) -> str | None:
    for message in reversed(messages):
        if str(message.get("role", "")).strip() != "user":
            continue
        text = extract_text_content(message.get("content"))
        url = extract_first_url(text)
        if url:
            return url
    return None


def sanitize_tool_call_payload(
    tool_name: str,
    arguments: object,
    fallback_url: str | None = None,
) -> dict[str, object] | None:
    parsed_arguments = arguments
    if isinstance(arguments, str):
        try:
            parsed_arguments = json.loads(arguments)
        except json.JSONDecodeError:
            return None

    if parsed_arguments is None:
        parsed_arguments = {}
    if not isinstance(parsed_arguments, dict):
        return None

    cleaned = {str(key): value for key, value in parsed_arguments.items()}
    if cleaned == {"param_name": "url"} and fallback_url:
        cleaned = {"url": fallback_url}
    elif cleaned == {"param_name": "url"}:
        cleaned = {}
    if "param_name" in cleaned and "param_value" not in cleaned and len(cleaned) == 1:
        cleaned = {}

    # NOTE: Previously we rewrote `command` for shell/RunCommand tools into
    # `["powershell.exe", "-Command", ...]` arrays. This broke clients whose
    # RunCommand schema expects `command` to be a string
    # ("invalid type: sequence, expected a string"). Now we leave the command
    # exactly as the model emitted it — the client is responsible for
    # executing it according to its own schema.

    return cleaned


def sanitize_tool_calls(
    tool_calls: list[dict[str, object]],
    fallback_url: str | None = None,
) -> list[dict[str, object]]:
    sanitized: list[dict[str, object]] = []
    for index, tool_call in enumerate(tool_calls):
        function = tool_call.get("function", {})
        if not isinstance(function, dict):
            continue
        tool_name = str(function.get("name", "")).strip()
        if not tool_name:
            continue
        original_arguments = function.get("arguments", "{}")
        original_value: object = original_arguments
        if isinstance(original_arguments, str):
            try:
                original_value = json.loads(original_arguments)
            except json.JSONDecodeError:
                original_value = original_arguments
        cleaned_arguments = sanitize_tool_call_payload(
            tool_name=tool_name,
            arguments=original_arguments,
            fallback_url=fallback_url,
        )
        if cleaned_arguments is None:
            continue
        cleaned_arguments = _remove_local_file_hint(cleaned_arguments)
        repaired = not isinstance(original_value, dict) or safe_json_dumps(cleaned_arguments) != safe_json_dumps(original_value)
        sanitized.append(
            {
                "id": str(tool_call.get("id", "")) or f"call_repaired_{index}",
                "type": "function",
                "index": index,
                "_repaired": repaired,
                "function": {
                    "name": tool_name,
                    "arguments": safe_json_dumps(cleaned_arguments),
                },
            }
        )
    return sanitized


def parse_tool_choice_policy(tool_choice: object, available_tool_names: set[str] | None = None) -> dict[str, object]:
    available = available_tool_names or set()
    if tool_choice is None:
        return {"mode": "auto", "tool_name": None}
    if isinstance(tool_choice, str):
        normalized = tool_choice.strip().lower()
        if normalized in {"auto", "none", "required"}:
            return {"mode": normalized, "tool_name": None}
        return {"mode": "auto", "tool_name": None}
    if not isinstance(tool_choice, dict):
        return {"mode": "auto", "tool_name": None}

    choice_type = str(tool_choice.get("type", "")).strip().lower()
    if choice_type == "function":
        function = tool_choice.get("function", {})
        if isinstance(function, dict):
            tool_name = str(function.get("name", "")).strip()
            if tool_name and (not available or tool_name in available):
                return {"mode": "specific", "tool_name": tool_name}
        return {"mode": "auto", "tool_name": None}

    if choice_type in {"auto", "none", "required"}:
        return {"mode": choice_type, "tool_name": None}
    return {"mode": "auto", "tool_name": None}


def convert_messages(
    messages: list[dict[str, object]],
    tools: list[dict[str, object]] | None,
    blocked_tool_names: set[str] | None = None,
    tool_choice: object | None = None,
    server_side_tool_names: set[str] | None = None,
) -> list[dict[str, object]]:
    tools = filter_tools(tools, blocked_tool_names or set())
    available_tool_names = {
        str(tool.get("function", {}).get("name", "")).strip()
        for tool in (tools or [])
        if isinstance(tool, dict) and isinstance(tool.get("function"), dict)
    }
    available_tool_names.discard("")
    server_side_tool_names = server_side_tool_names or SERVER_SIDE_TOOL_NAMES
    tool_choice_policy = parse_tool_choice_policy(tool_choice, available_tool_names)
    processed: list[dict[str, str]] = []
    latest_user_url: str | None = extract_recent_user_url(messages)
    valid_tool_call_ids: set[str] = set()
    repaired_tool_call_ids: set[str] = set()
    # Map tool_call_id -> tool_name so we can recover the tool name when the
    # client's tool-result message omits the `name` field (OpenAI spec makes
    # it optional). Without this, the result would be labelled "unknown_tool".
    tool_call_id_to_name: dict[str, str] = {}
    for message in messages:
        role = str(message.get("role", "user"))
        content = message.get("content")
        if role == "user":
            current_text = extract_text_content(content)
            current_url = extract_first_url(current_text)
            if current_url:
                latest_user_url = current_url
        if role == "assistant" and message.get("tool_calls"):
            tool_blocks: list[str] = []
            raw_tool_calls = message.get("tool_calls", []) # pyright: ignore[reportGeneralTypeIssues]
            sanitized_tool_calls = sanitize_tool_calls(
                raw_tool_calls if isinstance(raw_tool_calls, list) else [],
                fallback_url=latest_user_url,
            )
            for tool_call in sanitized_tool_calls:
                function = tool_call.get("function", {})
                tool_name = str(function.get("name", "unknown"))
                if available_tool_names and tool_name not in available_tool_names:
                    continue
                tool_blocks.append(
                    serialize_tool_call_block(
                        name=tool_name,
                        arguments=function.get("arguments", "{}"),
                    )
                )
                tool_call_id = str(tool_call.get("id", "")).strip()
                if tool_call_id and not tool_call_id.startswith("call_repaired_"):
                    valid_tool_call_ids.add(tool_call_id)
                    tool_call_id_to_name[tool_call_id] = tool_name
                    if bool(tool_call.get("_repaired")):
                        repaired_tool_call_ids.add(tool_call_id)
            assistant_text = extract_text_content(content).strip() if content else ""
            block = "\n".join(tool_blocks)
            if not assistant_text and not block:
                continue
            content = f"{assistant_text}\n{block}".strip() if assistant_text and block else (assistant_text or block)
        elif role == "tool":
            tool_call_id = str(message.get("tool_call_id", "")).strip()
            if tool_call_id and valid_tool_call_ids and tool_call_id not in valid_tool_call_ids:
                continue
            if tool_call_id and tool_call_id in repaired_tool_call_ids:
                continue
            role = "user"
            # Recover the tool name from the assistant's prior tool_calls when
            # the client's tool-result message omits the `name` field.
            tool_name = str(message.get("name", "")).strip()
            if not tool_name and tool_call_id:
                tool_name = tool_call_id_to_name.get(tool_call_id, "")
            if not tool_name:
                tool_name = "unknown_tool"
            tool_result_text = extract_text_content(content)
            content = serialize_tool_result_block(
                tool_call_id=tool_call_id or message.get("tool_call_id", "unknown"),
                tool_name=tool_name,
                content=tool_result_text,
            )
        elif role == "assistant" and not content:
            continue

        text = extract_text_content(content) if content else ""
        if text:
            processed.append({"role": role, "content": text})

    transcript_parts: list[str] = []

    if tools and tool_choice_policy.get("mode") != "none":
        transcript_parts.append(
            tools_to_prompt(
                tools,
                blocked_tool_names=blocked_tool_names,
                tool_choice_policy=tool_choice_policy,
                server_side_tool_names=server_side_tool_names,
            )
        )
        transcript_parts.append("# CONVERSATION")

    for item in processed:
        title = (
            item["role"]
            .replace("system", "System")
            .replace("assistant", "Assistant")
            .replace("user", "User")
            .replace("developer", "Developer")
        )
        transcript_parts.append(f"{title}: {item['content']}".strip())

    prompt = "\n".join(part for part in transcript_parts if part).strip()

    pattern = r'<system-reminder>.*'
    match = re.search(pattern, prompt, flags=re.DOTALL)
    result = match.group() if match else prompt
    LOCAL_FILE_HINT = "(本地文件，不在服务区上，应该用[function_calls]工具)"
    _LOCAL_FILE_PATH_RE = re.compile(
        rf'(?<!{re.escape(LOCAL_FILE_HINT)})'
        r'[A-Za-z]:[\\\/]'
        r'(?:[^\s<>()"\'\n\\\/]+[\\\/])*'
        r'[^\s<>()"\'\n\\\/]+'
        r'(?:#L\d+(?:-\d+)?)?'
    )
    prompt = _LOCAL_FILE_PATH_RE.sub(
        lambda m: m.group() + LOCAL_FILE_HINT, result
    )
    return [{
        "role": "user", "content": [{
            "type": "text", "text": prompt 
            + "\n\nAssistant: "
            }
            ]
    }]


def resolve_upstream_model(requested_model: str, config: AppConfig) -> tuple[str, str]:
    base_model, _ = split_model_features(requested_model)
    upstream_model = config.model_aliases.get(base_model, base_model)
    assistant_id = upstream_model if ASSISTANT_ID_PATTERN.fullmatch(upstream_model) else config.glm_assistant_id
    return upstream_model, assistant_id


def resolve_chat_mode(model: str, reasoning_effort: object, deep_research: object) -> str:
    lower_model = (model or "").lower()
    if deep_research or "deepresearch" in lower_model or "deep-research" in lower_model:
        return "deep_research"
    if reasoning_effort or model_requests_thinking(model) or "think" in lower_model or "zero" in lower_model:
        return "zero"
    return ""


def resolve_networking(model: str, web_search: object) -> bool:
    return bool(web_search) or model_requests_search(model)


@dataclass
class GLMEventAccumulator:
    model: str
    allowed_tool_names: set[str] | None = None
    fallback_tool_url: str | None = None
    debug_enabled: bool = False
    logger: Logger | None = None
    conversation_id: str = ""
    created: int = field(default_factory=lambda: int(time.time()))
    parts_by_logic_id: dict[str, dict[str, object]] = field(default_factory=dict)
    ordered_logic_ids: list[str] = field(default_factory=list)
    last_full_text: str = ""
    last_full_reasoning: str = ""
    _part_text_sent: dict[str, int] = field(default_factory=dict)
    _part_reasoning_sent: dict[str, int] = field(default_factory=dict)
    _known_logic_ids_for_text: list[str] = field(default_factory=list)
    _known_logic_ids_for_reasoning: list[str] = field(default_factory=list)
    tool_parser: StreamingToolParser = field(default_factory=StreamingToolParser)
    emitted_role: bool = False
    _render_cache_dirty: bool = True
    _cached_full_text: str = ""
    _cached_full_reasoning: str = ""
    _cached_part_texts: dict[str, str] = field(default_factory=dict)
    _cached_part_reasonings: dict[str, str] = field(default_factory=dict)
    _server_side_tool_calls: list[dict[str, object]] = field(default_factory=list)
    _server_side_tool_call_ids: set[str] = field(default_factory=set)
    _blocked_tool_call_ids: set[str] = field(default_factory=set)
    _blocked_tool_result_text: str = ""
    _deferred_visible_text: str = ""

    def __post_init__(self) -> None:
        self.tool_parser.allowed_tool_names = self.allowed_tool_names

    def consume_event(self, payload: dict[str, object]) -> tuple[list[str], str | None]:
        debug_dump(self.logger or logging.getLogger("glm2api.null"), self.debug_enabled, "GLM SSE 解析事件", payload)
        if not self.conversation_id and payload.get("conversation_id"):
            self.conversation_id = str(payload["conversation_id"])

        if self.tool_parser.is_tool_call_completed:
            return [], "tool_call_complete"

        for part in payload.get("parts", []) if isinstance(payload.get("parts"), list) else []: # pyright: ignore[reportGeneralTypeIssues]
            if isinstance(part, dict) and part.get("logic_id"):
                logic_id = str(part["logic_id"])
                if logic_id not in self.parts_by_logic_id:
                    insort(self.ordered_logic_ids, logic_id)
                self.parts_by_logic_id[logic_id] = part
                self._render_cache_dirty = True
            # Extract server-side native tool_calls from content items
            if isinstance(part, dict) and isinstance(part.get("content"), list):
                for content in part["content"]:
                    if isinstance(content, dict) and content.get("type") == "tool_calls":
                        tool_calls_data = content.get("tool_calls")
                        if isinstance(tool_calls_data, dict):
                            tool_name = str(tool_calls_data.get("name", "")).strip()
                            if tool_name == "open_url":
                                tool_name = "read"
                            tool_id = str(tool_calls_data.get("id", "")).strip()
                            arguments = tool_calls_data.get("arguments", "{}")
                            if tool_name in BLOCKED_NATIVE_TOOL_NAMES:
                                # Track blocked tool call IDs so we can convert
                                # their results to plain text instead of tool_calls.
                                if tool_id:
                                    self._blocked_tool_call_ids.add(tool_id)
                                continue
                            if self.allowed_tool_names is not None and tool_name not in self.allowed_tool_names:
                                continue
                            if tool_name and tool_id and tool_id not in self._server_side_tool_call_ids:
                                self._server_side_tool_call_ids.add(tool_id)
                                self._server_side_tool_calls.append(
                                    {
                                        "id": tool_id,
                                        "type": "function",
                                        "index": len(self._server_side_tool_calls),
                                        "function": {
                                            "name": tool_name,
                                            "arguments": str(arguments) if isinstance(arguments, str) else safe_json_dumps(arguments),
                                        },
                                    }
                                )
                    # Convert tool_result for blocked native tools into plain text
                    # so the model's answer still benefits from the fetched content.
                    if isinstance(content, dict) and content.get("type") == "tool_result":
                        tool_result_data = content.get("tool_calls")
                        if isinstance(tool_result_data, dict):
                            result_tool_id = str(tool_result_data.get("id", "")).strip()
                            if result_tool_id in self._blocked_tool_call_ids:
                                # Extract useful text from the tool result metadata
                                result_text_parts: list[str] = []
                                meta = part.get("meta_data", {})
                                if isinstance(meta, dict):
                                    tool_extra = meta.get("tool_result_extra", {})
                                    if isinstance(tool_extra, dict):
                                        search_results = tool_extra.get("search_results")
                                        if isinstance(search_results, list):
                                            for sr in search_results:
                                                if isinstance(sr, dict):
                                                    title = sr.get("title", "")
                                                    text = sr.get("text", "")
                                                    url = sr.get("url", "")
                                                    if text:
                                                        result_text_parts.append(text)
                                                    elif title:
                                                        result_text_parts.append(f"{title} ({url})")
                                if result_text_parts:
                                    self._blocked_tool_result_text = "\n\n".join(result_text_parts)

        text_delta, reasoning_delta = self._compute_deltas()
        self.last_full_text = self._cached_full_text
        self.last_full_reasoning = self._cached_full_reasoning

        chunks: list[str] = []
        if reasoning_delta:
            chunks.append(
                self._chunk_json(
                    {
                        "choices": [
                            {
                                "index": 0,
                                "delta": {"reasoning_content": reasoning_delta},
                                "finish_reason": None,
                            }
                        ]
                    }
                )
            )

        visible_text_delta = self.tool_parser.consume(text_delta)
        if visible_text_delta:
            if self.allowed_tool_names is not None:
                self._deferred_visible_text += visible_text_delta
            else:
                delta_payload: dict[str, object] = {"content": visible_text_delta}
                if not self.emitted_role:
                    delta_payload = {"role": "assistant", "content": visible_text_delta}
                    self.emitted_role = True
                chunks.append(
                    self._chunk_json(
                        {
                            "choices": [
                                {
                                    "index": 0,
                                    "delta": delta_payload,
                                    "finish_reason": None,
                                }
                            ]
                        }
                    )
                )
        debug_dump(self.logger or logging.getLogger("glm2api.null"), self.debug_enabled, "GLM SSE 生成增量块", chunks)
        return chunks, str(payload.get("status")) if payload.get("status") is not None else None

    def finalize(self, status: str | None, last_error: dict[str, object] | None = None) -> list[str]:
        tail_text, json_tool_calls = self.tool_parser.flush()
        json_tool_calls = sanitize_tool_calls(json_tool_calls, fallback_url=self.fallback_tool_url)
        if not json_tool_calls:
            json_tool_calls = self._extract_reasoning_tool_calls()

        # Merge server-side and JSON tool calls, re-indexing
        all_tool_calls: list[dict[str, object]] = list(self._server_side_tool_calls)
        for tc in json_tool_calls:
            tc_copy = dict(tc)
            tc_copy["index"] = len(all_tool_calls)
            all_tool_calls.append(tc_copy)

        if self.logger:
            self.logger.info(
                "响应收尾 status=%s text_len=%s reasoning_len=%s tool_calls=%s server_tools=%s",
                status,
                len(self._cached_full_text),
                len(self._cached_full_reasoning),
                len(json_tool_calls),
                len(self._server_side_tool_calls),
            )

        chunks: list[str] = []
        final_text = self._deferred_visible_text + tail_text
        self._deferred_visible_text = ""

        # If we still have no tool calls but the deferred visible text looks
        # like a bare-JSON tool_calls payload (the model omitted the
        # ```tool_code fence), try to extract tool calls from it instead of
        # leaking the raw JSON to the client.
        if not all_tool_calls and final_text:
            recovered_clean, recovered_calls = parse_tool_calls_from_text(
                final_text,
                allowed_tool_names=self.allowed_tool_names,
            )
            if recovered_calls:
                sanitized_recovered = sanitize_tool_calls(recovered_calls, fallback_url=self.fallback_tool_url)
                if sanitized_recovered:
                    for tc in sanitized_recovered:
                        tc_copy = dict(tc)
                        tc_copy["index"] = len(all_tool_calls)
                        all_tool_calls.append(tc_copy)
                    final_text = recovered_clean
                    if self.logger:
                        self.logger.info(
                            "finalize: recovered %d tool call(s) from deferred visible text",
                            len(sanitized_recovered),
                        )

        # Inject blocked native tool results as reference text so the model's
        # subsequent answer (which used the browser/fetch result) still makes
        # sense to the client even though we suppressed the tool_call itself.
        if self._blocked_tool_result_text and not all_tool_calls:
            if final_text:
                final_text = f"[Reference content fetched by browser]:\n{self._blocked_tool_result_text}\n\n{final_text}"
            else:
                final_text = f"[Reference content fetched by browser]:\n{self._blocked_tool_result_text}"
        if not final_text and not all_tool_calls and self.allowed_tool_names is not None:
            _, attempted_tool_calls = parse_tool_calls_from_text(
                self._cached_full_text.strip(),
                allowed_tool_names=None,
            )
            unavailable_names = sorted(
                {
                    str(tool_call.get("function", {}).get("name", "")).strip()
                    for tool_call in attempted_tool_calls
                    if isinstance(tool_call.get("function"), dict)
                    and str(tool_call.get("function", {}).get("name", "")).strip()
                    not in self.allowed_tool_names
                }
            )
            if unavailable_names:
                allowed_names = ", ".join(sorted(self.allowed_tool_names)) or "(none)"
                final_text = (
                    "模型尝试调用未声明工具 "
                    + ", ".join(f"`{name}`" for name in unavailable_names)
                    + f"，已阻止。本轮只允许这些工具：{allowed_names}。"
                )
        if final_text and not all_tool_calls:
            delta_payload: dict[str, object] = {"content": final_text}
            if not self.emitted_role:
                delta_payload = {"role": "assistant", "content": final_text}
                self.emitted_role = True
            chunks.append(
                self._chunk_json(
                    {
                        "choices": [
                            {
                                "index": 0,
                                "delta": delta_payload,
                                "finish_reason": None,
                            }
                        ]
                    }
                )
            )

        if status == "intervene" and last_error and last_error.get("intervene_text"):
            chunks.append(
                self._chunk_json(
                    {
                        "choices": [
                            {
                                "index": 0,
                                "delta": {"content": "\n\n" + str(last_error["intervene_text"])},
                                "finish_reason": None,
                            }
                        ]
                    }
                )
            )

        if all_tool_calls:
            if not self.emitted_role:
                chunks.append(
                    self._chunk_json(
                        {
                            "choices": [
                                {
                                    "index": 0,
                                    "delta": {"role": "assistant"},
                                    "finish_reason": None,
                                }
                            ]
                        }
                    )
                )
                self.emitted_role = True
            for tool_call in all_tool_calls:
                chunks.append(
                    self._chunk_json(
                        {
                            "choices": [
                                {
                                    "index": 0,
                                    "delta": {
                                        "tool_calls": [
                                            {
                                                "index": tool_call["index"],
                                                "id": tool_call["id"],
                                                "type": "function",
                                                "function": tool_call["function"],
                                            }
                                        ]
                                    },
                                    "finish_reason": None,
                                }
                            ]
                        }
                    )
                )

        finish_reason = "tool_calls" if all_tool_calls else "stop"
        chunks.append(
            self._chunk_json(
                {
                    "choices": [
                        {
                            "index": 0,
                            "delta": {},
                            "finish_reason": finish_reason,
                        }
                    ],
                    "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
                }
            )
        )
        chunks.append("data: [DONE]\n\n")
        debug_dump(self.logger or logging.getLogger("glm2api.null"), self.debug_enabled, "GLM SSE finalize 输出", chunks)
        return chunks

    def build_response(self) -> dict[str, object]:
        full_text, full_reasoning = self._render_full_output()
        if not full_text and self.last_full_text:
            full_text = self.last_full_text
        if not full_reasoning and self.last_full_reasoning:
            full_reasoning = self.last_full_reasoning
        clean_content, json_tool_calls = parse_tool_calls_from_text(
            full_text.strip(),
            allowed_tool_names=self.allowed_tool_names,
        )
        json_tool_calls = sanitize_tool_calls(json_tool_calls, fallback_url=self.fallback_tool_url)
        if not json_tool_calls:
            json_tool_calls = self._extract_reasoning_tool_calls(full_reasoning)

        # Merge server-side and JSON tool calls, re-indexing
        all_tool_calls: list[dict[str, object]] = list(self._server_side_tool_calls)
        for tc in json_tool_calls:
            tc_copy = dict(tc)
            tc_copy["index"] = len(all_tool_calls)
            all_tool_calls.append(tc_copy)

        final_content = clean_content.strip()
        message: dict[str, object] = {
            "role": "assistant",
            "content": None if all_tool_calls or not final_content else final_content,
            "reasoning_content": full_reasoning or None,
        }
        if all_tool_calls:
            message["tool_calls"] = [
                {"id": item["id"], "type": "function", "function": item["function"]}
                for item in all_tool_calls
            ]
        response = {
            "id": self.conversation_id,
            "object": "chat.completion",
            "created": self.created,
            "model": self.model,
            "choices": [
                {
                    "index": 0,
                    "message": message,
                    "finish_reason": "tool_calls" if all_tool_calls else "stop",
                }
            ],
            "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
        }
        if self.logger:
            self.logger.info(
                "非流式响应构建完成 model=%s text_len=%s reasoning_len=%s tool_calls=%s",
                self.model,
                len(final_content),
                len(full_reasoning),
                len(all_tool_calls),
            )
        debug_dump(self.logger or logging.getLogger("glm2api.null"), self.debug_enabled, "GLM 非流式最终响应", response)
        return response

    def _extract_reasoning_tool_calls(self, reasoning_text: str | None = None) -> list[dict[str, object]]:
        source = (reasoning_text if reasoning_text is not None else self.last_full_reasoning) or self._cached_full_reasoning
        if not source:
            return []
        _, tool_calls = parse_tool_calls_from_text(
            source.strip(),
            allowed_tool_names=self.allowed_tool_names,
        )
        return sanitize_tool_calls(tool_calls, fallback_url=self.fallback_tool_url)

    def _compute_deltas(self) -> tuple[str, str]:
        self._render_full_output()
        text_delta_parts: list[str] = []
        reasoning_delta_parts: list[str] = []

        for logic_id in self.ordered_logic_ids:
            rendered_text = self._cached_part_texts.get(logic_id, "")
            rendered_reasoning = self._cached_part_reasonings.get(logic_id, "")

            if rendered_text:
                prev_len = self._part_text_sent.get(logic_id, 0)
                is_new = logic_id not in self._known_logic_ids_for_text
                if is_new:
                    self._known_logic_ids_for_text.append(logic_id)
                    if text_delta_parts or self._part_text_sent:
                        text_delta_parts.append("\n\n")
                    text_delta_parts.append(rendered_text)
                elif len(rendered_text) > prev_len:
                    text_delta_parts.append(rendered_text[prev_len:])
                self._part_text_sent[logic_id] = len(rendered_text)

            if rendered_reasoning:
                prev_len = self._part_reasoning_sent.get(logic_id, 0)
                is_new = logic_id not in self._known_logic_ids_for_reasoning
                if is_new:
                    self._known_logic_ids_for_reasoning.append(logic_id)
                    if reasoning_delta_parts or self._part_reasoning_sent:
                        reasoning_delta_parts.append("\n\n")
                    reasoning_delta_parts.append(rendered_reasoning)
                elif len(rendered_reasoning) > prev_len:
                    reasoning_delta_parts.append(rendered_reasoning[prev_len:])
                self._part_reasoning_sent[logic_id] = len(rendered_reasoning)

        return "".join(text_delta_parts), "".join(reasoning_delta_parts)

    def _render_full_output(self) -> tuple[str, str]:
        if not self._render_cache_dirty:
            return self._cached_full_text, self._cached_full_reasoning

        text_parts: list[str] = []
        reasoning_parts: list[str] = []
        self._cached_part_texts.clear()
        self._cached_part_reasonings.clear()
        for logic_id in self.ordered_logic_ids:
            part = self.parts_by_logic_id.get(logic_id)
            if not isinstance(part, dict):
                continue
            content_items = part.get("content", [])
            if not isinstance(content_items, list):
                continue

            part_text: list[str] = []
            part_reasoning: list[str] = []
            for content in content_items:
                if not isinstance(content, dict):
                    continue
                item_type = content.get("type")
                if item_type == "text":
                    part_text.append(str(content.get("text", "")))
                elif item_type == "think":
                    part_reasoning.append(str(content.get("think", "")))
                elif item_type == "code":
                    part_text.append(f"```python\n{content.get('code', '')}\n```")
                elif item_type == "execution_output":
                    part_text.append(str(content.get("content", "")))
                elif item_type == "image":
                    images = content.get("image", [])
                    if isinstance(images, list):
                        for image in images:
                            if isinstance(image, dict) and image.get("image_url"):
                                part_text.append(f"![image]({image['image_url']})")

            rendered_text = "\n".join(filter(None, part_text)).strip()
            rendered_reasoning = "\n".join(filter(None, part_reasoning)).strip()
            if rendered_text:
                text_parts.append(rendered_text)
                self._cached_part_texts[logic_id] = rendered_text
            if rendered_reasoning:
                reasoning_parts.append(rendered_reasoning)
                self._cached_part_reasonings[logic_id] = rendered_reasoning

        self._cached_full_text = "\n\n".join(text_parts)
        self._cached_full_reasoning = "\n\n".join(reasoning_parts)
        self._render_cache_dirty = False
        return self._cached_full_text, self._cached_full_reasoning

    def _chunk_json(self, patch: dict[str, object]) -> str:
        payload = {
            "id": self.conversation_id,
            "object": "chat.completion.chunk",
            "created": self.created,
            "model": self.model,
        }
        payload.update(patch)
        return "data: " + safe_json_dumps(payload) + "\n\n"
