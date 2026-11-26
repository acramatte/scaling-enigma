import io
import json
import socket
import threading
import time
import unittest
import urllib.error
import urllib.request

from PIL import Image, UnidentifiedImageError
import uvicorn

import embed_service


def make_png_bytes(width: int = 8, height: int = 8) -> bytes:
    buf = io.BytesIO()
    Image.new("RGB", (width, height), color=(36, 84, 58)).save(buf, format="PNG")
    return buf.getvalue()


class StubRuntime(embed_service.EmbedderRuntime):
    """Test runtime that exercises real PIL decoding and the shared InferenceLimiter."""

    def __init__(
        self,
        *,
        max_concurrency: int = 1,
        acquire_timeout_seconds: float = 5.0,
        max_image_bytes: int = 1024 * 1024,
        delay_seconds: float = 0.0,
        block_event: threading.Event | None = None,
        entered_event: threading.Event | None = None,
    ) -> None:
        super().__init__(
            model_id="test-siglip2",
            vision_session=object(),
            vision_input_name="pixel_values",
            vision_embedding_output="pooler_output",
            text_session=object(),
            tokenizer=object(),
            text_embedding_output="pooler_output",
            limiter=embed_service.InferenceLimiter(
                max_concurrency=max_concurrency,
                acquire_timeout_seconds=acquire_timeout_seconds,
            ),
            max_image_bytes=max_image_bytes,
        )
        self.delay_seconds = delay_seconds
        self.block_event = block_event
        self.entered_event = entered_event
        self._lock = threading.Lock()
        self.active = 0
        self.peak_active = 0

    def _run_guarded(self, vector: list[float]) -> dict:
        with self._lock:
            self.active += 1
            self.peak_active = max(self.peak_active, self.active)
        if self.entered_event is not None:
            self.entered_event.set()
        try:
            if self.block_event is not None:
                self.block_event.wait(timeout=5.0)
            if self.delay_seconds > 0:
                time.sleep(self.delay_seconds)
            return embed_service.embedding_response(vector, self.model_id)
        finally:
            with self._lock:
                self.active -= 1

    def embed_image(self, image_bytes: bytes) -> dict:
        try:
            with Image.open(io.BytesIO(image_bytes)) as opened:
                opened.convert("RGB")
        except (UnidentifiedImageError, OSError, ValueError) as exc:
            raise embed_service.InvalidImageError(f"invalid image: {exc}") from exc
        return self._run_guarded([1.0, 0.0, 0.0])

    def embed_text(self, text: str) -> dict:
        return self._run_guarded([0.0, 1.0, 0.0])


class RunningServer:
    def __init__(self, app) -> None:
        sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        sock.bind(("127.0.0.1", 0))
        self.port = sock.getsockname()[1]
        sock.close()
        self.base_url = f"http://127.0.0.1:{self.port}"
        config = uvicorn.Config(
            app, host="127.0.0.1", port=self.port, log_level="error", ws="none"
        )
        self.server = uvicorn.Server(config)
        self.thread = threading.Thread(target=self.server.run, daemon=True)

    def __enter__(self) -> "RunningServer":
        self.thread.start()
        deadline = time.monotonic() + 5.0
        while time.monotonic() < deadline:
            try:
                status, _ = http_json("GET", f"{self.base_url}/health")
                if status == 200:
                    return self
            except OSError:
                time.sleep(0.02)
        raise RuntimeError("embed_service test server did not become healthy")

    def __exit__(self, *_exc) -> None:
        self.server.should_exit = True
        self.thread.join(timeout=5.0)


def http_json(method: str, url: str, body: dict | None = None) -> tuple[int, dict]:
    data = json.dumps(body).encode("utf-8") if body is not None else None
    headers = {"Content-Type": "application/json"} if body is not None else {}
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=5.0) as resp:
            return resp.status, json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as err:
        return err.code, json.loads(err.read().decode("utf-8"))


