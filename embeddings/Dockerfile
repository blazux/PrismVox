FROM python:3.11-slim

WORKDIR /app

# CPU-only torch (no CUDA) keeps the image small and off the GPU.
RUN pip install --no-cache-dir torch --index-url https://download.pytorch.org/whl/cpu \
 && pip install --no-cache-dir "fastapi" "uvicorn[standard]" "sentence-transformers"

ENV EMBEDDING_MODEL=paraphrase-multilingual-MiniLM-L12-v2
# Bake the model into the image so startup doesn't download it at runtime.
RUN python -c "from sentence_transformers import SentenceTransformer; SentenceTransformer('${EMBEDDING_MODEL}')"

COPY app/ ./app/

EXPOSE 8002
CMD ["uvicorn", "app.main:app", "--host", "0.0.0.0", "--port", "8002"]
