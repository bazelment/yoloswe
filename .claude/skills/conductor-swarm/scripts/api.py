#!/usr/bin/env python3
"""Conductor workspace and session operations for conductor-swarm."""

import argparse
import json
import os
import sys
import uuid
from urllib.error import HTTPError, URLError
from urllib.parse import quote, urlencode
from urllib.request import Request, urlopen


API_BASE = "https://api.conductor.build/v0"
API_KEYS_URL = "https://app.conductor.build/home/api-keys"


class ConductorError(Exception):
    """An API response or transport failure that cannot be treated as success."""


class ConductorClient:
    def __init__(self, api_key=None, *, base_url=API_BASE, opener=urlopen):
        self.api_key = api_key or os.environ.get("CONDUCTOR_API_KEY")
        if not self.api_key:
            raise ConductorError(
                f"set CONDUCTOR_API_KEY; create one at {API_KEYS_URL} if needed"
            )
        self.base_url = base_url.rstrip("/")
        self.opener = opener

    def _request(self, method, path, *, body=None, params=None):
        url = self.base_url + path
        if params:
            url += "?" + urlencode(params)
        data = json.dumps(body).encode("utf-8") if body is not None else None
        headers = {
            "Authorization": f"Bearer {self.api_key}",
            "User-Agent": "conductor-swarm/1",
        }
        if data is not None:
            headers["Content-Type"] = "application/json"
        request = Request(url, data=data, headers=headers, method=method)
        try:
            with self.opener(request, timeout=30) as response:
                payload = response.read()
        except HTTPError as error:
            payload = error.read()
            try:
                detail = json.loads(payload)
                message = detail.get("userMessage") or detail.get("message") or error.reason
            except (ValueError, AttributeError):
                message = error.reason
            raise ConductorError(f"Conductor HTTP {error.code}: {message}") from None
        except URLError as error:
            raise ConductorError(f"Conductor request failed: {error.reason}") from None
        if not payload:
            raise ConductorError(f"Conductor returned an empty response for {method} {path}")
        try:
            result = json.loads(payload)
        except ValueError as error:
            raise ConductorError(f"Conductor returned invalid JSON for {method} {path}") from error
        if not isinstance(result, dict):
            raise ConductorError(f"Conductor returned a non-object response for {method} {path}")
        return result

    @staticmethod
    def _id(value):
        if not value:
            raise ValueError("an ID is required")
        return quote(value, safe="")

    @staticmethod
    def _page(result):
        data = result.get("data")
        if not isinstance(data, list) or not isinstance(result.get("hasMore"), bool):
            raise ConductorError("Conductor returned an invalid list page")
        if result["hasMore"] and not data:
            raise ConductorError("Conductor returned an empty page with hasMore=true")
        return data

    def _paginated(self, path, filters=None):
        items = []
        offset = 0
        while True:
            params = {"limit": 100, "offset": offset}
            if filters:
                params.update(filters)
            page = self._request("GET", path, params=params)
            data = self._page(page)
            if page.get("offset") != offset:
                raise ConductorError("Conductor returned an unexpected list offset")
            items.extend(data)
            if not page["hasMore"]:
                return items
            offset += len(data)

    def projects(self):
        return self._paginated("/projects")

    def workspaces(self, project_id):
        return self._paginated(f"/projects/{self._id(project_id)}/workspaces")

    def find_workspaces(self, name, repo_url):
        matches = self._paginated("/workspaces", {"name": name, "repo": repo_url})
        canonical_repo = repo_url.rstrip("/").removesuffix(".git")
        return [item for item in matches if item.get("name") == name and
                (item.get("repoUrl") or "").rstrip("/").removesuffix(".git") == canonical_repo]

    def sessions(self, workspace_id):
        return self._paginated(f"/workspaces/{self._id(workspace_id)}/sessions")

    @staticmethod
    def _agent_body(name, agent, model, brief, effort, kind):
        if not brief.strip():
            raise ValueError(f"{kind} brief is empty")
        body = {"name": name, "agent": agent, "model": model, "message": brief}
        if effort:
            body["effort"] = effort
        return body

    def create_workspace(self, *, project_id=None, repository_url=None, branch, name,
                         agent, model, brief, effort=None):
        if bool(project_id) == bool(repository_url):
            raise ValueError("provide exactly one of project_id or repository_url")
        body = {
            "projectId" if project_id else "repositoryUrl": project_id or repository_url,
            "branch": branch,
            **self._agent_body(name, agent, model, brief, effort, "workspace"),
        }
        result = self._request("POST", "/workspaces", body=body)
        for field in ("workspaceId", "sessionId", "deepLink"):
            if not result.get(field):
                raise ConductorError(f"workspace creation response lacks {field}; reconcile before retrying")
        return result

    def create_session(self, *, workspace_id, name, agent, model, brief, effort=None):
        body = {
            "workspaceId": workspace_id,
            **self._agent_body(name, agent, model, brief, effort, "session"),
        }
        result = self._request("POST", "/sessions", body=body)
        if not result.get("id"):
            raise ConductorError("session creation response lacks id; reconcile before retrying")
        return result

    def send_message(self, session_id, message, *, message_id):
        if not message.strip():
            raise ValueError("follow-up message is empty")
        try:
            uuid.UUID(message_id)
        except (ValueError, AttributeError) as error:
            raise ValueError("message_id must be a UUID") from error
        return self._request(
            "POST", f"/sessions/{self._id(session_id)}/messages",
            body={"message": message, "messageId": message_id},
        )

    def workspace(self, workspace_id):
        return self._request("GET", f"/workspaces/{self._id(workspace_id)}")

    def workspace_status(self, workspace_id):
        return self._request("GET", f"/workspaces/{self._id(workspace_id)}/status")

    def session_status(self, session_id):
        return self._request("GET", f"/sessions/{self._id(session_id)}/status")

    def messages_since(self, session_id, after=None):
        messages = []
        cursor = after
        while True:
            params = {"limit": 100}
            if cursor:
                params["after"] = cursor
            page = self._request(
                "GET", f"/sessions/{self._id(session_id)}/messages", params=params,
            )
            data = self._page(page)
            if data:
                last = data[-1]
                if not isinstance(last, dict) or not last.get("id") or last["id"] == cursor:
                    raise ConductorError("Conductor returned a transcript page without a new message ID")
                cursor = last["id"]
                messages.extend(data)
            if not page["hasMore"]:
                return {"data": messages, "lastMessageId": cursor}

    def observe_session(self, session_id, after=None):
        return {
            "status": self.session_status(session_id),
            "transcript": self.messages_since(session_id, after),
        }

    def cancel_session(self, session_id):
        return self._request("POST", f"/sessions/{self._id(session_id)}/cancel")

    def sleep_workspace(self, workspace_id):
        return self._request("POST", f"/workspaces/{self._id(workspace_id)}/sleep")

    def archive_workspace(self, workspace_id):
        return self._request("POST", f"/workspaces/{self._id(workspace_id)}/archive")


