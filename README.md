# Local Visual Semantic Search

This project is a fully local semantic search engine for visual media, focused on images and video frames. The goal is to generate searchable embeddings on-device without depending on cloud APIs, avoiding network latency, external service cost, and remote data exposure.

The intended system has two core capabilities:

- Multimodal representation: use the image and text towers from `google/siglip2-base-patch16-256` to project visual media and text queries into a shared vector space. That shared space enables natural-language search over local images and frames, such as `sunset over a mountain ridge` or `red running shoes`.
- Local acceleration path: run embedding inference through AMD Ryzen AI when `VitisAIExecutionProvider` is available, with CPU fallback for development and environments where the NPU runtime is not active.

Current status: the FastAPI service embeds images and text, the Go client indexes images, and pgvector ranks indexed images or video segments for a natural-language query.

## Setup

Test installation:

```
conda activate ryzen-ai-1.7.1
```


```
source /opt/AMD/ryzenai/venv/bin/activate.fish
```

## PostgreSQL and pgvector

The local database runs PostgreSQL 18 (the current stable major release) with
pgvector preinstalled. PostgreSQL 19 is still a beta release as of July 2026.

Install Goose, start PostgreSQL, and apply the schema:

```bash
make tools
make db-up
make migrate-up
```

Open a `psql` shell inside the container:

```bash
make db-psql
```

The Go application uses GORM and reads `DATABASE_URL`. When it is unset, it
defaults to the credentials in `compose.yaml`. Copy `.env.example` if you want
to customize the Compose or application settings:

```bash
cp .env.example .env
export DATABASE_URL='postgres://semantic_search:semantic_search@localhost:5432/semantic_search?sslmode=disable'
```

The first migration enables pgvector and creates a split schema: `documents`
stores `source_uri` plus JSONB metadata, and `embeddings` stores the
`vector(768)` rows linked back to documents. The vector size matches the current
SigLIP embedding output. The second migration renames embeddings saved with the
old vision-only model label so image and text queries consistently identify the
shared `google/siglip2-base-patch16-256` checkpoint.

## Test the FastAPI embedding service

Start the service:

```bash
python embed_service.py
```

Send an image to the `/embed/image` endpoint:

```bash
curl -X POST \
  -F "file=@/home/alexis/Pictures/Screenshots/solutionpatterns.png" \
  http://127.0.0.1:8000/embed/image
```

Pretty-print the full embedding response:

```bash
curl -s -X POST \
  -F "file=@/home/alexis/Pictures/Screenshots/solutionpatterns.png" \
  http://127.0.0.1:8000/embed/image \
  | python -m json.tool
```

Print only the embedding vector:

```bash
curl -s -X POST \
  -F "file=@/home/alexis/Pictures/Screenshots/solutionpatterns.png" \
  http://127.0.0.1:8000/embed/image \
  | python -c "import sys,json; print(json.load(sys.stdin)['embedding'])"
```

Print one value per line with indexes:

```bash
curl -s -X POST \
  -F "file=@/home/alexis/Pictures/Screenshots/solutionpatterns.png" \
  http://127.0.0.1:8000/embed/image \
  | python -c "import sys,json; [print(i, v) for i,v in enumerate(json.load(sys.stdin)['embedding'])]"
```

Check the embedding dimensionality:

```bash
curl -s -X POST \
  -F "file=@/home/alexis/Pictures/Screenshots/solutionpatterns.png" \
  http://127.0.0.1:8000/embed/image \
  | python -c "import sys,json; print(len(json.load(sys.stdin)['embedding']))"
```

FastAPI also exposes interactive docs at:

```text
http://127.0.0.1:8000/docs
```

## Install and test the text encoder

The existing vision ONNX model only accepts images. Text search also requires
the text encoder and tokenizer from the same SigLIP 2 checkpoint so text-query
vectors can be compared with stored image vectors.

