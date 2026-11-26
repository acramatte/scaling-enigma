import asyncio
from contextlib import asynccontextmanager
import io
import os
from pathlib import Path
from typing import Any

from fastapi import FastAPI, File, HTTPException, UploadFile
from PIL import Image, UnidentifiedImageError
from pydantic import BaseModel, Field
import uvicorn

BASE_DIR = Path(__file__).resolve().parent
MODEL_ID = os.environ.get("MODEL_ID", "google/siglip2-base-patch16-256")
VAIP_CONFIG_PATH = os.environ.get(
    "VAIP_CONFIG_PATH", str(BASE_DIR / "vaip_config.json")
)
VISION_MODEL_PATH = Path(
    os.environ.get(
        "VISION_MODEL_PATH",
        os.environ.get(
            "MODEL_PATH", BASE_DIR.parent / "models" / "vision_model_int8.onnx"
        ),
    )
)
TEXT_MODEL_PATH = Path(
    os.environ.get(
        "TEXT_MODEL_PATH", BASE_DIR.parent / "models" / "text_model_int8.onnx"
    )
)
TOKENIZER_PATH = Path(
    os.environ.get("TOKENIZER_PATH", BASE_DIR.parent / "models" / "tokenizer")
)
TEXT_MAX_LENGTH = 64
DEFAULT_INFERENCE_MAX_CONCURRENCY = 1
DEFAULT_INFERENCE_ACQUIRE_TIMEOUT_SECONDS = 30.0
DEFAULT_INFERENCE_MAX_IMAGE_BYTES = 32 * 1024 * 1024


class InvalidImageError(ValueError):
    """Raised when uploaded bytes cannot be decoded as a valid raster image."""


class InferenceOverloadedError(TimeoutError):
    """Raised when all inference slots remain busy past the acquire timeout."""


class TextEmbeddingRequest(BaseModel):
    text: str = Field(min_length=1, max_length=2_000)


def parse_positive_int_env(name: str, default: int) -> int:
    raw = os.environ.get(name, "").strip()
    if not raw:
        return default
    try:
        value = int(raw)
    except ValueError as exc:
        raise ValueError(f"{name} must be a positive integer, got {raw!r}") from exc
    if value < 1:
        raise ValueError(f"{name} must be at least 1, got {value}")
    return value


def parse_nonnegative_float_env(name: str, default: float) -> float:
    raw = os.environ.get(name, "").strip()
    if not raw:
        return default
    try:
        value = float(raw)
    except ValueError as exc:
        raise ValueError(f"{name} must be a non-negative number, got {raw!r}") from exc
    if value < 0:
        raise ValueError(f"{name} must be non-negative, got {value}")
    return value


class InferenceLimiter:
    """Shared configurable semaphore protecting ONNX sessions across endpoints."""

    def __init__(
        self,
        max_concurrency: int = DEFAULT_INFERENCE_MAX_CONCURRENCY,
        acquire_timeout_seconds: float = DEFAULT_INFERENCE_ACQUIRE_TIMEOUT_SECONDS,
    ) -> None:
        if max_concurrency < 1:
            raise ValueError("INFERENCE_MAX_CONCURRENCY must be at least 1")
        if acquire_timeout_seconds < 0:
            raise ValueError("INFERENCE_ACQUIRE_TIMEOUT_SECONDS must be non-negative")
        self.max_concurrency = max_concurrency
        self.acquire_timeout_seconds = acquire_timeout_seconds
        self._semaphore: asyncio.Semaphore | None = None
        self._loop: asyncio.AbstractEventLoop | None = None
        self.in_flight = 0
        self.waiting = 0

    def _get_semaphore(self) -> asyncio.Semaphore:
        loop = asyncio.get_running_loop()
        if self._semaphore is None or self._loop is not loop:
            self._semaphore = asyncio.Semaphore(self.max_concurrency)
            self._loop = loop
        return self._semaphore

    @asynccontextmanager
    async def slot(self):
        semaphore = self._get_semaphore()
        self.waiting += 1
        acquired = False
        try:
            if self.acquire_timeout_seconds > 0:
                await asyncio.wait_for(
                    semaphore.acquire(), timeout=self.acquire_timeout_seconds
                )
            else:
                await semaphore.acquire()
            acquired = True
        except TimeoutError as exc:
            raise InferenceOverloadedError(
                f"inference concurrency limit ({self.max_concurrency}) saturated "
                f"for {self.acquire_timeout_seconds:g}s"
            ) from exc
        finally:
            self.waiting -= 1

        self.in_flight += 1
        try:
            yield
        finally:
            self.in_flight -= 1
            if acquired:
                semaphore.release()


