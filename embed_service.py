import os
import io
import numpy as np
from PIL import Image
from fastapi import FastAPI, UploadFile, File, HTTPException
import onnxruntime as ort
import uvicorn

app = FastAPI(title="Ryzen AI SigLIP 2 Embedding Service")

BASE_DIR = os.path.dirname(os.path.abspath(__file__))

# Paths can be overridden when running from another directory:
#   MODEL_PATH=/path/to/model.onnx VAIP_CONFIG_PATH=/path/to/vaip_config.json python embed_service.py
VAIP_CONFIG_PATH = os.environ.get("VAIP_CONFIG_PATH", os.path.join(BASE_DIR, "vaip_config.json"))
MODEL_PATH = os.environ.get("MODEL_PATH", os.path.abspath(os.path.join(BASE_DIR, "..", "models", "vision_model_int8.onnx")))


def build_providers() -> list:
    available_providers = ort.get_available_providers()
    providers = []

    if "VitisAIExecutionProvider" in available_providers:
        providers.append(("VitisAIExecutionProvider", {"config_file": VAIP_CONFIG_PATH}))
    else:
        print(
            "VitisAIExecutionProvider is not available in this Python environment. "
            f"Available providers: {', '.join(available_providers)}"
        )

    providers.append("CPUExecutionProvider")
    return providers


if not os.path.exists(MODEL_PATH):
    raise FileNotFoundError(
        f"ONNX model not found: {MODEL_PATH}\n"
        "Set MODEL_PATH to the exported/quantized SigLIP vision ONNX file, or place "
        "'vision_model_int8.onnx' next to embed_service.py."
    )

providers = build_providers()

print(f"Loading ONNX Model: {MODEL_PATH}")
try:
    session = ort.InferenceSession(MODEL_PATH, providers=providers)
    print("Model successfully loaded! Active Providers:", session.get_providers())
except Exception as e:
    if providers == ["CPUExecutionProvider"]:
        raise

    print(f"NPU initialization failed, falling back to CPU. Error: {e}")
    session = ort.InferenceSession(MODEL_PATH, providers=["CPUExecutionProvider"])

INPUT_NAME = session.get_inputs()[0].name
OUTPUT_NAMES = {output.name for output in session.get_outputs()}
EMBEDDING_OUTPUT_NAME = "pooler_output" if "pooler_output" in OUTPUT_NAMES else session.get_outputs()[-1].name

def preprocess_siglip2(image_bytes: bytes) -> np.ndarray:
    """Standard image pipeline matching SigLIP 2 expectations"""
    # 1. Load image and convert to RGB
    img = Image.open(io.BytesIO(image_bytes)).convert("RGB")

    # 2. SigLIP 2 base-256 expects exactly 256x256 pixels
    img = img.resize((256, 256), Image.Resampling.BILINEAR)

    # 3. Scale pixel values to [0, 1]
    img_data = np.array(img).astype(np.float32) / 255.0

    # 4. Normalize with SigLIP's standard mean and std dev (0.5, 0.5)
    mean = np.array([0.5, 0.5, 0.5], dtype=np.float32)
    std = np.array([0.5, 0.5, 0.5], dtype=np.float32)
    img_data = (img_data - mean) / std

    # 5. Convert HWC (Height, Width, Channel) to planar NCHW
    img_data = np.transpose(img_data, (2, 0, 1))
    img_data = np.expand_dims(img_data, axis=0) # Batch size of 1

    return img_data

@app.post("/embed")
async def get_embedding(file: UploadFile = File(...)):
    try:
        contents = await file.read()
        tensor_data = preprocess_siglip2(contents)

        # Run inference and use the pooled image embedding, not per-patch hidden states.
        raw_outputs = session.run([EMBEDDING_OUTPUT_NAME], {INPUT_NAME: tensor_data})
        raw_vector = np.asarray(raw_outputs[0][0], dtype=np.float32).reshape(-1)

        # L2 Normalize the vector so your Vector DB can run fast Cosine Similarity searches
        norm = np.linalg.norm(raw_vector)
        normalized_vector = (raw_vector / norm).tolist() if norm > 0 else raw_vector.tolist()

        return {"embedding": normalized_vector}

    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))

if __name__ == "__main__":
    # Run server locally on port 8000
    uvicorn.run(app, host="127.0.0.1", port=8000, ws="none")