Download `onnx/text_model_int8.onnx` and the tokenizer files from
[`onnx-community/siglip2-base-patch16-256-ONNX`](https://huggingface.co/onnx-community/siglip2-base-patch16-256-ONNX/tree/main),
then arrange them as follows:

```text
../models/
├── vision_model_int8.onnx
├── text_model_int8.onnx
└── tokenizer/
    ├── tokenizer.json
    ├── tokenizer.model
    ├── tokenizer_config.json
    └── special_tokens_map.json
```

These are runtime artifacts; the full PyTorch checkpoint and the FP32 text
model are not required. Restart `embed_service.py` after installing them.

Check that both embedding modes are ready:

```bash
curl -s http://127.0.0.1:8000/health | python -m json.tool
```

Embed a text query directly:

```bash
curl -s -X POST \
  -H 'Content-Type: application/json' \
  -d '{"text":"sunset over a mountain ridge"}' \
  http://127.0.0.1:8000/embed/text \
  | python -m json.tool
```

## Test with the Go client

Keep the FastAPI service running in one terminal:

```bash
python embed_service.py
```

Apply any pending migrations, then index an image from another terminal:

```bash
make migrate-up
go run client.go index /home/alexis/Pictures/Screenshots/solutionpatterns.png
```

Expected output includes the request duration, embedding dimensionality, and the first few vector values:

```text
Generating embedding for /home/alexis/Pictures/Screenshots/solutionpatterns.png...
Generated vector in 123ms
Vector dimensions: 768
First 5 dimensions: [0.0123 -0.0456 0.0789 0.0012 -0.0345]
```

Search the indexed embeddings with natural language:

```bash
go run client.go search sunset over a mountain ridge
```

The Go client asks the Python service for the normalized text embedding, then
orders matching database rows with pgvector's cosine-distance operator. Results
include the source URI and, for video frames, the segment timing.

## Web search

With PostgreSQL and the FastAPI embedding service running, start the Go web
server:

```bash
go run client.go serve
```

Open [http://127.0.0.1:8080](http://127.0.0.1:8080), enter a natural-language
description, and the page will display the closest indexed documents. The
server uses only Go's standard `net/http` and `html/template` packages for the
web layer; it reuses the same text-embedding and pgvector search path as the
CLI. Set `HTTP_ADDR` to change the listening address.

## Database integration test

Run the search integration test in a disposable pgvector container:

```bash
make test-integration
```

Testcontainers starts the same PostgreSQL/pgvector image used by Compose on a
random host port, and Goose applies the embedded project migrations. The test
writes its fixtures inside a transaction that is rolled back, then removes the
container. It verifies cosine ordering, model and media filters, limits, JSONB
metadata, and video segment timing without touching the development database.

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

   Responsibilities: load the vision and text ONNX models, initialize ONNX Runtime with `vaip_config.json` when applicable, tokenize text, normalize both embedding types, and expose image and text embedding endpoints.

2. The Go Layer: Orchestration and Integration

   The advantage: Go is a good fit for file walking, concurrency, database writes, request handling, and keeping the broader pipeline simple to deploy.

   Current role: `client.go` indexes image embeddings and performs natural-language searches over the stored 768-dimensional vectors.

   Intended role: a larger Go process can scan files, schedule embedding work, call the Python service over local HTTP, and store or compare vectors downstream.

Key benefits:

- Clean isolation: Ryzen AI, ONNX Runtime, and model-specific dependencies stay inside the Python environment instead of leaking into the Go process.
- Practical fallback: the same API can run on CPU while the NPU provider/runtime is unavailable, which keeps development and testing unblocked.
- Clear data contract: Go sends image bytes as multipart form data and receives a normalized flat embedding vector suitable for cosine similarity.
- Future acceleration path: when `VitisAIExecutionProvider` is correctly installed and detected, the Python service can use AMD's Ryzen AI runtime without changing the Go-side API.