def build_providers() -> list:
    import onnxruntime as ort

    available_providers = ort.get_available_providers()
    providers = []

    if "VitisAIExecutionProvider" in available_providers:
        providers.append(
            ("VitisAIExecutionProvider", {"config_file": VAIP_CONFIG_PATH})
        )
    else:
        print(
            "VitisAIExecutionProvider is not available in this Python environment. "
            f"Available providers: {', '.join(available_providers)}"
        )

    providers.append("CPUExecutionProvider")
    return providers


def load_session(model_path: Path, providers: list) -> Any:
    import onnxruntime as ort

    print(f"Loading ONNX model: {model_path}")
    try:
        loaded_session = ort.InferenceSession(str(model_path), providers=providers)
    except Exception as exc:
        if providers == ["CPUExecutionProvider"]:
            raise

        print(
            f"NPU initialization failed for {model_path.name}; falling back to CPU: {exc}"
        )
        loaded_session = ort.InferenceSession(
            str(model_path), providers=["CPUExecutionProvider"]
        )

    print(
        f"Loaded {model_path.name}. Active providers: {loaded_session.get_providers()}"
    )
    return loaded_session


def preprocess_siglip2(image_bytes: bytes) -> Any:
    """Build the normalized NCHW tensor expected by SigLIP 2 base patch16-256."""
    try:
        with Image.open(io.BytesIO(image_bytes)) as opened:
            image = opened.convert("RGB")
            image = image.resize((256, 256), Image.Resampling.BILINEAR)
    except (UnidentifiedImageError, Image.DecompressionBombError, OSError, ValueError) as exc:
        raise InvalidImageError(f"invalid or unsupported image: {exc}") from exc

    import numpy as np

    image_data = np.asarray(image, dtype=np.float32) / 255.0
    image_data = (image_data - 0.5) / 0.5
    image_data = np.transpose(image_data, (2, 0, 1))
    return np.expand_dims(image_data, axis=0)


def normalize_embedding(raw_output: Any) -> list[float]:
    import numpy as np

    raw_vector = np.asarray(raw_output[0], dtype=np.float32).reshape(-1)
    norm = np.linalg.norm(raw_vector)
    if norm == 0:
        raise ValueError("model returned a zero-length embedding")
    return (raw_vector / norm).tolist()


def embedding_response(vector: list[float], model_id: str = MODEL_ID) -> dict:
    return {
        "embedding": vector,
        "dimensions": len(vector),
        "model": model_id,
    }


