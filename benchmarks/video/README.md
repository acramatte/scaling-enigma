# Video-ingestion Phase 0 benchmark

This benchmark chooses the video sampling policy and operating limits before
video ingestion is implemented. Its product question is:

```text
For a query describing a visible feature, does a video containing that feature
appear among the top K distinct videos, at the correct approximate time?
```

It does not evaluate temporal-action understanding, speech, OCR, or captions.
Those are separate retrieval channels and must not be mixed into these results.

## Corpus

Keep source videos outside the repository. They may contain large or licensed
media. Create a local `manifest.yaml` from
[`manifest.example.yaml`](manifest.example.yaml). Phase 0 may use an absolute
local `source_path`; the end-to-end RustFS run must additionally record the
exact S3 object version/ETag. Retain the media until the experiment is
reproducible.

The first corpus should include at least:

- short clips and long recordings;
- static-camera video where an object enters without a scene cut;
- edited video with sharp and gradual transitions;
- features visible for at least one second;
- indoor, outdoor, low-light, motion-blurred, and text-heavy frames;
- at least one repeated/near-duplicate visual scene.

Every query must identify its expected video and visible time interval. Do not
label only a video; temporal annotations let us measure whether sampling missed
the feature or retrieval ranked it poorly.

## Configurations

Run a fixed-rate baseline before adding heuristics:

```text
0.5 seconds per frame
1 second per frame
2 seconds per frame
5 seconds per frame
```

Use the same videos, model artifact, preprocessing pipeline, search queries,
and candidate limits for every run. Record all limits: maximum object size,
duration, frame count, temporary disk, decoder timeout, worker count, and
inference concurrency.

A later experiment may compare a hybrid policy:

```text
fixed periodic baseline
+ scene-transition samples
- conservative near-duplicate samples
```

Do not make scene detection the only source of frames. It can miss a feature
appearing inside an otherwise static shot.

## Metrics

### Video Recall@K

For each query, return K distinct videos after grouping segments by document.

```text
Recall@K = relevant videos in the returned K / all relevant videos
```

When a query has one expected video, this is the proportion of queries whose
expected video appears in the top K.

Measure at least Recall@1, Recall@5, and Recall@10.

### Sampling recall

```text
sampling recall = labeled visible intervals containing at least one retained frame
                  / all labeled visible intervals
```

This isolates extraction policy from model quality.

### Temporal hit rate

For every returned relevant video, check whether the selected segment overlaps
its labeled feature interval or falls within a documented timestamp tolerance.

### Operational metrics

For each source video and run record:

- ffprobe duration, dimensions, stream codec, and container;
- retained frame count and effective interval;
- extraction duration and frames per second;
- embedding duration and frames per second;
- total ingestion latency and queue wait;
- peak temporary disk use;
- vector count and database storage growth;
- retries/errors and their classification;
- ONNX Runtime provider, model identifier, model artifact revision, and
  ffmpeg/ffprobe versions.

## Procedure

1. Smoke-test the exact ffmpeg/ffprobe binaries with a local sample before any
   full run.
2. Start the existing embedder and record its `/health` response and ONNX
   provider.
3. Run one short video under the 1-second baseline to verify timing and logs.
4. Run every corpus item under each fixed-rate configuration.
5. Search every annotated query and capture ranked distinct-video results plus
   returned timestamps.
6. Compute recall, sampling recall, temporal hit rate, and operational metrics.
7. Pick the policy only after comparing quality against resource cost.

## Acceptance decision

The selected MVP policy must have a documented result for all of these:

- minimum acceptable Video Recall@5 and Recall@10;
- minimum acceptable sampling recall for visible features lasting at least one
  second;
- maximum acceptable indexing delay per minute of source video;
- maximum temporary disk use and vector growth per video;
- maximum queue delay for image uploads while videos are processing;
- maximum supported duration/object size/frame count.

The actual numerical thresholds are product decisions. Do not silently change
the interval for a long video before those limits are approved; reject the job
with a durable reason or record an explicitly approved adaptive policy.

## Current Phase 0 evidence

Checked on the host on 2026-07-23:

```text
ffmpeg/ffprobe: 8.0.1-3ubuntu2
embedder: google/siglip2-base-patch16-256, image/text endpoints healthy
UCF101: 13,320 AVI clips, 101 classes, 7,193,326,396 bytes
local corpus root: /home/alexis/Development/private/ML/ucf101/UCF-101
```

The selected local manifest is
`/home/alexis/Development/private/ML/ucf101/benchmark/manifest.yaml`. It has
eight development and eight held-out clips from distinct UCF101 classes. The
development clips have preliminary whole-clip annotations from 1 fps contact
sheets; held-out annotations remain pending. UCF101 class names are weak action
labels only, so this corpus cannot report final Recall@K, sampling recall, or
temporal-hit metrics until the held-out visible-feature intervals are manually
reviewed.
The reproducible operational smoke command is:

```bash
python3 benchmarks/video/smoke.py \
  /home/alexis/Development/private/ML/ucf101/UCF-101/SoccerJuggling/v_SoccerJuggling_g01_c01.avi \
  --interval-seconds 1 \
  --query 'a person juggling a soccer ball'
```

Observed operational evidence, sequential requests to the existing local
embedder. The eight development clips total 78.988 source seconds:

The complete run record and its limitations are in
[`results/README.md`](results/README.md).

```text
interval     frames     extraction fps     embedding fps     temporary JPEG bytes
0.5 seconds     157            305.51            14.72                 1,870,880
1 second         80            155.86            13.83                   952,294
2 seconds        40             77.83            14.04                   481,966
5 seconds        16             33.22            16.17                   193,216
```

The temporary-JPEG column is the sum of per-video temporary frame files across
the sequential run, not a concurrent-worker peak. On the 30.03-second
SoccerJuggling smoke clip, the 0.5/1/2/5-second intervals emitted 60/30/15/6
frames respectively. This validates probe, extraction, embedding, and bounded
temporary-frame cleanup; it is not retrieval-quality evidence.
