#!/usr/bin/env python3
"""Boundary checks for conductor-swarm's HTTP client and CLI."""

import io
import json
import os
import tempfile
import unittest
from contextlib import redirect_stdout
from unittest.mock import patch
from urllib.error import HTTPError
from urllib.parse import parse_qs, urlsplit

import api


class Response:
    def __init__(self, body):
        self.body = json.dumps(body).encode("utf-8") if isinstance(body, dict) else body

    def __enter__(self):
        return self

    def __exit__(self, *_):
        return False

    def read(self):
        return self.body


class FakeOpener:
    def __init__(self, *responses):
        self.responses = iter(responses)
        self.calls = []

    def __call__(self, request, timeout):
        self.calls.append(request)
        response = next(self.responses)
        if isinstance(response, Exception):
            raise response
        return Response(response)


class ConductorClientTests(unittest.TestCase):
    def test_create_workspace_and_session_send_brief_once(self):
        opener = FakeOpener(
            {"workspaceId": "w", "sessionId": "s1", "deepLink": "https://example/w",
             "initialMessage": {"messageId": "m1", "state": "queued", "deepLink": "https://example/m1"}},
            {"id": "s2", "deepLink": "https://example/s2"},
        )
        client = api.ConductorClient("secret", opener=opener)
        workspace = client.create_workspace(
            project_id="p", branch="main", name="lane-a", agent="codex",
            model="gpt-5.5", brief="implement this", effort="high",
        )
        session = client.create_session(
            workspace_id="w", name="lane-a-review", agent="codex",
            model="gpt-5.5", brief="review this",
        )
        self.assertEqual((workspace["sessionId"], session["id"]), ("s1", "s2"))
        self.assertEqual(len(opener.calls), 2)
        self.assertEqual(opener.calls[0].get_header("Authorization"), "Bearer secret")
        self.assertEqual(json.loads(opener.calls[0].data)["message"], "implement this")
        self.assertEqual(json.loads(opener.calls[1].data)["message"], "review this")

    def test_project_and_transcript_pagination(self):
        opener = FakeOpener(
            {"data": [{"id": "p1"}], "offset": 0, "hasMore": True},
            {"data": [{"id": "p2"}], "offset": 1, "hasMore": False},
            {"data": [{"id": "m2"}], "offset": 0, "hasMore": True},
            {"data": [{"id": "m3"}], "offset": 0, "hasMore": False},
        )
        client = api.ConductorClient("secret", opener=opener)
        self.assertEqual([item["id"] for item in client.projects()], ["p1", "p2"])
        transcript = client.messages_since("session/one", after="m1")
        self.assertEqual(transcript["lastMessageId"], "m3")
        self.assertEqual([item["id"] for item in transcript["data"]], ["m2", "m3"])
        self.assertEqual(parse_qs(urlsplit(opener.calls[1].full_url).query)["offset"], ["1"])
        self.assertEqual(parse_qs(urlsplit(opener.calls[3].full_url).query)["after"], ["m2"])
        self.assertIn("session%2Fone", opener.calls[2].full_url)

    def test_reconcile_lists_and_observe_session(self):
        opener = FakeOpener(
            {"data": [{"id": "w"}], "offset": 0, "hasMore": False},
            {"data": [{"id": "s"}], "offset": 0, "hasMore": False},
            {"workspaceId": "w", "sessionId": "s", "status": "working", "updatedAt": "now"},
            {"data": [{"id": "m2"}], "offset": 0, "hasMore": False},
        )
        client = api.ConductorClient("secret", opener=opener)
        self.assertEqual(client.workspaces("p")[0]["id"], "w")
        self.assertEqual(client.sessions("w")[0]["id"], "s")
        observed = client.observe_session("s", after="m1")
        self.assertEqual(observed["status"]["status"], "working")
        self.assertEqual(observed["transcript"]["lastMessageId"], "m2")
        self.assertEqual(len(opener.calls), 4)

    def test_empty_pagination_and_response_are_unknown(self):
        client = api.ConductorClient("secret", opener=FakeOpener(
            {"data": [], "offset": 0, "hasMore": True}))
        with self.assertRaises(api.ConductorError):
            client.projects()
        client = api.ConductorClient("secret", opener=FakeOpener(b""))
        with self.assertRaises(api.ConductorError):
            client.session_status("s")
        client = api.ConductorClient("secret", opener=FakeOpener(
            {"data": [{"id": "p"}], "offset": 3, "hasMore": True}))
        with self.assertRaisesRegex(api.ConductorError, "offset"):
            client.projects()

    def test_api_key_requirement_and_error_redaction(self):
        with patch.dict(os.environ, {"CONDUCTOR_API_TOKEN": "wrong-token"}, clear=True):
            with self.assertRaisesRegex(api.ConductorError, "CONDUCTOR_API_KEY"):
                api.ConductorClient()
        error = HTTPError("https://example", 400, "bad request", {},
                          io.BytesIO(b'{"userMessage":"invalid model"}'))
        client = api.ConductorClient("secret", opener=FakeOpener(error))
        with self.assertRaisesRegex(api.ConductorError, "invalid model") as caught:
            client.session_status("s")
        self.assertNotIn("secret", str(caught.exception))

    def test_follow_up_requires_uuid_and_reuses_it(self):
        opener = FakeOpener({"messageId": "00000000-0000-4000-8000-000000000001",
                             "state": "queued", "deepLink": "https://example/message"})
        client = api.ConductorClient("secret", opener=opener)
        with self.assertRaisesRegex(ValueError, "UUID"):
            client.send_message("s", "brief", message_id="not-a-uuid")
        message_id = "00000000-0000-4000-8000-000000000001"
        client.send_message("s", "brief", message_id=message_id)
        self.assertEqual(len(opener.calls), 1)
        self.assertEqual(json.loads(opener.calls[0].data)["messageId"], message_id)

    def test_cli_create_workspace_reads_brief_file(self):
        opener = FakeOpener({"workspaceId": "w", "sessionId": "s", "deepLink": "https://example/w"})
        client = api.ConductorClient("secret", opener=opener)
        with tempfile.NamedTemporaryFile(mode="w+", encoding="utf-8") as brief:
            brief.write("self-contained brief\n")
            brief.flush()
            argv = ["api.py", "create-workspace", "--project-id", "p", "--branch", "main",
                    "--name", "lane-a", "--agent", "codex", "--model", "gpt-5.5",
                    "--brief-file", brief.name]
            with patch("sys.argv", argv), patch.object(api, "ConductorClient", return_value=client):
                with redirect_stdout(io.StringIO()) as output:
                    api.main()
        self.assertEqual(json.loads(output.getvalue())["workspaceId"], "w")
        self.assertEqual(json.loads(opener.calls[0].data)["message"], "self-contained brief\n")


if __name__ == "__main__":
    unittest.main()
