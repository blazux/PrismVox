"""
Embeddings service — sentence-transformers, multilingual MiniLM (384 dims).

Runs on CPU (no GPU contention with LLM/TTS/STT). Used by:
  - the UI to embed RAG chunks on ingestion (and backfill),
  - the agent to embed the caller's question for pgvector similarity search.

POST /embed  {"texts": ["...", ...]}  → {"embeddings": [[...384...], ...]}
"""
import logging
import os
from contextlib import asynccontextmanager

from fastapi import FastAPI, HTTPException
from pydantic import BaseModel
from sentence_transformers import SentenceTransformer

logging.basicConfig(level=logging.INFO, format="%(asctime)s [%(levelname)s] embeddings — %(message)s")
logger = logging.getLogger("embeddings")

MODEL_NAME = os.getenv("EMBEDDING_MODEL", "paraphrase-multilingual-MiniLM-L12-v2")
_model: SentenceTransformer | None = None


@asynccontextmanager
async def lifespan(app: FastAPI):
    global _model
    logger.info(f"Loading embedding model {MODEL_NAME} (CPU)…")
    _model = SentenceTransformer(MODEL_NAME, device="cpu")
    _model.encode(["bonjour"], normalize_embeddings=True)  # warm up
    logger.info(f"Embeddings ready — dim={_model.get_sentence_embedding_dimension()}")
    yield


app = FastAPI(title="Embeddings", lifespan=lifespan)


class EmbedRequest(BaseModel):
    texts: list[str]


@app.post("/embed")
async def embed(req: EmbedRequest):
    if _model is None:
        raise HTTPException(503, "model not loaded")
    if not req.texts:
        return {"embeddings": []}
    # normalize → cosine similarity is a dot product; matches pgvector vector_cosine_ops.
    vecs = _model.encode(req.texts, normalize_embeddings=True, batch_size=32)
    return {"embeddings": [v.tolist() for v in vecs]}


@app.get("/health")
async def health():
    return {
        "status": "ok",
        "model": MODEL_NAME,
        "dim": _model.get_sentence_embedding_dimension() if _model else None,
    }
