"""生成 README 横幅（docs/banner.svg）。

用法：python docs/banner_gen.py

设计取向：字标为主，不玩概念。全图只有一处彩色——字标下那条渐变线，
用的是管理面板同一套品牌色（#4285F4 → #9B72CB → #D96570），
在面板里它也只出现在一个地方（封禁红线轨道）。

两个约束来自 GitHub 的渲染方式（README 里的 SVG 是当图片渲染的）：
- 不用滤镜/阴影，糊开的发光在小尺寸下只会变脏
- 字体走系统等宽栈，不引外部字体（<img> 沙箱会拦外链）
"""
import io
import os

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
OUT = os.path.join(REPO, "docs", "banner.svg")

W, H = 1280, 260
MONO = "ui-monospace,SFMono-Regular,SF Mono,Menlo,Consolas,Liberation Mono,monospace"
CHIPS = ["no account", "no API key", "single Go binary", "admin panel"]

cx = W / 2
p = [
    f'<svg xmlns="http://www.w3.org/2000/svg" width="{W}" height="{H}" '
    f'viewBox="0 0 {W} {H}" role="img" '
    f'aria-label="gemini-web2api-go — Gemini web to OpenAI-compatible, native Gemini API and MCP">',
    '<defs><linearGradient id="sweep" x1="0" y1="0" x2="1" y2="0">'
    '<stop offset="0" stop-color="#4285F4"/>'
    '<stop offset="0.55" stop-color="#9B72CB"/>'
    '<stop offset="1" stop-color="#D96570"/></linearGradient></defs>',
    f'<rect width="{W}" height="{H}" fill="#0D1117"/>',
    f'<text x="{cx}" y="112" font-family="{MONO}" font-size="46" font-weight="600" '
    f'letter-spacing="-1.2" fill="#E9EEF6" text-anchor="middle">gemini-web2api-go</text>',
    f'<rect x="{cx - 190}" y="136" width="380" height="2.5" rx="1.25" fill="url(#sweep)"/>',
    f'<text x="{cx}" y="174" font-family="{MONO}" font-size="15" fill="#8695AE" '
    f'text-anchor="middle">gemini.google.com &#8594; OpenAI &#183; Gemini API &#183; MCP</text>',
]

# 一行胶囊，手工居中排版（SVG 没有 flex）
FW, PAD, GAP = 7.1, 15, 11        # FW = 12px 等宽字的近似字宽
widths = [len(c) * FW + PAD * 2 for c in CHIPS]
x = cx - (sum(widths) + GAP * (len(CHIPS) - 1)) / 2
for c, w in zip(CHIPS, widths):
    p.append(f'<rect x="{x:.1f}" y="200" width="{w:.1f}" height="27" rx="13.5" '
             f'fill="#161C26" stroke="#242C3A"/>')
    p.append(f'<text x="{x + w / 2:.1f}" y="218" font-family="{MONO}" font-size="12" '
             f'fill="#7D8CA6" text-anchor="middle">{c}</text>')
    x += w + GAP

p.append("</svg>")
io.open(OUT, "w", encoding="utf-8").write("\n".join(p))
print(f"已写 {OUT}  ({os.path.getsize(OUT)} 字节)")
