from __future__ import annotations

import json
import urllib.error
import urllib.parse
import urllib.request
from typing import Any, Mapping

JsonObject = dict[str, Any]


class CcSearchError(RuntimeError):
    """An HTTP error returned by the cc-search service."""

    def __init__(self, status: int, message: str, code: str | None = None, body: Any = None):
        super().__init__(message)
        self.status = status
        self.message = message
        self.code = code
        self.body = body


class CcSearchClient:
    """Small standard-library client for the local cc-search OpenAPI service."""

    def __init__(self, base_url: str = "http://127.0.0.1:8765", timeout: float = 30.0):
        self.base_url = base_url.rstrip("/")
        self.timeout = timeout

    def _request(
        self,
        method: str,
        path: str,
        params: Mapping[str, object | None] | None = None,
    ) -> JsonObject:
        query = []
        for key, value in (params or {}).items():
            if value is None:
                continue
            encoded = str(value).lower() if isinstance(value, bool) else str(value)
            query.append((key, encoded))
        url = f"{self.base_url}{path}"
        if query:
            url += "?" + urllib.parse.urlencode(query)
        request = urllib.request.Request(
            url,
            method=method,
            headers={"accept": "application/json"},
        )
        try:
            with urllib.request.urlopen(request, timeout=self.timeout) as response:
                return json.loads(response.read().decode("utf-8"))
        except urllib.error.HTTPError as error:
            body = _decode_body(error.read())
            if isinstance(body, dict):
                message = str(body.get("error", error.reason))
                code = body.get("code")
                raise CcSearchError(error.code, message, str(code) if code else None, body) from error
            raise CcSearchError(error.code, str(error.reason), body=body) from error
        except urllib.error.URLError as error:
            raise CcSearchError(0, str(error.reason)) from error

    def health(self) -> JsonObject:
        return self._request("GET", "/v1/health")

    def openapi(self) -> JsonObject:
        return self._request("GET", "/openapi.json")

    def search(
        self,
        pattern: str,
        *,
        limit: int | None = None,
        window_messages: int | None = None,
        window_hours: int | None = None,
        session: str | None = None,
        message_type: str | None = None,
        include_current: bool | None = None,
        any: bool | None = None,
        raw: bool | None = None,
        all: bool | None = None,
        full: bool | None = None,
        preview_length: int | None = None,
        budget: int | None = None,
    ) -> JsonObject:
        return self._request("GET", "/v1/search", {
            "pattern": pattern,
            "limit": limit,
            "window_messages": window_messages,
            "window_hours": window_hours,
            "session": session,
            "type": message_type,
            "include_current": include_current,
            "any": any,
            "raw": raw,
            "all": all,
            "full": full,
            "preview_length": preview_length,
            "budget": budget,
        })

    def last(
        self,
        *,
        count: int | None = None,
        hours: int | None = None,
        session: str | None = None,
        message_type: str | None = None,
        all: bool | None = None,
        full: bool | None = None,
        preview_length: int | None = None,
        budget: int | None = None,
    ) -> JsonObject:
        return self._request("GET", "/v1/last", {
            "count": count,
            "hours": hours,
            "session": session,
            "type": message_type,
            "all": all,
            "full": full,
            "preview_length": preview_length,
            "budget": budget,
        })

    def read(
        self,
        message_id: str,
        *,
        before: int | None = None,
        after: int | None = None,
        all: bool | None = None,
        full: bool | None = None,
        preview_length: int | None = None,
        budget: int | None = None,
    ) -> JsonObject:
        return self._request("GET", "/v1/read", {
            "id": message_id,
            "before": before,
            "after": after,
            "all": all,
            "full": full,
            "preview_length": preview_length,
            "budget": budget,
        })

    def rebuild(self, session: str | None = None) -> JsonObject:
        return self._request("POST", "/v1/rebuild", {"session": session})


def _decode_body(data: bytes) -> Any:
    try:
        return json.loads(data.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError):
        return data.decode("utf-8", errors="replace")
