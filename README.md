# Local Visual Semantic Search

This project is a fully local semantic search engine for visual media, focused on images and video frames. The goal is to generate searchable embeddings on-device without depending on cloud APIs, avoiding network latency, external service cost, and remote data exposure.

The intended system has two core capabilities:

- Multimodal representation: use a custom-quantized SigLIP 2 INT8 ONNX model to project visual media, and eventually text queries, into a shared vector space. That shared space enables natural-language search over local images and frames, such as `sunset over a mountain ridge` or `red running shoes`.
- Local acceleration path: run embedding inference through AMD Ryzen AI when `VitisAIExecutionProvider` is available, with CPU fallback for development and environments where the NPU runtime is not active.

Current status: the repository contains a FastAPI embedding service for images, a Go test client, and manual test commands. Text-query embedding and full media indexing/search orchestration are intended next layers on top of this baseline.

## Setup

Test installation:

```
conda activate ryzen-ai-1.7.1
```


```
source /opt/AMD/ryzenai/venv/bin/activate.fish
```

## Test the FastAPI embedding service

Start the service:

```bash
python embed_service.py
```

Send an image to the `/embed` endpoint:

```bash
curl -X POST \
  -F "file=@/home/alexis/Pictures/Screenshots/solutionpatterns.png" \
  http://127.0.0.1:8000/embed
```

Pretty-print the full embedding response:

```bash
curl -s -X POST \
  -F "file=@/home/alexis/Pictures/Screenshots/solutionpatterns.png" \
  http://127.0.0.1:8000/embed \
  | python -m json.tool
```

Print only the embedding vector:

```bash
curl -s -X POST \
  -F "file=@/home/alexis/Pictures/Screenshots/solutionpatterns.png" \
  http://127.0.0.1:8000/embed \
  | python -c "import sys,json; print(json.load(sys.stdin)['embedding'])"
```

Print one value per line with indexes:

```bash
curl -s -X POST \
  -F "file=@/home/alexis/Pictures/Screenshots/solutionpatterns.png" \
  http://127.0.0.1:8000/embed \
  | python -c "import sys,json; [print(i, v) for i,v in enumerate(json.load(sys.stdin)['embedding'])]"
```

Check the embedding dimensionality:

```bash
curl -s -X POST \
  -F "file=@/home/alexis/Pictures/Screenshots/solutionpatterns.png" \
  http://127.0.0.1:8000/embed \
  | python -c "import sys,json; print(len(json.load(sys.stdin)['embedding']))"
```

FastAPI also exposes interactive docs at:

```text
http://127.0.0.1:8000/docs
```


## Test with the Go client

Keep the FastAPI service running in one terminal:

```bash
python embed_service.py
```

Then run the Go client from another terminal with an image path:

```bash
go run client.go /home/alexis/Pictures/Screenshots/solutionpatterns.png
```

Expected output includes the request duration, embedding dimensionality, and the first few vector values:

```text
Generating embedding for /home/alexis/Pictures/Screenshots/solutionpatterns.png...
Successfully generated vector in 123ms!
Vector Dimensions: 768
First 5 Dimensions: [0.0123 -0.0456 0.0789 0.0012 -0.0345]
```

## Architecture: Why Python + Go?

This project uses a hybrid dual-runtime architecture split between Go and Python. Go is the intended orchestration layer, while Python owns the ONNX/Ryzen AI execution path and can fall back to CPU execution when the VitisAI provider is not available.

```text
┌────────────────────────┐         HTTP / JSON         ┌────────────────────────┐
│      Go Backend        │ ──────────────────────────> │   Python AI Service    │
│  (Orchestrator, DB,    │ <────────────────────────── │  (FastAPI, ONNX, ORT)  │
│   File Processing)     │       Embeddings Output     └────────────────────────┘
└────────────────────────┘                                         │
                                                                   ▼
                                                       ┌────────────────────────┐
                                                       │ Optional Ryzen AI NPU  │
                                                       │ via VitisAI provider   │
                                                       └────────────────────────┘
```

This split keeps the hardware/runtime-specific pieces isolated while leaving the application workflow in Go.

1. The Python Layer: ONNX and Ryzen AI Runtime

   The constraint: AMD's Ryzen AI software stack, the VitisAI Execution Provider, and the supporting runtime pieces are packaged primarily for Python/C++ workflows.

   The solution: Instead of binding Go directly to native NPU/runtime libraries with cgo, the project isolates model loading and inference inside a small FastAPI service. That service uses `VitisAIExecutionProvider` when available and intentionally falls back to `CPUExecutionProvider` otherwise.

   Responsibilities: load the INT8 ONNX model, initialize ONNX Runtime with `vaip_config.json` when applicable, select the pooled image embedding output, normalize the vector, and expose a single HTTP endpoint for embeddings.

2. The Go Layer: Orchestration and Integration

   The advantage: Go is a good fit for file walking, concurrency, database writes, request handling, and keeping the broader pipeline simple to deploy.

   Current role: `client.go` is a small test client that sends an image to the local FastAPI service and decodes the returned 768-dimensional embedding.

   Intended role: a larger Go process can scan files, schedule embedding work, call the Python service over local HTTP, and store or compare vectors downstream.

Key benefits:

- Clean isolation: Ryzen AI, ONNX Runtime, and model-specific dependencies stay inside the Python environment instead of leaking into the Go process.
- Practical fallback: the same API can run on CPU while the NPU provider/runtime is unavailable, which keeps development and testing unblocked.
- Clear data contract: Go sends image bytes as multipart form data and receives a normalized flat embedding vector suitable for cosine similarity.
- Future acceleration path: when `VitisAIExecutionProvider` is correctly installed and detected, the Python service can use AMD's Ryzen AI runtime without changing the Go-side API.
