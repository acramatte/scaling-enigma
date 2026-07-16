



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

