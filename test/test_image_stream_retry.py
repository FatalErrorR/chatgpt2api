from __future__ import annotations

import tempfile
import unittest
from pathlib import Path
from unittest import mock

from services.account_service import AccountService
from services.protocol import conversation
from services.storage.json_storage import JSONStorageBackend
from utils.helper import UpstreamHTTPError


class ImageRetryTestCase(unittest.TestCase):
    def _generate(self, errors, tokens=('one', 'two', 'three', 'four', 'five', 'six'), same_proxy=()):
        request = conversation.ConversationRequest(model='gpt-image-2', prompt='cat')
        service = mock.Mock()
        service.get_account.return_value = {}
        service.get_available_access_token.side_effect = list(tokens)
        service.same_proxy_tokens.return_value = set(same_proxy)
        output = conversation.ImageOutput(kind='result', model=request.model, index=1, total=1, data=[])

        def stream(*args):
            if errors:
                raise errors.pop(0)
            yield output

        with (
            mock.patch.object(conversation, 'account_service', service),
            mock.patch.object(conversation, 'OpenAIBackendAPI'),
            mock.patch.object(conversation, 'stream_image_outputs', side_effect=stream),
        ):
            try:
                return conversation._generate_single_image(request, 1, 1), service
            except conversation.ImageGenerationError:
                return None, service


class ImageModelNotFoundTests(ImageRetryTestCase):
    def test_only_exact_image_conversation_error_matches(self):
        for endpoint, status, body, expected in (
            ('/backend-api/f/conversation', 404, {'detail': 'Model not found'}, True),
            ('/backend-api/conversation', 404, {'detail': 'Model not found'}, False),
            ('/backend-api/f/conversation/prepare', 404, {'detail': 'Model not found'}, False),
            ('/backend-api/f/conversation', 403, {'detail': 'Model not found'}, False),
            ('/backend-api/f/conversation', 404, {'detail': 'Not Found'}, False),
            ('/backend-api/f/conversation', 404, {'detail': ' Model not found '}, False),
            ('/backend-api/f/conversation', 404, '', False),
        ):
            with self.subTest(endpoint=endpoint, status=status, body=body):
                self.assertEqual(conversation.is_image_model_not_found_error(
                    UpstreamHTTPError(endpoint, status, body)), expected)
        self.assertFalse(conversation.is_image_model_not_found_error('Model not found'))

    def test_account_cooldown_expires_without_changing_quota_or_status(self):
        with tempfile.TemporaryDirectory() as directory:
            service = AccountService(JSONStorageBackend(Path(directory) / 'accounts.json'))
            service.add_account_items([
                {'access_token': 'one', 'status': '正常', 'quota': 5},
                {'access_token': 'two', 'status': '正常', 'quota': 5},
            ])
            before = service.get_account('one')
            with mock.patch('services.account_service.time.monotonic', return_value=100):
                service.sideline_image_account('one', conversation.IMAGE_MODEL_NOT_FOUND_COOLDOWN_SECS)
                self.assertEqual(service._list_ready_candidate_tokens(), ['two'])
            with mock.patch('services.account_service.time.monotonic', return_value=700):
                self.assertEqual(set(service._list_ready_candidate_tokens()), {'one', 'two'})
            self.assertEqual(service.get_account('one'), before)
            self.assertEqual(conversation.IMAGE_MODEL_NOT_FOUND_COOLDOWN_SECS, 600)

    def test_disguised_404_moves_to_next_account(self):
        result, service = self._generate([
            UpstreamHTTPError('/backend-api/f/conversation', 404, {'detail': 'Model not found'})])
        self.assertEqual(len(result), 1)
        self.assertEqual(service.get_available_access_token.call_count, 2)
        service.sideline_image_account.assert_called_once_with('one', 600)
        service.refresh_access_token.assert_not_called()
        service.remove_invalid_token.assert_not_called()

    def test_other_404_is_not_retried(self):
        result, service = self._generate([
            UpstreamHTTPError('/backend-api/f/conversation', 404, {'detail': 'Not Found'})])
        self.assertIsNone(result)
        self.assertEqual(service.get_available_access_token.call_count, 1)
        service.sideline_image_account.assert_not_called()

    def test_failover_is_bounded_by_existing_account_retry_limit(self):
        result, service = self._generate([
            UpstreamHTTPError('/backend-api/f/conversation', 404, {'detail': 'Model not found'})
            for _ in range(6)])
        self.assertIsNone(result)
        self.assertEqual(service.get_available_access_token.call_count, 5)
        self.assertEqual(service.sideline_image_account.call_count, 5)


class ImageForbiddenTests(ImageRetryTestCase):
    CHALLENGE = '<html><body>Enable JavaScript and cookies to continue</body></html>'

    def _excluded(self, service, call_index):
        return service.get_available_access_token.call_args_list[call_index].kwargs['excluded_tokens']

    def test_403_cools_the_account_and_skips_its_egress_on_retry(self):
        result, service = self._generate(
            [UpstreamHTTPError('bootstrap', 403, self.CHALLENGE)], same_proxy={'one', 'sibling'})
        self.assertEqual(len(result), 1)
        service.sideline_image_account.assert_called_once_with('one', 600)
        self.assertEqual(self._excluded(service, 0), set())
        self.assertEqual(self._excluded(service, 1), {'one', 'sibling'})
        service.remove_invalid_token.assert_not_called()

    def test_403_falls_back_to_the_same_egress_when_nothing_else_is_free(self):
        result, service = self._generate(
            [UpstreamHTTPError('chat_requirements_prepare', 403, self.CHALLENGE)],
            tokens=['one', RuntimeError('no available image quota'), 'sibling'],
            same_proxy={'one', 'sibling'},
        )
        self.assertEqual(len(result), 1)
        self.assertEqual(self._excluded(service, 1), {'one', 'sibling'})
        self.assertEqual(self._excluded(service, 2), set())

    def test_403_retries_are_bounded(self):
        result, service = self._generate(
            [UpstreamHTTPError('bootstrap', 403, self.CHALLENGE) for _ in range(6)])
        self.assertIsNone(result)
        self.assertEqual(service.get_available_access_token.call_count, 4)
        self.assertEqual(service.sideline_image_account.call_count, 4)

    def test_same_proxy_tokens_only_groups_accounts_with_that_proxy(self):
        with tempfile.TemporaryDirectory() as directory:
            service = AccountService(JSONStorageBackend(Path(directory) / 'accounts.json'))
            service.add_account_items([
                {'access_token': 'a', 'proxy': 'http://1.1.1.1:80'},
                {'access_token': 'b', 'proxy': 'http://1.1.1.1:80'},
                {'access_token': 'c', 'proxy': 'http://2.2.2.2:80'},
                {'access_token': 'd'},
                {'access_token': 'e'},
            ])
            self.assertEqual(service.same_proxy_tokens('a'), {'a', 'b'})
            self.assertEqual(service.same_proxy_tokens('c'), {'c'})
            self.assertEqual(service.same_proxy_tokens('d'), set())
