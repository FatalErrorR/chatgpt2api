"""生图失败处理：先判断上游这一轮有没有开画，再决定等图、换号还是报错。

用假的上游 SSE 事件驱动真实的解析和判断流程（stream_image_outputs / _generate_single_image），
网络调用（会话文档、任务列表、轮询、下载）由 FakeBackend 替代。
"""
from __future__ import annotations

import contextlib
import json
import tempfile
import threading
import unittest
from pathlib import Path
from unittest import mock

from services.account_service import AccountService
from services.openai_backend_api import ImagePollTimeoutError, OpenAIBackendAPI
from services.protocol import conversation
from services.storage.json_storage import JSONStorageBackend

CONV_ID = "conv-1"


def sse(*events: dict) -> list[str]:
    return [json.dumps(event, ensure_ascii=False) for event in events] + ["[DONE]"]


START = {"type": "resume_conversation_token", "conversation_id": CONV_ID}
PROGRESS = {"type": "title_generation", "title": "创建图像", "conversation_id": CONV_ID}
TOO_LARGE = {"message": None, "conversation_id": CONV_ID, "error": "你提交的消息过长，请编辑后重新发送。",
             "error_code": "input_too_large", "error_reason": "last_user_message"}
IMAGE_TOOL_MESSAGE = {"o": "add", "conversation_id": CONV_ID, "v": {"message": {
    "id": "tool-1", "author": {"role": "tool", "name": "t2uay3k.sj1i4kz"}, "metadata": {},
    "content": {"content_type": "multimodal_text", "parts": [
        {"content_type": "image_asset_pointer", "asset_pointer": "sediment://file_0000000027908"}]}}}}


def metadata(tool_invoked: bool, use_case: str) -> dict:
    return {"type": "server_ste_metadata", "metadata": {"tool_invoked": tool_invoked, "turn_use_case": use_case}}


def assistant(text: str) -> dict:
    return {"conversation_id": CONV_ID, "v": {"message": {
        "id": "answer-1", "author": {"role": "assistant"}, "recipient": "all", "metadata": {},
        "status": "finished_successfully", "content": {"content_type": "text", "parts": [text]}}}}


def document(*messages: dict) -> dict:
    user = {"author": {"role": "user"}, "recipient": "all", "content": {"content_type": "text", "parts": ["(prompt)"]}}
    return {"mapping": {str(i): {"message": message} for i, message in enumerate([user, *messages])}}


def doc_text(text: str) -> dict:
    return {"author": {"role": "assistant"}, "recipient": "all", "content": {"content_type": "text", "parts": [text]}}


DOC_TOOL_CALL = {"author": {"role": "assistant"}, "recipient": "t2uay3k.sj1i4kz",
                 "content": {"content_type": "code", "text": '{"size":"1024x1024","n":1,"prompt":"a cat"}'}}

SERVICE_DOWN_TASK = {"conversation_id": CONV_ID, "image_gen_message": {
    "author": {"role": "assistant"}, "metadata": {"is_error": True},
    "content": {"content_type": "text", "parts": ["服务暂时不可用，请稍后再试。"]}}}
IMAGE_POLICY = "非常抱歉，生成的图片可能违反了关于裸露、色情或情色内容的防护限制。如果你认为此判断有误，请重试或修改提示语。"
POLICY_TASK = {"conversation_id": CONV_ID, "image_gen_message": {
    "author": {"role": "assistant"}, "metadata": {"is_error": True},
    "content": {"content_type": "text", "parts": ["非常抱歉，生成的图片可能违反了我们的内容政策。"]}}}
COPY_ONLY = "你的提示词已经非常完整，以下是整理后的精简版本：画面中央一只猫，暖色调，柔和光线。"
CANNOT_GENERATE = "目前无法在此对话中直接生成图片。\n\n可以使用以下精简提示词：纯白背景，中央一个小黑点。"
QUOTA_EXHAUSTED = "你已达到免费套餐的图片生成上限，限制将在 23 小时 10 分钟后重置。"
POLICY = "非常抱歉，该提示可能违反了我们的内容政策。如果你认为此判断有误，请重试或修改提示语。"


