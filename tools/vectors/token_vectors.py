#!/usr/bin/env python3
"""Golden vectors for internal/imagegen token counting, from utils/image_tokens.py on main.

    PYTHONPATH=<main> <main>/.venv/bin/python tools/vectors/token_vectors.py > internal/imagegen/testdata/token_vectors.json
"""
import json, os, sys
os.environ.setdefault("CHATGPT2API_AUTH_KEY", "vector-key")
import contextlib, io
with contextlib.redirect_stdout(io.StringIO()):  # importing the services prints storage banners
    from services.protocol.conversation import count_text_tokens
    from utils import image_tokens as t

texts = ["", "a cat", "画一只橘猫，16:9 横版构图，简洁插画", "Hello, world! " * 20, "输出图片尺寸为 1024x1536。输出图片质量为 auto。"]
models = ["gpt-image-2", "auto", "gpt-4o", "gpt-image-2.6-sunburst", "codex-gpt-image-2"]
sizes = [(1024, 1024), (1536, 1024), (1024, 1536), (4096, 4096), (64, 64), (3000, 200), (8000, 6000), (17, 9000)]
out = {
    "text": [{"text": x, "model": m, "tokens": count_text_tokens(x, m)} for x in texts for m in models],
    "input": [{"w": w, "h": h, "detail": d, "tokens": t.count_image_input_tokens(w, h, "gpt-image-2", d)} for w, h in sizes for d in ("auto", "low", "high", "original")],
    "generated": [{"w": w, "h": h, "quality": q, "tokens": t.count_generated_image_tokens(w, h, q)} for w, h in sizes for q in ("auto", "low", "high", "hd", "medium")],
    "parse": [{"size": s, "wh": list(t.parse_image_size(s))} for s in ["1024x1536", "", None, "abc", "9x9", "1792 * 1024", "1x99999", "00x00"]],
}
json.dump(out, sys.stdout, ensure_ascii=False, indent=1)