def post_image(url: str, filename: str, payload: bytes) -> tuple[int, dict, dict]:
    boundary = "----semantic-search-test-boundary"
    body = (
        f"--{boundary}\r\n"
        f'Content-Disposition: form-data; name="file"; filename="{filename}"\r\n'
        f"Content-Type: application/octet-stream\r\n\r\n"
    ).encode("utf-8") + payload + f"\r\n--{boundary}--\r\n".encode("utf-8")
    req = urllib.request.Request(
        url,
        data=body,
        headers={"Content-Type": f"multipart/form-data; boundary={boundary}"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=5.0) as resp:
            return resp.status, dict(resp.headers), json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as err:
        return err.code, dict(err.headers), json.loads(err.read().decode("utf-8"))


class EmbedServiceTests(unittest.TestCase):
    def test_shared_semaphore_serializes_image_and_text_with_limit_one(self) -> None:
        runtime = StubRuntime(max_concurrency=1, delay_seconds=0.12)
        app = embed_service.create_app(runtime)
        png = make_png_bytes()

        with RunningServer(app) as srv:
            results: list[int] = []
            lock = threading.Lock()

            def call_image() -> None:
                code, _, _ = post_image(f"{srv.base_url}/embed/image", "a.png", png)
                with lock:
                    results.append(code)

            def call_text() -> None:
                code, _ = http_json("POST", f"{srv.base_url}/embed/text", {"text": "sunset"})
                with lock:
                    results.append(code)

            threads = [
                threading.Thread(target=call_image),
                threading.Thread(target=call_text),
                threading.Thread(target=call_image),
            ]
            for thread in threads:
                thread.start()
            for thread in threads:
                thread.join(timeout=5.0)

        self.assertEqual(results, [200, 200, 200])
        self.assertEqual(runtime.peak_active, 1)

    def test_shared_semaphore_honors_higher_configured_concurrency(self) -> None:
        runtime = StubRuntime(max_concurrency=2, delay_seconds=0.15)
        app = embed_service.create_app(runtime)
        png = make_png_bytes()

        with RunningServer(app) as srv:
            results: list[int] = []
            lock = threading.Lock()

            def call_image() -> None:
                code, _, _ = post_image(f"{srv.base_url}/embed/image", "a.png", png)
                with lock:
                    results.append(code)

            threads = [threading.Thread(target=call_image) for _ in range(3)]
            for thread in threads:
                thread.start()
            for thread in threads:
                thread.join(timeout=5.0)

        self.assertEqual(results, [200, 200, 200])
        self.assertEqual(runtime.peak_active, 2)

    def test_health_stays_responsive_and_saturation_returns_503(self) -> None:
        entered = threading.Event()
        release = threading.Event()
        runtime = StubRuntime(
            max_concurrency=1,
            acquire_timeout_seconds=0.15,
            block_event=release,
            entered_event=entered,
        )
        app = embed_service.create_app(runtime)
        png = make_png_bytes()

        with RunningServer(app) as srv:
            first_status: list[int] = []

            def run_first() -> None:
                code, _, _ = post_image(f"{srv.base_url}/embed/image", "slow.png", png)
                first_status.append(code)

            worker = threading.Thread(target=run_first)
            worker.start()
            self.assertTrue(entered.wait(timeout=2.0), "first inference did not enter slot")

            # /health must respond immediately while the single slot is held.
            health_code, health_body = http_json("GET", f"{srv.base_url}/health")
            self.assertEqual(health_code, 200)
            self.assertEqual(health_body["inference_max_concurrency"], 1)
            self.assertEqual(health_body["inference_in_flight"], 1)

            # A concurrent text request saturates the slot and times out with 503.
            text_code, text_body = http_json(
                "POST", f"{srv.base_url}/embed/text", {"text": "mountain"}
            )
            self.assertEqual(text_code, 503)
            self.assertIn("saturated", text_body["detail"])

            release.set()
            worker.join(timeout=5.0)
            self.assertEqual(first_status, [200])

            # Slot is released cleanly after completion.
            _, after_health = http_json("GET", f"{srv.base_url}/health")
            self.assertEqual(after_health["inference_in_flight"], 0)
            self.assertEqual(after_health["inference_waiting"], 0)

    def test_permanent_image_rejection_status_codes(self) -> None:
        runtime = StubRuntime(max_concurrency=1, max_image_bytes=64)
        app = embed_service.create_app(runtime)

        with RunningServer(app) as srv:
            empty_code, _, _ = post_image(f"{srv.base_url}/embed/image", "empty.jpg", b"")
            self.assertEqual(empty_code, 422)

            corrupt_code, _, _ = post_image(
                f"{srv.base_url}/embed/image", "corrupt.jpg", b"not a real jpeg"
            )
            self.assertEqual(corrupt_code, 422)

            oversized_code, _, _ = post_image(
                f"{srv.base_url}/embed/image", "huge.jpg", b"x" * 100
            )
            self.assertEqual(oversized_code, 413)

    def test_env_validation(self) -> None:
        with self.assertRaises(ValueError):
            embed_service.InferenceLimiter(max_concurrency=0)
        with self.assertRaises(ValueError):
            embed_service.InferenceLimiter(max_concurrency=1, acquire_timeout_seconds=-1.0)


if __name__ == "__main__":
    unittest.main()
