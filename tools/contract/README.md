# 契约对比

同一组场景（`scenarios.json`）分别打到 Python 版和 Go 版，比较状态码和 JSON 结构。

准备（重启后 /tmp 会清空，跑一次 `tools/contract/setup.sh` 即可全部重建）：
- 假上游：`go run ./tools/fakeupstream -addr 127.0.0.1:18100 -delay 2s`
- Python 版：一个 `main` 的 worktree 放在 `$PY_DIR`（默认 `/tmp/c2a-pyfake`），里面有 `config.json` 和 `.venv`
- PostgreSQL：docker 容器 `c2a-pg`（127.0.0.1:55432，用户、密码、库都是 c2a）

运行：
```
tools/contract/run-python.sh /tmp/c2a-contract-py   # 全新 Python 实例，录制
tools/contract/run-go.sh /tmp/c2a-contract-go       # 全新 Go 实例和数据库，录制
python3 tools/contract/contract.py diff /tmp/c2a-contract-py /tmp/c2a-contract-go
```

录制结果可能包含配置里的密钥，只放在 /tmp，不要提交。

新增的模块要在 `scenarios.json` 里补上对应场景。每次补完都先把 Python 版录两遍做 diff，结果必须完全一致，确认场景是确定性的。
