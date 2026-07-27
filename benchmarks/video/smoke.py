#!/usr/bin/env python3
"""Run a bounded local media-to-SigLIP2 operational smoke benchmark.

This is intentionally not a retrieval-quality evaluator. It probes one source,
extracts periodic frames, embeds them through the running local service, and
emits reproducible JSON metrics for Phase 0. Retrieval Recall@K additionally
requires a manually annotated corpus manifest.
"""

from __future__ import annotations

import argparse
import json
import shutil
import subprocess
import tempfile
import time
import urllib.request
import uuid
from pathlib import Path
from typing import Any


def run_probe(source: Path) -> dict[str, Any]:
    command = [
        "ffprobe",
        "-v",
        "error",
        "-select_streams",
        "v:0",
        "-show_entries",
        "format=duration,format_name,size:stream=codec_name,width,height,avg_frame_rate",
        "-of",
        "json",
        str(source),
    ]
    result = subprocess.run(command, check=True, capture_output=True, text=True)
    probe = json.loads(result.stdout)
    if not probe.get("streams"):
        raise ValueError(f"no video stream found in {source}")

    return {
        "container": probe["format"]["format_name"],
        "duration_seconds": float(probe["format"]["duration"]),
        "size_bytes": int(probe["format"]["size"]),
        "video": probe["streams"][0],
    }


def post_multipart(endpoint: str, image: Path) -> dict[str, Any]:
    boundary = uuid.uuid4().hex
    image_bytes = image.read_bytes()
    body = b"".join(
        [
            f"--{boundary}\r\n".encode(),
            (
                "Content-Disposition: form-data; "
                f'name="file"; filename="{image.name}"\r\n'
            ).encode(),
            b"Content-Type: image/jpeg\r\n\r\n",
            image_bytes,
            f"\r\n--{boundary}--\r\n".encode(),
        ]
    )
    request = urllib.request.Request(
        f"{endpoint.rstrip('/')}/embed/image",
        data=body,
        method="POST",
        headers={"Content-Type": f"multipart/form-data; boundary={boundary}"},
    )
    with urllib.request.urlopen(request, timeout=120) as response:
        return json.load(response)


def post_json(endpoint: str, payload: dict[str, str]) -> dict[str, Any]:
    request = urllib.request.Request(
        f"{endpoint.rstrip('/')}/embed/text",
        data=json.dumps(payload).encode(),
        method="POST",
        headers={"Content-Type": "application/json"},
    )
    with urllib.request.urlopen(request, timeout=120) as response:
        return json.load(response)


def directory_size(path: Path) -> int:
    return sum(item.stat().st_size for item in path.rglob("*") if item.is_file())


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("source", type=Path, help="local source video")
    parser.add_argument(
        "--interval-seconds",
        type=float,
        default=1.0,
        help="periodic sampling interval; must be greater than zero (default: 1)",
    )
    parser.add_argument(
        "--endpoint",
        default="http://127.0.0.1:8000",
        help="local embedding service base URL",
    )
    parser.add_argument(
        "--query",
        help="optional text query for a frame-score smoke check",
    )
    parser.add_argument(
        "--output",
        type=Path,
        help="optional JSON result path; stdout remains machine-readable JSON",
    )
    return parser.parse_args()


def main() -> None:
    args = parse_args()
    if args.interval_seconds <= 0:
        raise SystemExit("--interval-seconds must be greater than zero")
    source = args.source.resolve()
    if not source.is_file():
        raise SystemExit(f"source video does not exist: {source}")

    probe = run_probe(source)
    work_directory = Path(tempfile.mkdtemp(prefix="semantic-search-video-smoke-"))
    try:
        frames_directory = work_directory / "frames"
        frames_directory.mkdir()
        extraction_start = time.monotonic()
        subprocess.run(
            [
                "ffmpeg",
                "-hide_banner",
                "-loglevel",
                "error",
                "-nostdin",
                "-i",
                str(source),
                "-vf",
                f"fps=1/{args.interval_seconds}",
                "-q:v",
                "2",
                str(frames_directory / "frame-%06d.jpg"),
            ],
            check=True,
            timeout=180,
        )
        extraction_seconds = time.monotonic() - extraction_start
        frames = sorted(frames_directory.glob("*.jpg"))
        if not frames:
            raise RuntimeError("ffmpeg extracted no frames")

        embedding_start = time.monotonic()
        frame_embeddings = [post_multipart(args.endpoint, frame) for frame in frames]
        embedding_seconds = time.monotonic() - embedding_start
        dimensions = {response.get("dimensions") for response in frame_embeddings}
        models: set[str] = {str(response["model"]) for response in frame_embeddings}
        if dimensions != {768}:
            raise RuntimeError(f"unexpected image embedding dimensions: {dimensions}")

        result: dict[str, Any] = {
            "source": str(source),
            "source_media": probe,
            "sampling_policy": f"fps=1/{args.interval_seconds}",
            "sample_interval_seconds": args.interval_seconds,
            "frames_extracted": len(frames),
            "temporary_frame_bytes": directory_size(frames_directory),
            "extraction_seconds": extraction_seconds,
            "extraction_frames_per_second": len(frames) / extraction_seconds,
            "embedding_seconds": embedding_seconds,
            "embedding_frames_per_second": len(frames) / embedding_seconds,
            "embedding_dimensions": 768,
            "embedding_models": sorted(models),
        }
        if args.query:
            query_embedding = post_json(args.endpoint, {"text": args.query})["embedding"]
            scores = [
                sum(query_value * frame_value for query_value, frame_value in zip(query_embedding, response["embedding"]))
                for response in frame_embeddings
            ]
            best_index = max(range(len(scores)), key=scores.__getitem__)
            result["query_smoke_check"] = {
                "query": args.query,
                "best_frame_index_zero_based": best_index,
                "best_frame_approximate_timestamp_seconds": best_index * args.interval_seconds,
                "best_cosine_similarity": scores[best_index],
                "minimum_cosine_similarity": min(scores),
                "maximum_cosine_similarity": max(scores),
            }

        rendered = json.dumps(result, sort_keys=True)
        print(rendered)
        if args.output:
            args.output.parent.mkdir(parents=True, exist_ok=True)
            args.output.write_text(rendered + "\n")
    finally:
        shutil.rmtree(work_directory, ignore_errors=True)


if __name__ == "__main__":
    main()