class EmbedderRuntime:
    """Owns loaded ONNX sessions, tokenizer, and the shared inference limiter."""

    def __init__(
        self,
        *,
        model_id: str,
        vision_session: Any,
        vision_input_name: str,
        vision_embedding_output: str,
        text_session: Any = None,
        tokenizer: Any = None,
        text_embedding_output: str | None = None,
        text_initialization_error: str | None = None,
        limiter: InferenceLimiter | None = None,
        max_image_bytes: int = DEFAULT_INFERENCE_MAX_IMAGE_BYTES,
    ) -> None:
        self.model_id = model_id
        self.vision_session = vision_session
        self.vision_input_name = vision_input_name
        self.vision_embedding_output = vision_embedding_output
        self.text_session = text_session
        self.tokenizer = tokenizer
        self.text_embedding_output = text_embedding_output
        self.text_initialization_error = text_initialization_error
        self.limiter = limiter or InferenceLimiter()
        self.max_image_bytes = max_image_bytes

    @classmethod
    def load_from_env(cls) -> "EmbedderRuntime":
        model_id = os.environ.get("MODEL_ID", MODEL_ID)
        vision_model_path = Path(
            os.environ.get(
                "VISION_MODEL_PATH",
                os.environ.get("MODEL_PATH", str(VISION_MODEL_PATH)),
            )
        )
        text_model_path = Path(os.environ.get("TEXT_MODEL_PATH", str(TEXT_MODEL_PATH)))
        tokenizer_path = Path(os.environ.get("TOKENIZER_PATH", str(TOKENIZER_PATH)))

        max_concurrency = parse_positive_int_env(
            "INFERENCE_MAX_CONCURRENCY", DEFAULT_INFERENCE_MAX_CONCURRENCY
        )
        acquire_timeout = parse_nonnegative_float_env(
            "INFERENCE_ACQUIRE_TIMEOUT_SECONDS",
            DEFAULT_INFERENCE_ACQUIRE_TIMEOUT_SECONDS,
        )
        max_image_bytes = parse_positive_int_env(
            "INFERENCE_MAX_IMAGE_BYTES", DEFAULT_INFERENCE_MAX_IMAGE_BYTES
        )

        if not vision_model_path.exists():
            raise FileNotFoundError(
                f"ONNX vision model not found: {vision_model_path}\n"
                "Set VISION_MODEL_PATH to the exported/quantized SigLIP 2 vision model."
            )

        providers = build_providers()
        vision_session = load_session(vision_model_path, providers)
        vision_input_name = vision_session.get_inputs()[0].name
        vision_output_names = {output.name for output in vision_session.get_outputs()}
        vision_embedding_output = (
            "pooler_output"
            if "pooler_output" in vision_output_names
            else vision_session.get_outputs()[-1].name
        )

        text_session = None
        tokenizer = None
        text_embedding_output = None
        text_initialization_error = None

        if text_model_path.exists() and tokenizer_path.exists():
            try:
                from transformers import AutoTokenizer

                tokenizer = AutoTokenizer.from_pretrained(
                    str(tokenizer_path), local_files_only=True
                )
                text_session = load_session(text_model_path, providers)
                text_output_names = {
                    output.name for output in text_session.get_outputs()
                }
                text_embedding_output = (
                    "pooler_output"
                    if "pooler_output" in text_output_names
                    else text_session.get_outputs()[-1].name
                )
            except Exception as exc:
                text_initialization_error = str(exc)
                print(f"Text embedding is unavailable: {exc}")
        else:
            text_initialization_error = (
                f"missing {text_model_path} or tokenizer directory {tokenizer_path}; "
                "download the text ONNX model and tokenizer artifacts first"
            )
            print(f"Text embedding is unavailable: {text_initialization_error}")

        return cls(
            model_id=model_id,
            vision_session=vision_session,
            vision_input_name=vision_input_name,
            vision_embedding_output=vision_embedding_output,
            text_session=text_session,
            tokenizer=tokenizer,
            text_embedding_output=text_embedding_output,
            text_initialization_error=text_initialization_error,
            limiter=InferenceLimiter(
                max_concurrency=max_concurrency,
                acquire_timeout_seconds=acquire_timeout,
            ),
            max_image_bytes=max_image_bytes,
        )

    def embed_image(self, image_bytes: bytes) -> dict:
        """Run CPU/NPU-bound preprocessing and inference outside the event loop."""
        tensor_data = preprocess_siglip2(image_bytes)
        raw_output = self.vision_session.run(
            [self.vision_embedding_output],
            {self.vision_input_name: tensor_data},
        )[0]
        return embedding_response(normalize_embedding(raw_output), self.model_id)

    def embed_text(self, text: str) -> dict:
        """Run CPU/NPU-bound tokenization and text inference outside the event loop."""
        import numpy as np

        encoded = self.tokenizer(
            text,
            padding="max_length",
            truncation=True,
            max_length=TEXT_MAX_LENGTH,
            return_tensors="np",
        )
        input_names = {model_input.name for model_input in self.text_session.get_inputs()}
        model_inputs = {
            name: np.asarray(encoded[name], dtype=np.int64)
            for name in input_names
            if name in encoded
        }
        missing_inputs = input_names.difference(model_inputs)
        if missing_inputs:
            raise ValueError(
                f"tokenizer did not produce model inputs: {sorted(missing_inputs)}"
            )

        raw_output = self.text_session.run([self.text_embedding_output], model_inputs)[0]
        return embedding_response(normalize_embedding(raw_output), self.model_id)


