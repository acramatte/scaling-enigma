# Video semantic-ingestion plan

This document is the implementation plan for making RustFS-uploaded videos
searchable by their visible content. It lives beside the Go ingestion service
because the durable queue, object lifecycle, ffmpeg orchestration, and index
publication rules belong to that boundary.

The product promise for the first version is deliberately narrow:

```text
If a visible feature occurs in a retained video frame, a natural-language query
for that feature can return the parent video once, at the best matching time.
```

This is visual-feature retrieval. Frame-level SigLIP2 embeddings do not provide
reliable temporal-action understanding such as "a person enters and closes the
door", nor do they index speech or on-screen text beyond what an image encoder
can observe.

## Current boundary

```text
S3 upload
  -> RustFS ObjectCreated webhook
  -> PostgreSQL ingestion_jobs
  -> Go ingestion worker
  -> RustFS object download
  -> Python SigLIP2 image embedding
  -> documents + pgvector embeddings
```

`documents.media_type`, `embeddings.segment_index`, `embeddings.start_ms`, and
`embeddings.end_ms` already model a video and its timestamped segments. The
current worker intentionally marks videos as ignored.

## Target boundary

```text
S3 video upload
  -> RustFS ObjectCreated webhook
  -> one durable ingestion job for the exact source object version
  -> Go video worker
      -> bounded seekable temporary source file
      -> ffprobe validation
      -> one ffmpeg sampling pass
      -> Python SigLIP2 embedding per retained frame
  -> one fenced PostgreSQL transaction publishes the complete segment set
  -> search groups segments by video and returns the strongest matching time
  -> signed, Range-aware S3 video proxy plays the matching moment
```

## Non-negotiable invariants

- The durable work unit is one source object version, not one job per frame.
- Object version/ETag identity is retained from notification through fetch,
  indexing, and playback.
- Sampling policy, effective interval, model, and pipeline version are frozen
  for a claimed indexing run so retries are deterministic.
- Video processing is bounded by object size, duration, frame count, temporary
  disk, decoder wall-clock time, worker count, and inference concurrency.
- Long-running jobs use leases, heartbeats, expired-work recovery, and fencing.
  A stale worker cannot publish or complete after another worker owns the job.
- A video index generation is published atomically. Search sees either the
  previous complete set or the new complete set, never partial or mixed frames.
- Reindexing removes obsolete segments.
- A video ranks by its best matching segment, not an average of all frames.
- The web server never invokes ffmpeg while serving a search result.

## Phase 0: benchmark and operating limits

Before choosing a permanent sampling policy, build and run the benchmark
specified in [`../../benchmarks/video/README.md`](../../benchmarks/video/README.md).

The benchmark must record:

- model identifier and model artifact revision;
- ffmpeg/ffprobe versions and command line;
- hardware/provider used by ONNX Runtime;
- sampling policy and effective interval;
- ingestion latency, decoder throughput, embedding throughput, queue delay,
  temporary disk peak, and vector count;
- video Recall@K, sampling recall, and temporal hit rate.

The committed manifest schema is at
[`../../benchmarks/video/manifest.example.yaml`](../../benchmarks/video/manifest.example.yaml).
Real video binaries and completed manifests are intentionally local artifacts,
not repository fixtures.

Initial policy hypothesis to test:

```text
fixed 1 fps baseline
maximum frame count enforced explicitly
one video worker
one image worker
one shared inference slot
```

Initial product constraints are intentionally loose: indexing may take minutes,
there is no requested upload-size or duration limit, and sub-second visible
features are out of scope. The implementation must still impose explicit
resource bounds; Phase 0 measurements will inform those operating limits rather
than treating unbounded input as supported behavior.

Do not infer the final interval from intuition. Compare at least 0.5, 1, 2, and
5 seconds per frame against labeled visible-feature intervals. The repeatable
operational smoke harness is
[`../../benchmarks/video/smoke.py`](../../benchmarks/video/smoke.py); it does
not replace the manually annotated retrieval-quality evaluation.

## Phase 1: queue hardening

1. Classify jobs as image, video, or unsupported when they are enqueued, then
   verify the classification while processing.
2. Drain ready jobs until the queue is empty; wait only while it is empty.
3. Reserve independent image and video worker capacity so long videos do not
   cause image head-of-line blocking.
4. Add lease token, expiry, heartbeat, expired-job reclaim, and fenced writes.
5. Put the shared configurable inference semaphore in the Python service, which
   owns the ONNX sessions and accelerator.
6. Separate retryable transport/inference errors from permanent media and limit
   failures.

## Phase 2: bounded video extraction

1. Download the exact source object version to a private temporary directory.
2. Run `ffprobe` and validate video-stream presence, duration, dimensions,
   rotation, codec/container, object size, and computed sample count.
3. Compute deterministic sample timestamps and segment indexes.
4. Run one ffmpeg process per source video; do not seek and launch ffmpeg once
   for every sample.
5. Extract only retained frames, scaled consistently with the current SigLIP2
   preprocessing path. Do not pre-transcode the entire video just to sample it.
6. Embed frames with the existing image encoder, with at most one in-flight
   request per video initially.
7. Record duration, dimensions, codec, effective policy, retained-frame count,
   pipeline version, and per-frame timestamp metadata.
8. Terminate the whole decoder process group on cancellation or lease loss and
   remove temporary data in every exit path.

## Phase 3: atomic publication

Add a batch-oriented database operation that:

1. verifies the current lease token;
2. upserts the stable video document;
3. replaces all segment embeddings for that document/model;
4. removes segments absent from the new generation;
5. records object identity and pipeline metadata;
6. marks the job complete in the same fenced transaction.

The current one-embedding-at-a-time write path remains appropriate for images,
but must not be the durable publication boundary for videos.

## Phase 4: retrieval and playback

Change the vector query to choose the best segment per document before applying
the final result limit. A repetitive video must not consume all raw frame
candidates and hide other matching videos.

Return the best `segment_index`, `start_ms`, and `end_ms` with each video. Add a
signed S3 media proxy that supports HTTP Range requests so the browser can seek
to the matched moment. Browser codec compatibility and derived posters/clips are
separate follow-up work.

## Phase 5: quality and scale follow-ups

Only after Phase 0/2 measurements:

- hybrid periodic plus scene-boundary sampling;
- conservative near-duplicate suppression;
- embedding batch endpoint if profiling shows HTTP overhead matters;
- hardware decode if decoding is proven to be the bottleneck;
- HNSW after representative vector corpus measurements;
- OCR, captions, speech transcript retrieval, or a separate temporal video
  model when benchmark failures show that frame sampling is insufficient.

## Explicitly deferred alternatives

- Scene-only/keyframe-only sampling: unsafe as the sole policy because a
  feature can appear within a static shot without a cut.
- Codec I-frames as semantic samples: I-frames serve compression, not semantic
  coverage.
- Mean-pooling all frames into one vector: dilutes brief features.
- Per-frame durable queue jobs: requires durable derivative-frame artifacts and
  parent/child completion semantics; defer until distributed frame processing is
  needed.
- Native video-text models: appropriate for motion/order queries, but require a
  different model/export/evaluation path and should not replace frame retrieval
  without evidence.
