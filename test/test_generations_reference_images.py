import base64
import unittest
from unittest import mock

from fastapi import FastAPI
from fastapi.testclient import TestClient

import api.ai as ai_module
import api.image_tasks as image_tasks_module

IDENTITY = {"id": "admin", "name": "admin", "role": "admin"}
PNG_DATA_URL = "data:image/png;base64," + base64.b64encode(b"fake-png").decode("ascii")


class GenerationsWithReferenceImageTests(unittest.TestCase):
    """部分客户端把参考图放进 generations 请求：带图时转去 edits，不带图时行为不变。"""

    def setUp(self):
        self.edits = []
        self.generations = []
        self.tasks = []
        for target, attr, value in (
            (ai_module.openai_v1_image_edit, "handle", lambda payload: self.edits.append(payload) or {"data": []}),
            (ai_module.openai_v1_image_generations, "handle", lambda payload: self.generations.append(payload) or {"data": []}),
            (ai_module, "filter_or_log", mock.AsyncMock()),
            (ai_module, "require_identity", mock.Mock(return_value=IDENTITY)),
            (image_tasks_module, "filter_or_log", mock.AsyncMock()),
            (image_tasks_module, "require_identity", mock.Mock(return_value=IDENTITY)),
            (image_tasks_module.image_task_service, "submit_edit", lambda *a, **kw: self.tasks.append(("edit", kw)) or {}),
            (image_tasks_module.image_task_service, "submit_generation", lambda *a, **kw: self.tasks.append(("generation", kw)) or {}),
        ):
            patcher = mock.patch.object(target, attr, value)
            patcher.start()
            self.addCleanup(patcher.stop)
        app = FastAPI()
        app.include_router(ai_module.create_router())
        app.include_router(image_tasks_module.create_router())
        self.client = TestClient(app)

    def test_v1_generations_with_images_goes_to_edits(self):
        response = self.client.post("/v1/images/generations", json={"prompt": "换成夜景", "images": [PNG_DATA_URL]})
        self.assertEqual(response.status_code, 200, response.text)
        self.assertEqual(self.generations, [])
        self.assertEqual(self.edits[0]["prompt"], "换成夜景")
        self.assertEqual(self.edits[0]["images"][0][0], b"fake-png")

    def test_v1_generations_without_images_is_unchanged(self):
        response = self.client.post(
            "/v1/images/generations",
            json={"prompt": "猫", "output_format": "gif", "frame_count": 8, "images": []},
        )
        self.assertEqual(response.status_code, 200, response.text)
        self.assertEqual(self.edits, [])
        self.assertEqual(self.generations[0]["frame_count"], 8)

    def test_v1_generations_still_validates_without_images(self):
        response = self.client.post("/v1/images/generations", json={"prompt": "猫", "n": 9})
        self.assertEqual(response.status_code, 422)
        self.assertEqual(self.generations, [])

    def test_image_task_generation_with_image_goes_to_edit_task(self):
        response = self.client.post(
            "/api/image-tasks/generations",
            json={"client_task_id": "t1", "prompt": "换成夜景", "image": PNG_DATA_URL},
        )
        self.assertEqual(response.status_code, 200, response.text)
        kind, kwargs = self.tasks[0]
        self.assertEqual(kind, "edit")
        self.assertEqual(kwargs["client_task_id"], "t1")
        self.assertEqual(kwargs["images"][0][0], b"fake-png")

    def test_image_task_generation_without_image_is_unchanged(self):
        response = self.client.post("/api/image-tasks/generations", json={"client_task_id": "t2", "prompt": "猫"})
        self.assertEqual(response.status_code, 200, response.text)
        self.assertEqual(self.tasks[0][0], "generation")


if __name__ == "__main__":
    unittest.main()