def brief_from_file(path):
    with open(path, encoding="utf-8") as file:
        brief = file.read()
    if not brief.strip():
        raise ValueError("brief file is empty")
    return brief


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    commands.add_parser("projects")
    workspaces = commands.add_parser("workspaces")
    workspaces.add_argument("--project-id", required=True)
    find = commands.add_parser("find-workspaces")
    find.add_argument("--name", required=True)
    find.add_argument("--repo-url", required=True)
    sessions = commands.add_parser("sessions")
    sessions.add_argument("--workspace-id", required=True)

    workspace = commands.add_parser("create-workspace")
    source = workspace.add_mutually_exclusive_group(required=True)
    source.add_argument("--project-id")
    source.add_argument("--repository-url")
    workspace.add_argument("--branch", required=True)
    workspace.add_argument("--name", required=True)
    workspace.add_argument("--agent", required=True)
    workspace.add_argument("--model", required=True)
    workspace.add_argument("--effort")
    workspace.add_argument("--brief-file", required=True)

    session = commands.add_parser("create-session")
    session.add_argument("--workspace-id", required=True)
    session.add_argument("--name", required=True)
    session.add_argument("--agent", required=True)
    session.add_argument("--model", required=True)
    session.add_argument("--effort")
    session.add_argument("--brief-file", required=True)

    message = commands.add_parser("send")
    message.add_argument("--session-id", required=True)
    message.add_argument("--brief-file", required=True)
    message.add_argument("--message-id", help="reuse this UUID if retrying an uncertain send")

    for name in ("workspace", "workspace-status", "sleep-workspace", "archive-workspace"):
        command = commands.add_parser(name)
        command.add_argument("--workspace-id", required=True)
    for name in ("session-status", "cancel-session", "messages", "observe-session"):
        command = commands.add_parser(name)
        command.add_argument("--session-id", required=True)
        if name in ("messages", "observe-session"):
            command.add_argument("--after")

    args = parser.parse_args()
    try:
        client = ConductorClient()
        if args.command == "projects":
            result = {"data": client.projects()}
        elif args.command == "workspaces":
            result = {"data": client.workspaces(args.project_id)}
        elif args.command == "find-workspaces":
            result = {"data": client.find_workspaces(args.name, args.repo_url)}
        elif args.command == "sessions":
            result = {"data": client.sessions(args.workspace_id)}
        elif args.command == "create-workspace":
            result = client.create_workspace(
                project_id=args.project_id, repository_url=args.repository_url,
                branch=args.branch, name=args.name, agent=args.agent, model=args.model,
                effort=args.effort, brief=brief_from_file(args.brief_file),
            )
        elif args.command == "create-session":
            result = client.create_session(
                workspace_id=args.workspace_id, name=args.name, agent=args.agent,
                model=args.model, effort=args.effort,
                brief=brief_from_file(args.brief_file),
            )
        elif args.command == "send":
            brief = brief_from_file(args.brief_file)
            message_id = args.message_id or str(uuid.uuid4())
            print(f"messageId for retry: {message_id}", file=sys.stderr)
            result = client.send_message(
                args.session_id, brief, message_id=message_id,
            )
        elif args.command == "messages":
            result = client.messages_since(args.session_id, args.after)
        elif args.command == "observe-session":
            result = client.observe_session(args.session_id, args.after)
        else:
            method = getattr(client, args.command.replace("-", "_"))
            identifier = args.workspace_id if hasattr(args, "workspace_id") else args.session_id
            result = method(identifier)
    except (ConductorError, OSError, ValueError) as error:
        parser.exit(1, f"{error}\n")
    print(json.dumps(result, indent=2))


if __name__ == "__main__":
    main()
