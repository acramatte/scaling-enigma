# Phase 0 operational benchmark results

**Run date:** 2026-07-23

This records the first benchmark for planned video ingestion. It
measures local media probing, periodic ffmpeg frame extraction, and sequential
calls to the existing SigLIP2 image embedding endpoint. It is not yet a
retrieval-quality evaluation.

## Environment

```text
Host media tools: ffmpeg 8.0.1-3ubuntu2, ffprobe 8.0.1-3ubuntu2
Embedding service: google/siglip2-base-patch16-256
Embedding endpoint: http://127.0.0.1:8000/embed/image
Embedding dimensions: 768
Request mode: sequential, one frame request at a time
```

The benchmark used the extracted original UCF101 archive outside the repository:

```text
[UCF101 - Action Recognition Data Set](https://www.crcv.ucf.edu/data/UCF101.php)
```

The full archive contains 13,320 AVI clips across 101 classes, totalling
7,193,326,396 bytes. The selected development corpus comprises one clip from
each of these classes:

```text
ApplyEyeMakeup
Basketball
Biking
PlayingGuitar
Skiing
HorseRiding
SoccerJuggling
Typing
```

Together, the eight development clips contain 78.988 seconds of source video.
The local selection and preliminary annotations are in:

```text
ucf101/benchmark/manifest.yaml
```

## Method

Each source was processed with the committed harness:

```bash
python3 benchmarks/video/smoke.py <local-source-video> \
  --interval-seconds <0.5|1|2|5> \
  --output <local-result.json>
```

For each interval, the harness:

1. probes the source with `ffprobe`;
2. extracts JPEG frames with one `ffmpeg` process and `fps=1/<interval>`;
3. posts every retained frame to the running `/embed/image` endpoint;
4. records frame count, temporary JPEG bytes, extraction duration, embedding
duration, model, and vector dimensionality;
5. deletes the per-run temporary directory.

## Results

```text
interval     frames     extraction fps     embedding fps     temporary JPEG bytes
0.5 seconds     157            305.51            14.72                 1,870,880
1 second         80            155.86            13.83                   952,294
2 seconds        40             77.83            14.04                   481,966
5 seconds        16             33.22            16.17                   193,216
```

The temporary-JPEG value is the sum of temporary-frame bytes from the eight
sequential runs. It is not a concurrent-worker disk peak.

A focused 30.03-second `SoccerJuggling` smoke clip produced:

```text
interval        retained frames
0.5 seconds                 60
1 second                    30
2 seconds                   15
5 seconds                    6
```

At the one-second interval, that clip extracted frames at 466.92 frames per
second and embedded them at 13.79 frames per second. The text-query smoke check
for `a person juggling a soccer ball` completed successfully and returned a
non-zero cosine score for every sampled frame. Those scores are only an endpoint
sanity check; they are not a ranking metric.

## Findings

- The local SigLIP2 embedding path, at roughly 14 frames per second in this
  sequential test, is the dominant cost. ffmpeg extraction was substantially
  faster on these short 320×240 MPEG-4 AVI clips.
- A fixed one-frame-per-second policy is viable for the measured
  development corpus and remains the MVP baseline to evaluate.
- The result does not justify selecting one second as the final policy. The
  visible-feature recall trade-off between 0.5, 1, 2, and 5 seconds has not yet
  been measured on held-out annotations.
- The product requirement excludes sub-second visible features for now. This
  permits evaluation around one-second-or-longer visible intervals, while still
  retaining the 0.5-second run as a cost/coverage reference point.

## Limitations and next steps

UCF101 action names are weak labels, not complete visible-feature ground truth.
The development clips have preliminary whole-clip annotations based on 1 fps
contact sheets. The held-out clips still need equivalent manual visible-feature
interval annotations.

Before choosing a production interval or resource limits, the next benchmark
must:

1. annotate the held-out clips without using them to tune sampling policy;
2. retain sampled frame embeddings for each candidate interval;
3. rank distinct videos by their strongest matching frame;
4. calculate held-out Video Recall@1, Recall@5, Recall@10, sampling recall,
   and temporal-hit rate;
5. measure bounded temporary-disk peak, queue delay, and end-to-end S3/job
   processing after video ingestion exists.

The raw local JSON outputs are deliberately kept outside Git.
