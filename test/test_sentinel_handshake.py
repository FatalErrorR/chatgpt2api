from __future__ import annotations

import base64
import json
import unittest
from unittest import mock

from services import sentinel_solver_client
from services.openai_backend_api import ChatRequirements, OpenAIBackendAPI
from services import openai_backend_api
from utils.pow import build_legacy_requirements_token, build_pow_config, build_proof_token

SCRIPT_SOURCES = ["https://chatgpt.com/backend-api/sentinel/sdk.js"]


class PowTokenTests(unittest.TestCase):
    def test_requirements_token_is_the_25_field_config(self):
        token = build_legacy_requirements_token("UA", SCRIPT_SOURCES, "prod-build")
        self.assertTrue(token.startswith("gAAAAAC"))
        config = json.loads(base64.b64decode(token[len("gAAAAAC"):]))
        self.assertEqual(len(config), 25)
        # The counter fields the proof of work mutates.
        self.assertEqual(config[3], 1)
        self.assertIsInstance(config[9], float)

    def test_proof_token_uses_fnv_and_carries_the_marker_suffix(self):
        proof = build_proof_token("0.5", "00ff", "UA", SCRIPT_SOURCES, "prod-build")
        self.assertTrue(proof.startswith("gAAAAAB"))
        self.assertTrue(proof.endswith("~S"))
        body = json.loads(base64.b64decode(proof[len("gAAAAAB"):-len("~S")]))
        self.assertEqual(len(body), 25)

    def test_pow_solution_meets_the_requested_difficulty(self):
        from utils.pow import _fnv1a_32, _pow_generate

        config = build_pow_config("UA", SCRIPT_SOURCES, "prod-build")
        payload, solved = _pow_generate("seed-1", "00ff", config)
        self.assertTrue(solved)
        self.assertLessEqual(_fnv1a_32("seed-1" + payload)[:4], "00ff")


class ImageModelSlugTests(unittest.TestCase):
    def test_web_image_models_map_to_auto(self):
        api = OpenAIBackendAPI("")
        for model in ("gpt-image-2", "auto", ""):
            self.assertEqual(api._image_model_settings(model), ("auto", ""), model)

    def test_web_image_2_5_slug_is_passed_through(self):
        # fork 的 Images 2.5 支持：2.5 系列 slug 保持透传，不降成 auto。
        api = OpenAIBackendAPI("")
        self.assertEqual(api._image_model_settings("gpt-image-2.5"), ("gpt-image-2.5", ""))

    def test_codex_image_model_keeps_its_own_slug(self):
        self.assertEqual(OpenAIBackendAPI("")._image_model_settings("codex-gpt-image-2"), ("codex-gpt-image-2", ""))