class FakeBackend(OpenAIBackendAPI):
    """只替换网络调用；会话文档解析、任务错误判断用真实实现。"""

    def __new__(cls, *args, **kwargs):
        return object.__new__(cls)

    def __init__(self, payloads, doc=None, tasks=None, doc_error=None, tasks_error=None):
        self.payloads = payloads
        self.doc = doc if doc is not None else document()
        self.tasks = tasks or []
        self.doc_error = doc_error
        self.tasks_error = tasks_error
        self.poll_timeouts: list[float] = []
        self.doc_reads = 0

    def stream_conversation(self, **kwargs):
        return iter(self.payloads)

    def _get_conversation(self, conversation_id):
        self.doc_reads += 1
        if self.doc_error:
            raise self.doc_error
        return self.doc

    def _query_backend_tasks(self, conversation_id="", task_id="", timeout_secs=30.0):
        if self.tasks_error:
            raise self.tasks_error
        return self.tasks

    def _poll_image_results(self, conversation_id, timeout_secs=120.0, initial_file_ids=None, initial_sediment_ids=None):
        # 真实轮询会在这里等满 timeout_secs；测试里直接按超时处理，并记下要等多久
        self.poll_timeouts.append(timeout_secs)
        raise ImagePollTimeoutError(f"ChatGPT 生图超时（已等待 {timeout_secs} 秒）。", conversation_id)

    def resolve_conversation_image_urls(self, conversation_id, file_ids, sediment_ids, poll=True, poll_timeout_secs=None):
        if file_ids or sediment_ids:
            return ["https://files.example/image.png"]
        if poll and conversation_id:
            self._poll_image_results(conversation_id, poll_timeout_secs, file_ids, sediment_ids)
        return []

    def download_image_bytes(self, urls):
        return [b"png-bytes" for _ in urls]

    def find_conversation_by_prompt(self, *args, **kwargs):
        return ""

    def close(self):
        pass


class Sleeps:
    def __init__(self):
        self.total = 0.0

    def __call__(self, secs):
        self.total += float(secs)


def fake_result(items, prompt, response_format, base_url, created, size=None):
    return {"created": created, "data": [{"url": "https://local/image.png"} for _ in items]}


