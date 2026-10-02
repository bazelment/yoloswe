#!/usr/bin/env python3
"""Small authenticated client for Conductor's versioned REST API."""

import argparse
import json
import os
import sys
from urllib.error import HTTPError, URLError
from urllib.request import Request, urlopen


API_BASE = "https://api.conductor.build/v0"


def request(method, path, data_file=None, *, key=None, base=API_BASE):
    if not path.startswith("/") or path.startswith("//") or ".." in path.split("?")[0].split("/"):
        raise ValueError("path must be relative to /v0")
    if method == "GET" and data_file:
        raise ValueError("GET does not take a request body")
    if method == "POST" and not data_file:
        raise ValueError("POST requires --data-file (use a file containing {} for an empty body)")

    token = key or os.environ.get("CONDUCTOR_API_KEY")
    if not token:
        raise ValueError("set CONDUCTOR_API_KEY; create one at https://app.conductor.build/home/api-keys if needed")
    body = None
    if data_file:
        with open(data_file, "rb") as file:
            body = file.read()
        json.loads(body)
    headers = {"Authorization": f"Bearer {token}", "User-Agent": "conductor-swarm/1"}
    if body is not None:
        headers["Content-Type"] = "application/json"
    req = Request(base + path, data=body, headers=headers, method=method)
    try:
        with urlopen(req, timeout=30) as response:
            payload = response.read()
    except HTTPError as error:
        payload = error.read()
        try:
            detail = json.loads(payload)
            message = detail.get("userMessage") or detail.get("message") or error.reason
        except (ValueError, AttributeError):
            message = error.reason
        raise RuntimeError(f"Conductor HTTP {error.code}: {message}") from None
    except URLError as error:
        raise RuntimeError(f"Conductor request failed: {error.reason}") from None
    return json.loads(payload) if payload else {}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("method", choices=["GET", "POST"])
    parser.add_argument("path", help="path below /v0, including optional query string")
    parser.add_argument("--data-file", help="JSON request body; required for POST")
    args = parser.parse_args()
    try:
        result = request(args.method, args.path, args.data_file)
    except (ValueError, OSError, RuntimeError) as error:
        parser.exit(1, f"{error}\n")
    print(json.dumps(result, indent=2))


if __name__ == "__main__":
    main()