class SolverClientTests(unittest.TestCase):
    def setUp(self):
        sentinel_solver_client._user_agent_cache.update(url="", value="", expires_at=0.0)

    def _config(self, url: str = "http://solver:8080", key: str = "secret"):
        return mock.patch.object(
            sentinel_solver_client,
            "config",
            mock.Mock(sentinel_solver_url=url, sentinel_solver_key=key),
        )

    def test_unconfigured_solver_fails_closed(self):
        with self._config(url=""):
            with self.assertRaises(sentinel_solver_client.SentinelSolverError):
                sentinel_solver_client.solve_turnstile("dx", "p")

    def test_solved_token_is_returned(self):
        response = mock.Mock(status_code=200)
        response.json.return_value = {"token": "solved-token"}
        with self._config(), mock.patch.object(sentinel_solver_client.requests, "post", return_value=response) as post:
            token = sentinel_solver_client.solve_turnstile("dx-value", "p-value")
        self.assertEqual(token, "solved-token")
        self.assertEqual(post.call_args.kwargs["json"], {"dx": "dx-value", "p": "p-value", "proxy": ""})
        self.assertEqual(post.call_args.kwargs["headers"]["Authorization"], "Bearer secret")

    def test_empty_token_fails_closed(self):
        response = mock.Mock(status_code=200)
        response.json.return_value = {"token": ""}
        with self._config(), mock.patch.object(sentinel_solver_client.requests, "post", return_value=response):
            with self.assertRaises(sentinel_solver_client.SentinelSolverError):
                sentinel_solver_client.solve_turnstile("dx", "p")

    def test_solver_error_status_fails_closed(self):
        response = mock.Mock(status_code=503, text="turnstile solve failed")
        with self._config(), mock.patch.object(sentinel_solver_client.requests, "post", return_value=response):
            with self.assertRaises(sentinel_solver_client.SentinelSolverError):
                sentinel_solver_client.solve_turnstile("dx", "p")

    def test_user_agent_is_read_from_health_and_cached(self):
        response = mock.Mock(status_code=200)
        response.json.return_value = {"user_agent": "UA-from-solver"}
        with self._config(), mock.patch.object(
            sentinel_solver_client.requests, "get", return_value=response
        ) as get:
            self.assertEqual(sentinel_solver_client.solver_user_agent(), "UA-from-solver")
            self.assertEqual(sentinel_solver_client.solver_user_agent(), "UA-from-solver")
        self.assertEqual(get.call_count, 1)

    def test_unreachable_solver_leaves_user_agent_empty(self):
        with self._config(), mock.patch.object(
            sentinel_solver_client.requests, "get", side_effect=OSError("connection refused")
        ):
            self.assertEqual(sentinel_solver_client.solver_user_agent(), "")

    def test_prepare_response_is_forwarded_without_account_material(self):
        response = mock.Mock(status_code=200)
        response.json.return_value = {"token": "solved"}
        requirements = {"prepare_token": "prepare", "turnstile": {"required": True, "dx": "dx"}}
        with self._config(), mock.patch.object(sentinel_solver_client.requests, "post", return_value=response) as post:
            sentinel_solver_client.solve_turnstile("dx", "p", requirements)
        self.assertEqual(post.call_args.kwargs["json"], {"dx": "dx", "p": "p", "proxy": "", "requirements": requirements})


    def test_egress_proxy_is_forwarded(self):
        response = mock.Mock(status_code=200)
        response.json.return_value = {"token": "solved"}
        with self._config(), mock.patch.object(sentinel_solver_client.requests, "post", return_value=response) as post:
            sentinel_solver_client.solve_turnstile("dx", "p", proxy="http://user:pass@proxy:8118")
        self.assertEqual(post.call_args.kwargs["json"]["proxy"], "http://user:pass@proxy:8118")