class StreamAssessmentTests(unittest.TestCase):
    """stream_image_outputs 读完一轮上游后的判断。"""

    def run_stream(self, backend, *, images=None, message_as_error=True, failover_timeout_secs=60, task_poll_timeout_secs=540):
        request = conversation.ConversationRequest(
            model="gpt-image-2", prompt="a cat", images=images, message_as_error=message_as_error,
            failover_timeout_secs=failover_timeout_secs, task_poll_timeout_secs=task_poll_timeout_secs)
        sleeps = Sleeps()
        with mock.patch.object(conversation.time, "sleep", sleeps), \
                mock.patch.object(conversation, "format_image_result", fake_result):
            outputs, error = [], None
            try:
                for output in conversation.stream_image_outputs(backend, request):
                    outputs.append(output)
            except Exception as exc:  # noqa: BLE001 - 测试要看具体是哪种异常
                error = exc
        return outputs, error, sleeps

    def test_input_too_large_with_an_image_in_the_same_turn_still_succeeds(self):
        # 生产重现过：同一轮既有 input_too_large 又出了图
        backend = FakeBackend(sse(START, IMAGE_TOOL_MESSAGE, TOO_LARGE, metadata(True, "image gen")))
        outputs, error, _ = self.run_stream(backend)
        self.assertIsNone(error)
        self.assertEqual(outputs[-1].kind, "result")
        self.assertEqual(backend.poll_timeouts, [])
        self.assertEqual(backend.doc_reads, 0)  # SSE 里已经有图，不用再查

    def test_input_too_large_without_image_or_task_is_a_400(self):
        backend = FakeBackend(sse(START, TOO_LARGE, metadata(False, "unknown")))
        _, error, _ = self.run_stream(backend, images=["ref"])
        self.assertIsInstance(error, conversation.ImageInputTooLargeError)
        self.assertEqual((error.status_code, error.code), (400, "input_too_large"))
        self.assertIn("缩短", str(error))
        self.assertEqual(backend.poll_timeouts, [])

    def test_input_too_large_but_task_running_keeps_waiting(self):
        backend = FakeBackend(sse(START, TOO_LARGE, metadata(False, "unknown")), doc=document(DOC_TOOL_CALL))
        _, error, _ = self.run_stream(backend, images=["ref"])
        self.assertIsInstance(error, ImagePollTimeoutError)
        self.assertEqual(backend.poll_timeouts, [540])

    def test_copy_only_reply_on_an_edit_does_not_wait_for_an_image(self):
        backend = FakeBackend(sse(START, PROGRESS, PROGRESS, assistant(COPY_ONLY), metadata(False, "multimodal")),
                              doc=document(doc_text(COPY_ONLY)))
        _, error, sleeps = self.run_stream(backend, images=["ref"])
        self.assertIsInstance(error, conversation.ImageNotStartedError)
        self.assertEqual(error.reason, "text_only")
        self.assertEqual(backend.poll_timeouts, [])  # 以前这里会等满 60 秒
        self.assertLessEqual(sleeps.total, conversation.NOT_STARTED_PROBE_WAIT_SECS)
        self.assertEqual(backend.doc_reads, 1)  # 只快速确认一次

    def test_empty_reply_after_long_input_is_reported_as_not_started(self):
        backend = FakeBackend(sse(START, PROGRESS, metadata(False, "unknown")))
        _, error, _ = self.run_stream(backend, images=["ref"])
        self.assertIsInstance(error, conversation.ImageNotStartedError)
        self.assertEqual(error.reason, "empty")
        self.assertEqual(backend.poll_timeouts, [])

    def test_cannot_generate_and_quota_texts_are_told_apart(self):
        for text, reason in ((CANNOT_GENERATE, "cannot_generate"), (QUOTA_EXHAUSTED, "quota_exhausted")):
            with self.subTest(reason=reason):
                backend = FakeBackend(sse(START, assistant(text), metadata(False, "chat")))
                _, error, _ = self.run_stream(backend)
                self.assertIsInstance(error, conversation.ImageNotStartedError)
                self.assertEqual(error.reason, reason)

    def test_policy_refusal_keeps_the_existing_handling(self):
        backend = FakeBackend(sse(START, assistant(POLICY), metadata(False, "chat")))
        outputs, error, _ = self.run_stream(backend)
        self.assertIsNone(error)
        self.assertEqual(outputs[-1].kind, "message")  # 由 _generate_single_image 转成 content_policy_violation
        self.assertEqual(outputs[-1].text, POLICY)
        self.assertEqual(backend.doc_reads, 0)

    def test_task_found_by_the_check_waits_on_the_same_account(self):
        backend = FakeBackend(sse(START, assistant(COPY_ONLY), metadata(False, "multimodal")),
                              doc=document(DOC_TOOL_CALL))
        _, error, _ = self.run_stream(backend, images=["ref"])
        self.assertIsInstance(error, ImagePollTimeoutError)
        self.assertTrue(getattr(error, "task_started", False))
        self.assertEqual(backend.poll_timeouts, [540])  # 用剩下的总时限，而不是单号的 60 秒

    def test_unfinished_task_in_the_task_list_counts_as_running(self):
        task = {"conversation_id": CONV_ID, "image_gen_message": {"author": {"role": "tool"}, "metadata": {},
                                                                  "content": {"content_type": "multimodal_text"}}}
        backend = FakeBackend(sse(START, metadata(False, "unknown")), tasks=[task])
        _, error, _ = self.run_stream(backend, images=["ref"])
        self.assertTrue(getattr(error, "task_started", False))

    def test_started_turn_waits_the_remaining_budget_on_the_same_account(self):
        backend = FakeBackend(sse(START, PROGRESS, metadata(True, "image gen")))
        _, error, _ = self.run_stream(backend)
        self.assertIsInstance(error, ImagePollTimeoutError)
        self.assertTrue(error.task_started)
        self.assertEqual(backend.poll_timeouts, [540])

    def test_tool_invoked_for_another_tool_is_not_treated_as_drawing(self):
        backend = FakeBackend(sse(START, metadata(True, "data analysis")))
        _, error, _ = self.run_stream(backend, images=["ref"])
        self.assertIsInstance(error, ImagePollTimeoutError)
        self.assertFalse(getattr(error, "task_started", False))
        self.assertEqual(backend.poll_timeouts, [60])  # 原来的换号生成流程

    def test_failed_check_keeps_the_state_unknown(self):
        backend = FakeBackend(sse(START, assistant(COPY_ONLY), metadata(False, "multimodal")),
                              doc_error=RuntimeError("network down"))
        _, error, _ = self.run_stream(backend, images=["ref"])
        self.assertNotIsInstance(error, conversation.ImageNotStartedError)
        self.assertIsInstance(error, ImagePollTimeoutError)  # 走原来的等图流程
        self.assertEqual(backend.poll_timeouts, [60])

    def test_task_list_timeout_keeps_the_state_unknown_and_waits_on_the_same_account(self):
        # 会话里暂时没图，任务列表又查不到：不能当成没开画
        backend = FakeBackend(sse(START, assistant(COPY_ONLY), metadata(False, "multimodal")),
                              doc=document(doc_text(COPY_ONLY)), tasks_error=TimeoutError("tasks timed out"))
        _, error, _ = self.run_stream(backend)
        self.assertNotIsInstance(error, conversation.ImageNotStartedError)
        self.assertIsInstance(error, ImagePollTimeoutError)
        self.assertFalse(getattr(error, "task_started", False))
        self.assertEqual(backend.poll_timeouts, [60])  # 先在原账号上按单号时限等一轮

    def test_task_list_refusal_without_text_ends_with_the_refusal(self):
        backend = FakeBackend(sse(START, metadata(False, "unknown")), tasks=[POLICY_TASK])
        outputs, error, _ = self.run_stream(backend, images=["ref"])
        self.assertIsNone(error)
        self.assertEqual(outputs[-1].kind, "message")
        self.assertIn("内容政策", outputs[-1].text)
        self.assertEqual(backend.poll_timeouts, [])

    def test_tool_params_marked_as_task_error_still_counts_as_running(self):
        params_task = {"conversation_id": CONV_ID, "image_gen_message": {
            "author": {"role": "assistant"}, "metadata": {"is_error": True},
            "content": {"content_type": "text", "parts": ['{"size":"1024x1024","n":1}']}}}
        backend = FakeBackend(sse(START, metadata(False, "unknown")), tasks=[params_task])
        _, error, _ = self.run_stream(backend, images=["ref"])
        self.assertTrue(getattr(error, "task_started", False))

    def test_input_too_large_without_tool_invoked_is_checked_too(self):
        for doc, expected in ((document(), conversation.ImageInputTooLargeError),
                              (document(DOC_TOOL_CALL), ImagePollTimeoutError)):
            with self.subTest(expected=expected.__name__):
                backend = FakeBackend(sse(START, TOO_LARGE), doc=doc)  # 没有 server_ste_metadata
                _, error, _ = self.run_stream(backend, images=["ref"])
                self.assertIsInstance(error, expected)
                self.assertEqual(backend.doc_reads, 1)

    def test_missing_tool_invoked_keeps_the_state_unknown(self):
        backend = FakeBackend(sse(START, PROGRESS))
        _, error, _ = self.run_stream(backend, images=["ref"])
        self.assertIsInstance(error, ImagePollTimeoutError)
        self.assertEqual(backend.doc_reads, 0)
        self.assertEqual(backend.poll_timeouts, [60])

    def test_chat_endpoints_get_the_text_back_without_waiting(self):
        backend = FakeBackend(sse(START, assistant(COPY_ONLY), metadata(False, "multimodal")),
                              doc=document(doc_text(COPY_ONLY)))
        outputs, error, _ = self.run_stream(backend, images=["ref"], message_as_error=False)
        self.assertIsNone(error)
        self.assertEqual((outputs[-1].kind, outputs[-1].text), ("message", COPY_ONLY))
        self.assertEqual(backend.poll_timeouts, [])

    def test_quota_reset_time_comes_from_the_text_then_the_account(self):
        self.assertEqual(conversation.quota_reset_secs(QUOTA_EXHAUSTED), 23 * 3600 + 10 * 60)
        self.assertEqual(conversation.quota_reset_secs("limit resets in 2 hours"), 7200)
        account = {"limits_progress": [{"feature_name": "image_gen", "reset_after": "2000-01-01T00:00:00+00:00"}]}
        self.assertEqual(conversation.quota_reset_secs("额度已用完", account), conversation.QUOTA_EXHAUSTED_DEFAULT_SECS)
        self.assertEqual(conversation.quota_reset_secs("", None), conversation.QUOTA_EXHAUSTED_DEFAULT_SECS)

    def test_text_classification(self):
        for text, expected in (
            ("", "empty"),
            ("目前无法在此对话中直接生成图片。", "cannot_generate"),
            ("目前无法直接在此对话中生成图片，但可以帮你整理提示词", "cannot_generate"),
            ("我目前无法在这个聊天中直接生成或编辑图片", "cannot_generate"),
            ("I can't generate images in this chat, but here is a prompt.", "cannot_generate"),
            ("我目前无法直接在此对图片进行生成或编辑", "cannot_generate"),  # 线上原话
            ("我无法在此生成这类内容，因为它违反了我们的内容政策", "policy"),  # 政策字眼优先
            (QUOTA_EXHAUSTED, "quota_exhausted"),
            ("You've hit the free plan limit for image generation requests. It resets in 5 hours.", "quota_exhausted"),
            (POLICY, "policy"),
            (COPY_ONLY, "text_only"),
        ):
            with self.subTest(text=text[:20]):
                self.assertEqual(conversation.classify_not_started_text(text), expected)


