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


def make_test_image(path: str) -> str:
    """生成蓝色方块测试图，返回文件路径。"""
    try:
        from PIL import Image

        Image.new("RGB", (150, 150), (30, 60, 220)).save(path)
    except ImportError:
        # 固定的 1x1 蓝色 PNG 兜底
        data = base64.b64decode(
            "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJ"
            "AAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
        )
        with open(path, "wb") as f:
            f.write(data)
    return path


if len(sys.argv) > 1:
    image_path = sys.argv[1]
else:
    image_path = make_test_image("/tmp/upload_test/blue_square.png")

# 图片转 base64 data URL
with open(image_path, "rb") as f:
    img_b64 = base64.b64encode(f.read()).decode("ascii")
data_url = f"data:image/png;base64,{img_b64}"
print(f"图片: {image_path} ({os.path.getsize(image_path)} bytes, base64 {len(img_b64)} 字符)")

payload = {
    "model": "glm-5.3",
    "stream": False,
    "messages": [
        {
            "role": "user",
            "content": [
                {"type": "text", "text": "这张图片是什么颜色？只回答颜色，不要猜。"},
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
