"""Compare two walkthrough runs: screenshots pixel by pixel, and API calls / failures per page."""
import json
import re
import sys
from pathlib import Path

from PIL import Image, ImageChops

py_dir, go_dir = Path(sys.argv[1]), Path(sys.argv[2])
for shot in sorted(py_dir.glob("*.png")):
    a = Image.open(shot).convert("RGB")
    b = Image.open(go_dir / shot.name).convert("RGB")
    note = "" if a.size == b.size else f"  尺寸 py{a.size} go{b.size}"
    if a.size != b.size:
        b = b.resize(a.size)
    diff = ImageChops.difference(a, b).convert("L").point(lambda v: 255 if v > 24 else 0)
    pixels = diff.get_flattened_data() if hasattr(diff, "get_flattened_data") else diff.getdata()
    ratio = sum(1 for v in pixels if v) / (a.size[0] * a.size[1])
    print(f"{shot.name:22} 差异 {ratio * 100:5.2f}%  区域 {diff.getbbox()}{note}")
if len(sys.argv) > 3:  # flows: step results and errors
    for side, folder in (("py", py_dir), ("go", go_dir)):
        flows = json.loads((folder / "flows.json").read_text())
        bad = [s for s in flows["steps"] if not s.endswith(" ok")]
        print(f"{side}: 步骤 {len(flows['steps'])} 个，失败 {bad}；控制台错误 {flows['console'][:3]}；页面错误 {flows['pageErrors'][:2]}；接口失败 {sorted(set(flows['failed']))[:5]}")
    sys.exit(0)
py = json.loads((py_dir / "report.json").read_text())
go = json.loads((go_dir / "report.json").read_text())
norm = lambda call: re.sub(r"[0-9a-f]{32}", "<id>", call)
for page, p in py.items():
    g = go.get(page, {"calls": [], "failed": [], "pageErrors": []})
    only_py = sorted(set(map(norm, p["calls"])) - set(map(norm, g["calls"])))
    only_go = sorted(set(map(norm, g["calls"])) - set(map(norm, p["calls"])))
    # HEAD prefetches and the mock data's localhost image URLs fail the same way on both sides
    failed = lambda items: [x for x in items if "HEAD " not in x and "//localhost/" not in x]
    if only_py or only_go or failed(g["failed"]) != failed(p["failed"]) or g["pageErrors"]:
        print(f"{page}: 只在 py {only_py}  只在 go {only_go}  go 失败 {failed(g['failed'])[:3]}  py 失败 {failed(p['failed'])[:3]}  go 页面错误 {g['pageErrors'][:1]}")