class GenerateAttemptTests(unittest.TestCase):
    """_generate_single_image：按判断结果换号、暂停派工或结束。"""

    def setUp(self):
        conversation._cannot_generate_seen.clear()
        self.addCleanup(conversation._cannot_generate_seen.clear)

    def run_case(self, backends: dict, *, tokens=("one", "two", "three", "four", "five"), images=None,
                 failover=True, prompt="a cat", request=None):
        request = request or conversation.ConversationRequest(
            model="gpt-image-2", prompt=prompt, images=images, message_as_error=True)
        service = mock.Mock()
        service.get_account.side_effect = lambda token: {"email": f"account{tokens.index(token) + 1}@example.com"}

        def pick(**kwargs):
            free = [token for token in tokens if token not in kwargs["excluded_tokens"]]
            if not free:
                raise RuntimeError("no available image quota")
            return free[0]

        service.get_available_access_token.side_effect = pick
        settings = {"image_failover_enabled": failover, "image_failover_secs": 60, "image_poll_timeout_secs": 600}
        patches = [mock.patch.object(type(conversation.config), name, new_callable=mock.PropertyMock, return_value=value)
                   for name, value in settings.items()]
        patches += [
            mock.patch.object(conversation, "account_service", service),
            mock.patch.object(conversation, "OpenAIBackendAPI", side_effect=lambda access_token: backends[access_token]),
            mock.patch.object(conversation, "_remove_image_conversation_later"),
            mock.patch.object(conversation, "format_image_result", fake_result),
            mock.patch.object(conversation.time, "sleep", Sleeps()),
        ]
        with contextlib.ExitStack() as stack:
            for patcher in patches:
                stack.enter_context(patcher)
            try:
                result, error = conversation._generate_single_image(request, 1, 1), None
            except Exception as exc:  # noqa: BLE001
                result, error = None, exc
        used = [call.kwargs for call in service.get_available_access_token.call_args_list]
        return result, error, service, used

    @staticmethod
    def copy_only():
        return FakeBackend(sse(START, PROGRESS, assistant(COPY_ONLY), metadata(False, "multimodal")),
                           doc=document(doc_text(COPY_ONLY)))

    @staticmethod
    def drawing():
        return FakeBackend(sse(START, PROGRESS, IMAGE_TOOL_MESSAGE, metadata(True, "image gen")))

    def assert_one_release_per_account(self, service, accounts):
        released = [call.args[0] for call in service.mark_image_result.call_args_list] + \
                   [call.args[0] for call in service.release_image_slot.call_args_list]
        self.assertEqual(sorted(released), sorted(accounts))

    def test_copy_only_switches_once_then_reports_not_started(self):
        backends = {token: self.copy_only() for token in ("one", "two", "three")}
        _, error, service, used = self.run_case(backends, images=["ref"])
        self.assertIsInstance(error, conversation.ImageNotStartedError)
        self.assertEqual(error.code, "image_generation_not_started")
        self.assertIn("上游未启动生图", str(error))
        self.assertEqual(len(used), 2)  # 只换一个号
        self.assertIn("one", used[1]["excluded_tokens"])
        self.assertEqual(backends["one"].poll_timeouts + backends["two"].poll_timeouts, [])  # 没空等
        service.mark_image_result.assert_not_called()  # 只回文案不算账号故障
        self.assert_one_release_per_account(service, ["one", "two"])

    def test_copy_only_then_a_working_account_succeeds(self):
        result, error, service, used = self.run_case({"one": self.copy_only(), "two": self.drawing()}, images=["ref"])
        self.assertIsNone(error)
        self.assertEqual(result[-1].kind, "result")
        service.mark_image_result.assert_called_once_with("two", True)
        self.assert_one_release_per_account(service, ["one", "two"])

    def test_cannot_generate_switches_to_an_untried_account(self):
        cannot = FakeBackend(sse(START, assistant(CANNOT_GENERATE), metadata(False, "chat")))
        result, error, service, used = self.run_case({"one": cannot, "two": self.drawing()})
        self.assertIsNone(error)
        self.assertIn("one", used[1]["excluded_tokens"])
        service.mark_image_result.assert_any_call("one", False)  # 账号自己的问题，记一次失败
        service.sideline_image_account.assert_not_called()  # 只出现一次，不暂停派工
        self.assert_one_release_per_account(service, ["one", "two"])

    def test_same_account_repeating_cannot_generate_across_requests_is_sidelined(self):
        for _ in range(2):  # 两个不同的请求
            cannot = FakeBackend(sse(START, assistant(CANNOT_GENERATE), metadata(False, "chat")))
            _, error, service, _ = self.run_case({"one": cannot, "two": self.drawing()})
            self.assertIsNone(error)
        service.sideline_image_account.assert_called_once_with("one", conversation.CANNOT_GENERATE_COOLDOWN_SECS)

    def test_quota_exhausted_pauses_new_work_until_the_reset(self):
        quota = FakeBackend(sse(START, assistant(QUOTA_EXHAUSTED), metadata(False, "chat")))
        result, error, service, used = self.run_case({"one": quota, "two": self.drawing()})
        self.assertIsNone(error)
        service.sideline_image_account.assert_called_once_with("one", 23 * 3600 + 10 * 60)
        self.assertNotIn(mock.call("one", False), service.mark_image_result.call_args_list)

    def test_input_too_large_is_a_400_and_not_an_account_failure(self):
        too_large = FakeBackend(sse(START, TOO_LARGE, metadata(False, "unknown")))
        _, error, service, used = self.run_case({"one": too_large}, images=["ref"])
        self.assertIsInstance(error, conversation.ImageInputTooLargeError)
        self.assertEqual(error.status_code, 400)
        self.assertEqual(len(used), 1)  # 换号也一样，不换
        service.mark_image_result.assert_not_called()
        service.release_image_slot.assert_called_once_with("one")

    def test_input_too_large_with_an_image_in_the_same_turn_succeeds(self):
        mixed = FakeBackend(sse(START, IMAGE_TOOL_MESSAGE, TOO_LARGE, metadata(True, "image gen")))
        result, error, service, _ = self.run_case({"one": mixed})
        self.assertIsNone(error)
        self.assertEqual(result[-1].kind, "result")
        service.mark_image_result.assert_called_once_with("one", True)

    def test_started_task_timeout_does_not_switch_accounts(self):
        slow = FakeBackend(sse(START, PROGRESS, metadata(True, "image gen")))
        _, error, service, used = self.run_case({"one": slow, "two": self.drawing()})
        self.assertIsInstance(error, ImagePollTimeoutError)
        self.assertIn("同一个账号", str(error))
        self.assertEqual(len(used), 1)  # 不换号，免得重复开画
        self.assertEqual(slow.poll_timeouts, [600])
        self.assert_one_release_per_account(service, ["one"])

    def test_no_untried_account_left_reports_the_not_started_reason(self):
        _, error, _, used = self.run_case({"one": self.copy_only()}, tokens=("one",), images=["ref"])
        self.assertIsInstance(error, conversation.ImageNotStartedError)

    def test_progress_events_do_not_block_the_tool_params_text_retry(self):
        text = '我来生成：{"size":"1024x1024","n":1}'
        calls = []

        def stream(backend, request, index, total):
            calls.append(backend)
            yield conversation.ImageOutput(kind="progress", model=request.model, index=index, total=total)
            if len(calls) == 1:
                yield conversation.ImageOutput(kind="message", model=request.model, index=index, total=total, text=text)
            else:
                yield conversation.ImageOutput(kind="result", model=request.model, index=index, total=total, data=[{"url": "x"}])

        with mock.patch.object(conversation, "stream_image_outputs", side_effect=stream):
            result, error, service, used = self.run_case({"one": mock.Mock(), "two": mock.Mock()})
        self.assertIsNone(error)
        self.assertEqual(len(used), 2)
        self.assertIn("one", used[1]["excluded_tokens"])  # 不会又选回原账号

    def test_logs_record_the_decision_without_prompt_or_token(self):
        secret_prompt = "SECRET-PROMPT-第一行内容"
        token = "eyJ-very-secret-access-token-0123456789"
        with mock.patch.object(conversation, "logger") as logger:
            self.run_case({token: self.copy_only(), "two": self.copy_only()}, tokens=(token, "two"),
                          images=["ref"], prompt=secret_prompt)
        payloads = [call.args[0] for method in (logger.info, logger.warning, logger.debug)
                    for call in method.call_args_list]
        dumped = json.dumps(payloads, ensure_ascii=False, default=str)
        self.assertNotIn(secret_prompt, dumped)
        self.assertNotIn(token, dumped)
        outcomes = [item for item in payloads if item.get("event") == "image_attempt_outcome"]
        self.assertEqual([item["decision"] for item in outcomes], ["switch", "finish"])
        self.assertEqual(outcomes[0]["conversation_id"], CONV_ID)
        self.assertEqual(outcomes[0]["tool_invoked"], False)
        self.assertEqual(outcomes[0]["task_state"], "none")
        self.assertEqual(outcomes[0]["reason"], "not_started:text_only")
        assessed = [item for item in payloads if item.get("event") == "image_turn_assessed"]
        self.assertTrue(assessed and assessed[0]["task_state"] == "none")


