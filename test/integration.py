"""集成测试开关：要连真实上游或本地已启动服务的测试，默认跳过。

在 localhost:8000 启动服务、准备好 assets/image.png 后，用
CHATGPT2API_INTEGRATION=1 .venv/bin/python -m unittest discover -s test -t . 一起跑。
"""

import os
import unittest

ENABLED = os.getenv("CHATGPT2API_INTEGRATION") == "1"

live = unittest.skipUnless(ENABLED, "集成测试：设置 CHATGPT2API_INTEGRATION=1 才运行")