class SentinelHandshakeTests(unittest.TestCase):
    def test_solver_gets_the_proxy_this_account_egresses_through(self):
        with mock.patch.object(openai_backend_api.account_service, "get_account",
                               return_value={"proxy": "http://user:pass@account-proxy:8118"}):
            api = OpenAIBackendAPI("token")
        self.addCleanup(api.close)
        self.assertEqual(api.proxy_url, "http://user:pass@account-proxy:8118")

    def test_prepare_finalize_uses_page_user_agent_and_local_turnstile(self):
        api = OpenAIBackendAPI("")
        self.addCleanup(api.close)
        api.session.cookies.set("oai-sc", "restricted", domain="chatgpt.com")
        api.session.cookies.set("keep", "value", domain="chatgpt.com")
        prepare = {"prepare_token": "prepare", "proofofwork": {"required": True, "seed": "seed", "difficulty": "f"},
                   "turnstile": {"required": True, "dx": "dx"}}
        responses = [mock.Mock(status_code=200), mock.Mock(status_code=200)]
        responses[0].json.return_value = prepare
        responses[1].json.return_value = {"token": "requirements"}
        with (
            mock.patch.object(api.session, "post", side_effect=responses) as post,
            mock.patch.object(openai_backend_api, "build_legacy_requirements_token", return_value="p") as build,
            mock.patch.object(openai_backend_api, "build_proof_token", return_value="proof") as proof,
            mock.patch.object(openai_backend_api, "solve_turnstile_token", return_value="turnstile") as solve,
        ):
            result = api._get_chat_requirements()
        self.assertEqual(build.call_args.args[0], api.user_agent)
        self.assertEqual(proof.call_args.args[2], api.user_agent)
        solve.assert_called_once_with("dx", "p")
        self.assertEqual(post.call_args_list[1].kwargs["json"], {
            "prepare_token": "prepare", "proof_token": "proof", "turnstile_token": "turnstile"})
        self.assertEqual(result.token, "requirements")
        self.assertEqual(api.session.cookies.get("oai-sc"), "restricted")
        self.assertEqual(api.session.cookies.get("keep"), "value")

    def test_turnstile_without_dx_is_sent_empty(self):
        api = OpenAIBackendAPI("")
        self.addCleanup(api.close)
        responses = [mock.Mock(status_code=200), mock.Mock(status_code=200)]
        responses[0].json.return_value = {"prepare_token": "prepare", "turnstile": {"required": True}}
        responses[1].json.return_value = {"token": "requirements"}
        with (
            mock.patch.object(api.session, "post", side_effect=responses) as post,
            mock.patch.object(openai_backend_api, "solve_turnstile_token") as solve,
        ):
            result = api._get_chat_requirements()
        solve.assert_not_called()
        self.assertEqual(post.call_count, 2)
        self.assertEqual(result.token, "requirements")

    def test_stored_tls_profile_is_kept_and_empty_uses_chrome110(self):
        api = OpenAIBackendAPI("")
        self.addCleanup(api.close)
        api.account = {"impersonate": "chrome110"}
        self.assertEqual(api._build_fp()["impersonate"], "chrome110")
        api.account = {"impersonate": "edge101"}
        self.assertEqual(api._build_fp()["impersonate"], "edge101")
        api.account = {}
        self.assertEqual(api._build_fp()["impersonate"], "chrome110")
        api.account = {"impersonate": "chrome145"}
        self.assertEqual(api._build_fp()["impersonate"], "chrome145")

    def test_image_prepare_and_conversation_share_trace_and_current_payload(self):
        api = OpenAIBackendAPI("")
        self.addCleanup(api.close)
        requirements = ChatRequirements(token="requirements", proof_token="proof", turnstile_token="turnstile")
        response = mock.Mock(status_code=200)
        response.json.return_value = {"conduit_token": "conduit"}
        reference = {"file_id": "file-1", "width": 100, "height": 200, "file_size": 20,
                     "mime_type": "image/png", "file_name": "input.png"}
        for references in (None, [reference]):
            with self.subTest(edit=bool(references)), mock.patch.object(api.session, "post", return_value=response) as post:
                api._prepare_image_conversation("cat", requirements, "gpt-image-2")
                api._start_image_generation("cat", requirements, "conduit", "gpt-image-2", references)
            first, second = post.call_args_list
            trace = first.kwargs["headers"]["X-Oai-Turn-Trace-Id"]
            self.assertEqual(second.kwargs["headers"]["X-Oai-Turn-Trace-Id"], trace)
            for call in (first, second):
                self.assertEqual(call.kwargs["json"]["model"], "auto")
                self.assertEqual(call.kwargs["json"]["parent_message_id"], "client-created-root")
                self.assertEqual(call.kwargs["headers"]["X-OpenAI-Target-Path"],
                                 call.kwargs["headers"]["X-OpenAI-Target-Route"])
                self.assertEqual(call.kwargs["headers"]["OpenAI-Sentinel-Turnstile-Token"], "turnstile")
            message = second.kwargs["json"]["messages"][0]
            self.assertEqual(message["content"]["parts"][-1], "@创建图像 cat")
            self.assertEqual(message["metadata"]["serialization_metadata"]["custom_symbol_offsets"][0]["id"], "picture_v2")
            if references:
                self.assertEqual(message["content"]["parts"][0]["asset_pointer"], "file-service://file-1")


if __name__ == "__main__":
    unittest.main()