def png_data_url() -> str:
    import base64
    from io import BytesIO

    from PIL import Image

    buffer = BytesIO()
    Image.new("RGB", (8, 8), "white").save(buffer, format="PNG")
    return "data:image/png;base64," + base64.b64encode(buffer.getvalue()).decode("ascii")


class ImageApiEntryTests(unittest.TestCase):
    """从 /v1/images/generations 入口验证最终的 HTTP 状态和错误码（单张、多张）。"""

    def setUp(self):
        import os
        from fastapi import FastAPI
        from fastapi.testclient import TestClient
        import api.ai as ai_module
        import services.log_service as log_module

        conversation._cannot_generate_seen.clear()
        self.addCleanup(conversation._cannot_generate_seen.clear)
        stack = contextlib.ExitStack()
        self.addCleanup(stack.close)
        stack.enter_context(mock.patch.dict(os.environ, {"CHATGPT2API_AUTH_KEY": "chatgpt2api"}))
        stack.enter_context(mock.patch.object(log_module.log_service, "add"))
        self.stack = stack
        app = FastAPI()
        app.include_router(ai_module.create_router())
        self.client = TestClient(app)

    def post(self, backends, *, n=1, parallel=True, tokens=("one", "two", "three"), path="/v1/images/generations"):
        service = mock.Mock()
        service.get_account.side_effect = lambda token: {"email": f"account{tokens.index(token) + 1}@example.com"}
        lock = threading.Lock()

        def pick(**kwargs):
            with lock:
                free = [token for token in tokens if token not in kwargs["excluded_tokens"]]
                if not free:
                    raise RuntimeError("no available image quota")
                return free[0]

        service.get_available_access_token.side_effect = pick
        settings = {"image_failover_enabled": True, "image_failover_secs": 60, "image_poll_timeout_secs": 600,
                    "image_parallel_generation": parallel}
        with contextlib.ExitStack() as stack:
            for name, value in settings.items():
                stack.enter_context(mock.patch.object(type(conversation.config), name, new_callable=mock.PropertyMock,
                                                      return_value=value))
            stack.enter_context(mock.patch.object(conversation, "account_service", service))
            stack.enter_context(mock.patch.object(conversation, "OpenAIBackendAPI",
                                                  side_effect=lambda access_token: backends[access_token]))
            stack.enter_context(mock.patch.object(conversation, "_remove_image_conversation_later"))
            stack.enter_context(mock.patch.object(conversation, "format_image_result", fake_result))
            stack.enter_context(mock.patch.object(conversation.time, "sleep", Sleeps()))
            body = {"model": "gpt-image-2", "prompt": "a cat", "n": n}
            if path == "/v1/images/edits":
                body["images"] = [{"image_url": png_data_url()}]
            response = self.client.post(path, headers={"Authorization": "Bearer chatgpt2api"}, json=body)
        picked = [call.kwargs for call in service.get_available_access_token.call_args_list]
        return response, picked

    @staticmethod
    def code(response):
        return response.json()["error"]["code"]

    def test_input_too_large_is_400_for_one_and_for_many_images(self):
        for n, parallel in ((1, True), (2, True), (2, False)):
            with self.subTest(n=n, parallel=parallel):
                too_large = FakeBackend(sse(START, TOO_LARGE, metadata(False, "unknown")))
                response, picked = self.post({"one": too_large}, n=n, parallel=parallel)
                self.assertEqual(response.status_code, 400, response.text)
                self.assertEqual(self.code(response), "input_too_large")
                self.assertIn("缩短", response.json()["error"]["message"])
                # 每张图只取一次号，不换号（串行时第一张失败就结束）
                self.assertEqual(len(picked), n if parallel else 1)

    def test_not_started_keeps_its_code_for_one_and_for_many_images(self):
        def copy_only():
            return FakeBackend(sse(START, assistant(COPY_ONLY), metadata(False, "chat")), doc=document(doc_text(COPY_ONLY)))

        for n in (1, 2):
            with self.subTest(n=n):
                response, picked = self.post({"one": copy_only(), "two": copy_only(), "three": copy_only()}, n=n)
                self.assertEqual(response.status_code, 502, response.text)
                self.assertEqual(self.code(response), "image_generation_not_started")
                self.assertIn("上游未启动生图", response.json()["error"]["message"])
                self.assertEqual(len(picked), 2 * n)  # 每张图最多换一个号

    def test_policy_refusal_takes_one_account_and_ends(self):
        for name, backend in (
            ("reply text", FakeBackend(sse(START, assistant(POLICY), metadata(False, "chat")))),
            ("task list", FakeBackend(sse(START, metadata(False, "unknown")), tasks=[POLICY_TASK])),
        ):
            with self.subTest(source=name):
                response, picked = self.post({"one": backend})
                self.assertEqual(response.status_code, 400, response.text)
                self.assertEqual(self.code(response), "content_policy_violation")
                self.assertEqual(len(picked), 1)

    def test_edit_stream_policy_refusal_ends_on_one_account_without_waiting(self):
        # 串流已明确回了政策拒绝，之后的会话是空的：不进入等图，也不换号
        for name, meta in (("tool not invoked", metadata(False, "multimodal")),
                           ("refused after the tool ran", metadata(True, "image gen"))):
            with self.subTest(case=name):
                refused = FakeBackend(sse(START, PROGRESS, assistant(IMAGE_POLICY), meta), doc=document())
                response, picked = self.post({"one": refused, "two": refused}, path="/v1/images/edits")
                self.assertEqual(response.status_code, 400, response.text)
                self.assertEqual(self.code(response), "content_policy_violation")
                self.assertEqual(len(picked), 1)
                self.assertEqual(refused.poll_timeouts, [])
                self.assertEqual(refused.doc_reads, 0)

    def test_task_list_service_error_is_an_upstream_error_not_a_policy_violation(self):
        for name, backend, path in (
            # tool_invoked=false：快速查询时在任务列表里看到
            ("checked after tool_invoked=false", FakeBackend(sse(START, metadata(False, "unknown")),
                                                             tasks=[SERVICE_DOWN_TASK]), "/v1/images/edits"),
            # 没带 tool_invoked 的文生图：原来的「等图前查任务」也看到
            ("checked before waiting", FakeBackend(sse(START), tasks=[SERVICE_DOWN_TASK]), "/v1/images/generations"),
        ):
            with self.subTest(case=name):
                response, picked = self.post({"one": backend, "two": backend}, path=path)
                self.assertEqual(response.status_code, 502, response.text)
                self.assertEqual(self.code(response), "upstream_service_error")
                self.assertIn("服务暂时不可用", response.json()["error"]["message"])
                self.assertEqual(len(picked), 1)
                self.assertEqual(backend.poll_timeouts, [])

    def test_task_check_failure_waits_on_the_same_account_before_switching(self):
        unsure = FakeBackend(sse(START, assistant(COPY_ONLY), metadata(False, "multimodal")),
                             doc=document(doc_text(COPY_ONLY)), tasks_error=TimeoutError("tasks timed out"))
        drawing = FakeBackend(sse(START, PROGRESS, IMAGE_TOOL_MESSAGE, metadata(True, "image gen")))
        response, picked = self.post({"one": unsure, "two": drawing})
        self.assertEqual(response.status_code, 200, response.text)
        self.assertEqual(unsure.poll_timeouts, [60])  # 先在原账号上等满一轮才换号
        self.assertEqual(len(picked), 2)

    def test_copy_only_then_success_returns_the_image(self):
        copy_only = FakeBackend(sse(START, assistant(COPY_ONLY), metadata(False, "chat")), doc=document(doc_text(COPY_ONLY)))
        drawing = FakeBackend(sse(START, IMAGE_TOOL_MESSAGE, metadata(True, "image gen")))
        response, picked = self.post({"one": copy_only, "two": drawing})
        self.assertEqual(response.status_code, 200, response.text)
        self.assertEqual(len(response.json()["data"]), 1)
        self.assertEqual(copy_only.poll_timeouts, [])