def create_app(runtime: EmbedderRuntime | None = None) -> FastAPI:
    @asynccontextmanager
    async def lifespan(application: FastAPI):
        if getattr(application.state, "runtime", None) is None:
            application.state.runtime = EmbedderRuntime.load_from_env()
        yield

    application = FastAPI(
        title="SigLIP 2 Embedding Service",
        lifespan=lifespan,
    )
    if runtime is not None:
        application.state.runtime = runtime

    def get_runtime() -> EmbedderRuntime:
        active = getattr(application.state, "runtime", None)
        if active is None:
            raise HTTPException(
                status_code=503, detail="embedding runtime is not initialized"
            )
        return active

    @application.get("/health")
    async def health():
        active = get_runtime()
        return {
            "status": "ok",
            "model": active.model_id,
            "image_embeddings": True,
            "text_embeddings": active.text_session is not None,
            "text_error": active.text_initialization_error,
            "inference_max_concurrency": active.limiter.max_concurrency,
            "inference_in_flight": active.limiter.in_flight,
            "inference_waiting": active.limiter.waiting,
        }

    @application.post("/embed/image")
    @application.post("/embed", include_in_schema=False)
    async def get_image_embedding(file: UploadFile = File(...)):
        active = get_runtime()
        contents = await file.read(active.max_image_bytes + 1)
        if not contents:
            raise HTTPException(status_code=422, detail="image payload is empty")
        if len(contents) > active.max_image_bytes:
            raise HTTPException(
                status_code=413,
                detail=f"image payload exceeds limit of {active.max_image_bytes} bytes",
            )

        try:
            async with active.limiter.slot():
                return await asyncio.to_thread(active.embed_image, contents)
        except InferenceOverloadedError as exc:
            raise HTTPException(
                status_code=503,
                detail=str(exc),
                headers={"Retry-After": "1"},
            ) from exc
        except InvalidImageError as exc:
            raise HTTPException(status_code=422, detail=str(exc)) from exc
        except Exception as exc:
            raise HTTPException(status_code=500, detail=str(exc)) from exc

    @application.post("/embed/text")
    async def get_text_embedding(request: TextEmbeddingRequest):
        active = get_runtime()
        if (
            active.text_session is None
            or active.tokenizer is None
            or active.text_embedding_output is None
        ):
            raise HTTPException(
                status_code=503,
                detail=f"text embedding is unavailable: {active.text_initialization_error}",
            )

        text = request.text.strip()
        if not text:
            raise HTTPException(status_code=422, detail="text must not be blank")

        try:
            async with active.limiter.slot():
                return await asyncio.to_thread(active.embed_text, text)
        except InferenceOverloadedError as exc:
            raise HTTPException(
                status_code=503,
                detail=str(exc),
                headers={"Retry-After": "1"},
            ) from exc
        except Exception as exc:
            raise HTTPException(status_code=500, detail=str(exc)) from exc

    return application


app = create_app()


if __name__ == "__main__":
    uvicorn.run(app, host="127.0.0.1", port=8000, ws="none")
