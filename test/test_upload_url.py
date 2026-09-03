#!/usr/bin/env python3
"""测试附件上传（URL 路径）。

用法：
    python3 test_upload_url.py [图片URL]

默认使用本机文件服务 http://127.0.0.1:9999/red_circle.png，
可先用如下命令在本机起一个文件服务：
    cd /tmp/upload_test && python3 -m http.server 9999
"""
import json
import sys
import urllib.request

API = "http://127.0.0.1:8000/v1/chat/completions"

image_url = sys.argv[1] if len(sys.argv) > 1 else "https://ts1.tc.mm.bing.net/th/id/R-C.b4cea389f5b5c5a0833fb2a26bd42263?rik=nXUJ0VDLjThccg&riu=http%3a%2f%2fwww.mrcdzg.com%2fuploads%2fimages%2f20200604%2ff9b6a49b4072d883a6e82ffe1bec7321.jpg&ehk=IFh9Xv9uoFI1X9%2fZ3qSROPVT%2bmnMCHItePUwlXe9o8E%3d&risl=&pid=ImgRaw&r=0"

payload = {
    "model": "glm-5.3",
    "stream": False,
    "messages": [
        {
            "role": "user",
            "content": [
                {"type": "text", "text": "这是谁？"},
                {"type": "image_url", "image_url": {"url": image_url}},
            ],
        }
    ],
}

req = urllib.request.Request(
    API,
    data=json.dumps(payload).encode("utf-8"),
    headers={"Content-Type": "application/json"},
    method="POST",
)

text, reasoning = "", ""
with urllib.request.urlopen(req, timeout=300) as resp:
    for raw in resp:
        line = raw.decode("utf-8", errors="ignore").strip()
        if not line.startswith("data: "):
            continue
        data = line[6:]
        if data == "[DONE]":
            break
        try:
            event = json.loads(data)
        except json.JSONDecodeError:
            continue
        for choice in event.get("choices", []):
            delta = choice.get("delta", {})
            text += delta.get("content") or ""
            reasoning += delta.get("reasoning_content") or ""

print("=== 图片 URL ===")
print(image_url)
print("=== 回答 ===")
print(text.strip() or "(空)")
if reasoning:
    print("=== 推理（尾部 200 字） ===")
    print(reasoning[-200:])