class PollTaskErrorTests(unittest.TestCase):
    """后端等图超时后，任务列表里的错误只有政策拒绝才算内容违规。"""

    def resolve_with_task_error(self, task_error):
        from services import openai_backend_api

        backend = OpenAIBackendAPI.__new__(OpenAIBackendAPI)
        timeout = ImagePollTimeoutError("ChatGPT 生图超时（已等待 60 秒）。", CONV_ID)
        timeout.task_error = task_error
        backend._poll_image_results = mock.Mock(side_effect=timeout)
        with mock.patch.object(type(openai_backend_api.config), "image_check_before_hit_enabled",
                               new_callable=mock.PropertyMock, return_value=False):
            backend.resolve_conversation_image_urls(CONV_ID, [], [], poll_timeout_secs=60)

    def test_service_error_is_not_a_policy_violation(self):
        from services.openai_backend_api import ImageContentPolicyError, ImageUpstreamTaskError

        with self.assertRaises(ImageUpstreamTaskError):
            self.resolve_with_task_error("服务暂时不可用，请稍后再试。")
        with self.assertRaises(ImageContentPolicyError):
            self.resolve_with_task_error("非常抱歉，生成的图片可能违反了我们的内容政策。")


class SharedAccountSlotTests(unittest.TestCase):
    """同一账号同时跑两个任务，其中一个失败：只还一个并发名额，另一个任务不受影响。"""

    def test_failed_task_releases_exactly_one_slot(self):
        with tempfile.TemporaryDirectory() as directory:
            service = AccountService(JSONStorageBackend(Path(directory) / "accounts.json"))
            service.add_account_items([{"access_token": "one", "status": "正常", "quota": 5}])
            service.fetch_remote_info = lambda token, *args, **kwargs: service.get_account(token)
            slow_started, finish_slow = threading.Event(), threading.Event()

            def stream(backend, request, index, total):
                yield conversation.ImageOutput(kind="progress", model=request.model, index=index, total=total)
                if request.prompt == "slow":
                    slow_started.set()
                    finish_slow.wait(5)
                    yield conversation.ImageOutput(kind="result", model=request.model, index=index, total=total, data=[{"url": "x"}])
                # "fail"：收到进度，最后没有图片数据

            settings = {"image_failover_enabled": False, "image_account_concurrency": 2}
            patches = [mock.patch.object(type(conversation.config), name, new_callable=mock.PropertyMock, return_value=value)
                       for name, value in settings.items()]
            patches += [
                mock.patch.object(conversation, "account_service", service),
                mock.patch.object(conversation, "OpenAIBackendAPI"),
                mock.patch.object(conversation, "stream_image_outputs", side_effect=stream),
                mock.patch.object(conversation, "_remove_image_conversation_later"),
            ]
            with contextlib.ExitStack() as stack:
                for patcher in patches:
                    stack.enter_context(patcher)
                slow_result = {}
                slow = threading.Thread(target=lambda: slow_result.setdefault("outputs", conversation._generate_single_image(
                    conversation.ConversationRequest(model="gpt-image-2", prompt="slow", message_as_error=True), 1, 1)))
                slow.start()
                self.assertTrue(slow_started.wait(5))
                self.assertEqual(service._image_inflight.get("one"), 1)

                with self.assertRaises(conversation.ImageGenerationError) as failed:
                    conversation._generate_single_image(
                        conversation.ConversationRequest(model="gpt-image-2", prompt="fail", message_as_error=True), 1, 1)
                self.assertEqual(failed.exception.code, "no_image_generated")
                # 失败的那个只还自己的名额：慢任务还占着 1 个
                self.assertEqual(service._image_inflight.get("one"), 1)
                self.assertEqual(service.get_account("one")["fail"], 1)  # 也只记一次失败

                finish_slow.set()
                slow.join(5)
            self.assertEqual(slow_result["outputs"][-1].kind, "result")
            self.assertNotIn("one", service._image_inflight)


if __name__ == "__main__":
    unittest.main()
