import io
import os
from pathlib import Path

import numpy as np
import onnxruntime as ort
import uvicorn
from fastapi import FastAPI, File, HTTPException, UploadFile
from pydantic import BaseModel, Field
from PIL import Image

app = FastAPI(title="Ryzen AI SigLIP 2 Embedding Service")

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


class TextEmbeddingRequest(BaseModel):
    text: str = Field(min_length=1, max_length=2_000)


def build_providers() -> list:
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


def load_session(model_path: Path, providers: list) -> ort.InferenceSession:
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


if not VISION_MODEL_PATH.exists():
    raise FileNotFoundError(
        f"ONNX vision model not found: {VISION_MODEL_PATH}\n"
        "Set VISION_MODEL_PATH to the exported/quantized SigLIP 2 vision model."
    )

providers = build_providers()
vision_session = load_session(VISION_MODEL_PATH, providers)
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

if TEXT_MODEL_PATH.exists() and TOKENIZER_PATH.exists():
    try:
        from transformers import AutoTokenizer

        tokenizer = AutoTokenizer.from_pretrained(
            str(TOKENIZER_PATH), local_files_only=True
        )
        text_session = load_session(TEXT_MODEL_PATH, providers)
        text_output_names = {output.name for output in text_session.get_outputs()}
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
        f"missing {TEXT_MODEL_PATH} or tokenizer directory {TOKENIZER_PATH}; "
        "download the text ONNX model and tokenizer artifacts first"
    )
    print(f"Text embedding is unavailable: {text_initialization_error}")


def preprocess_siglip2(image_bytes: bytes) -> np.ndarray:
    """Build the normalized NCHW tensor expected by SigLIP 2 base patch16-256."""
    image = Image.open(io.BytesIO(image_bytes)).convert("RGB")
    image = image.resize((256, 256), Image.Resampling.BILINEAR)
    image_data = np.asarray(image, dtype=np.float32) / 255.0
    image_data = (image_data - 0.5) / 0.5
    image_data = np.transpose(image_data, (2, 0, 1))
    return np.expand_dims(image_data, axis=0)


def normalize_embedding(raw_output: np.ndarray) -> list[float]:
    raw_vector = np.asarray(raw_output[0], dtype=np.float32).reshape(-1)
    norm = np.linalg.norm(raw_vector)
    if norm == 0:
        raise ValueError("model returned a zero-length embedding")
    return (raw_vector / norm).tolist()


def embedding_response(vector: list[float]) -> dict:
    return {
        "embedding": vector,
        "dimensions": len(vector),
        "model": MODEL_ID,
    }


@app.get("/health")
def health():
    return {
        "status": "ok",
        "model": MODEL_ID,
        "image_embeddings": True,
        "text_embeddings": text_session is not None,
        "text_error": text_initialization_error,
    }


@app.post("/embed/image")
@app.post("/embed", include_in_schema=False)
async def get_image_embedding(file: UploadFile = File(...)):
    try:
        contents = await file.read()
        tensor_data = preprocess_siglip2(contents)
        raw_output = vision_session.run(
            [vision_embedding_output],
            {vision_input_name: tensor_data},
        )[0]
        return embedding_response(normalize_embedding(raw_output))
    except Exception as exc:
        raise HTTPException(status_code=500, detail=str(exc)) from exc


@app.post("/embed/text")
def get_text_embedding(request: TextEmbeddingRequest):
    if text_session is None or tokenizer is None or text_embedding_output is None:
        raise HTTPException(
            status_code=503,
            detail=f"text embedding is unavailable: {text_initialization_error}",
        )

    text = request.text.strip()
    if not text:
        raise HTTPException(status_code=422, detail="text must not be blank")

    try:
        encoded = tokenizer(
            text,
            padding="max_length",
            truncation=True,
            max_length=TEXT_MAX_LENGTH,
            return_tensors="np",
        )
        input_names = {model_input.name for model_input in text_session.get_inputs()}
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

        raw_output = text_session.run([text_embedding_output], model_inputs)[0]
        return embedding_response(normalize_embedding(raw_output))
    except Exception as exc:
        raise HTTPException(status_code=500, detail=str(exc)) from exc


if __name__ == "__main__":
    uvicorn.run(app, host="127.0.0.1", port=8000, ws="none")
