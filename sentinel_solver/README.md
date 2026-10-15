# sentinel 求解服务

chatgpt.com 从 2026-10-14 起校验生图请求的 Turnstile。证明质量不够时，对话接口返回伪装成 404 的 `{"detail":"Model not found"}`。

本服务常驻一个无头 Chromium，打开 chatgpt.com，只根据 `dx` 和 `p` 计算 Turnstile。不接收账号 token、Cookie 或用户提示词。设置 `SENTINEL_SOLVER_KEY` 后，请求必须带 `Authorization: Bearer <key>`。不要把 8080 暴露到公网。

主服务用 `CHATGPT2API_SENTINEL_SOLVER_URL` 和 `CHATGPT2API_SENTINEL_SOLVER_KEY` 连接。`SENTINEL_SOLVER_TIMEZONE` 要和出口 IP 的时区一致。这台机器的出口在加州，用 `America/Los_Angeles`。
