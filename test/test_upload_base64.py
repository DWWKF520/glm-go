#!/usr/bin/env python3
"""测试附件上传（base64 data URL 路径）。

用法：
    python3 test_upload_base64.py [本地图片路径]

不带参数时会自动生成一张蓝色方块测试图并转为 base64。
"""
import base64
import json
import os
import sys
import urllib.request

API = "http://127.0.0.1:8000/v1/chat/completions"

image = "/home/wkf/下载/R-C.jpeg"
base64_img = base64.b64encode(open(image, "rb").read()).decode("ascii")
data_url = f"data:image/jpeg;base64,{base64_img}"
print(f"图片: {image} ({os.path.getsize(image)} bytes, base64 {len(base64_img)} 字符)")

payload = {
    "model": "glm-5.3",
    "stream": False,
    "messages": [
        {
            "role": "user",
            "content": [
                {"type": "text", "text": "这张图片是什么"},
                {"type": "image_url", "image_url": {"url": data_url}},
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

print("=== 回答 ===")
print(text.strip() or "(空)")
if reasoning:
    print("=== 推理（尾部 200 字） ===")
    print(reasoning[-200:])
