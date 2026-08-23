import json
import pathlib
import sys
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

sys.path.insert(0, str(pathlib.Path(__file__).parent))
from cc_search_client import CcSearchClient, CcSearchError  # noqa: E402


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        parsed = urlparse(self.path)
        query = parse_qs(parsed.query)
        if parsed.path == "/v1/search":
            body = {
                "results": [{"id": "m1", "sessionId": "s1"}],
                "total": 1,
                "truncated": False,
                "relaxed": False,
                "budget": {"limit": 60000, "spent": 2, "dropped": 0, "shrunk": False},
                "pattern": query.get("pattern", [""])[0],
            }
            self.send_response(200)
        else:
            body = {"version": 1, "error": "missing", "code": "not_found"}
            self.send_response(404)
        self.send_header("content-type", "application/json")
        self.end_headers()
        self.wfile.write(json.dumps(body).encode())

    def log_message(self, *_args):
        pass


class ClientTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        cls.thread = threading.Thread(target=cls.server.serve_forever, daemon=True)
        cls.thread.start()
        cls.client = CcSearchClient(f"http://127.0.0.1:{cls.server.server_port}")

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        cls.thread.join()

    def test_search_encodes_typed_query(self):
        result = self.client.search("auth flow", full=True, limit=3)
        self.assertEqual(result["pattern"], "auth flow")
        self.assertEqual(result["results"][0]["id"], "m1")

    def test_http_errors_are_typed(self):
        with self.assertRaises(CcSearchError) as raised:
            self.client.read("missing")
        self.assertEqual(raised.exception.status, 404)
        self.assertEqual(raised.exception.code, "not_found")


if __name__ == "__main__":
    unittest.main()
