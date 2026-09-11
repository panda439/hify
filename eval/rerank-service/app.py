"""Local BGE reranker sidecar for the Hify benchmark.

The production scorer is deliberately created only when this module is run as
an application. Tests may inject a scorer, but no test double is a production
fallback.
"""

from __future__ import annotations

import math
import os
import platform
import threading
import time
from dataclasses import dataclass, field
from typing import Any, Protocol

import psutil
from fastapi import FastAPI, HTTPException
from fastapi.responses import JSONResponse
from pydantic import BaseModel, field_validator


MODEL_NAME = "BAAI/bge-reranker-v2-m3"
MODEL_REVISION = "953dc6f6f85a1b2dbfca4c34a2796e7dde08d41e"
MODEL_WEIGHT_DIGEST = "d9e3e081faff1eefb84019509b2f5558fd74c1a05a2c7db22f74174fcedb5286"
MODEL_LICENSE = "Apache-2.0"
MAX_DOCUMENTS = 50


class Scorer(Protocol):
    model_name: str
    revision: str
    digest: str
    license: str
    runtime: dict[str, str]
    ready: bool

    def ensure_loaded(self) -> None: ...

    def score(self, query: str, documents: list[str]) -> list[float]: ...


class RerankRequest(BaseModel):
    model: str
    query: str
    documents: list[str]

    @field_validator("query")
    @classmethod
    def query_required(cls, value: str) -> str:
        if not value.strip():
            raise ValueError("query must not be empty")
        return value

    @field_validator("documents")
    @classmethod
    def documents_bounded(cls, value: list[str]) -> list[str]:
        if not value or len(value) > MAX_DOCUMENTS:
            raise ValueError(f"documents count must be in 1..{MAX_DOCUMENTS}")
        if any(not item.strip() for item in value):
            raise ValueError("documents must not contain empty text")
        return value


@dataclass
class Stats:
    request_count: int = 0
    success_count: int = 0
    failure_count: int = 0
    degraded_count: int = 0
    candidate_count_total: int = 0
    cold_start_ms: int = 0
    steady_latency_ms: list[float] = field(default_factory=list)
    current_rss_bytes: int = 0
    peak_rss_bytes: int = 0
    swap_used_bytes: int = 0


class ModelScorer:
    model_name = MODEL_NAME
    revision = MODEL_REVISION
    digest = MODEL_WEIGHT_DIGEST
    license = MODEL_LICENSE

    def __init__(self) -> None:
        self.ready = False
        self._model: Any = None
        self._lock = threading.Lock()
        self.runtime = {
            "python": platform.python_version(),
            "torch": "unloaded",
            "transformers": "unloaded",
        }

    def ensure_loaded(self) -> None:
        if self.ready:
            return
        with self._lock:
            if self.ready:
                return
            from sentence_transformers import CrossEncoder
            import torch
            import transformers

            cache_root = os.environ.get("HF_HOME", os.path.join("eval", "cache", "rerank-models"))
            cache_dir = os.path.join(cache_root, "hub")
            self._model = CrossEncoder(MODEL_NAME, revision=MODEL_REVISION, cache_dir=cache_dir)
            self.runtime = {
                "python": platform.python_version(),
                "torch": str(torch.__version__),
                "transformers": str(transformers.__version__),
            }
            self.ready = True

    def score(self, query: str, documents: list[str]) -> list[float]:
        self.ensure_loaded()
        values = self._model.predict(
            [(query, document) for document in documents],
            convert_to_numpy=True,
            show_progress_bar=False,
        )
        return [float(value) for value in values]


def _identity(scorer: Scorer) -> dict[str, Any]:
    return {
        "ready": bool(scorer.ready),
        "model": scorer.model_name,
        "revision": scorer.revision,
        "digest": scorer.digest,
        "license": scorer.license,
        "runtime": dict(scorer.runtime),
    }


def _rss() -> int:
    return int(psutil.Process().memory_info().rss)


def _swap() -> int:
    return int(psutil.swap_memory().used)


def create_app(scorer: Scorer | None = None) -> FastAPI:
    model = scorer or ModelScorer()
    stats = Stats()
    stats_lock = threading.Lock()

    app = FastAPI(title="Hify BGE reranker", docs_url=None, redoc_url=None)

    @app.get("/health")
    def health() -> dict[str, Any]:
        if not model.ready:
            try:
                started = time.perf_counter()
                model.ensure_loaded()
                elapsed = int((time.perf_counter() - started) * 1000)
                with stats_lock:
                    stats.cold_start_ms = elapsed
            except Exception:
                return JSONResponse(status_code=503, content={**_identity(model), "ready": False})
        if not model.ready:
            raise HTTPException(status_code=503, detail="rerank model is not ready")
        return _identity(model)

    @app.get("/stats")
    def get_stats() -> dict[str, Any]:
        with stats_lock:
            body = {
                "request_count": stats.request_count,
                "success_count": stats.success_count,
                "failure_count": stats.failure_count,
                "degraded_count": stats.degraded_count,
                "candidate_count_total": stats.candidate_count_total,
                "cold_start_ms": stats.cold_start_ms,
                "steady_latency_ms": list(stats.steady_latency_ms),
                "current_rss_bytes": _rss(),
                "peak_rss_bytes": stats.peak_rss_bytes,
                "swap_used_bytes": _swap(),
            }
        return body

    @app.post("/rerank")
    def rerank(request: RerankRequest) -> dict[str, list[dict[str, float | int]]]:
        if request.model != MODEL_NAME:
            raise HTTPException(status_code=422, detail="requested model does not match fixed model")
        with stats_lock:
            stats.request_count += 1
            stats.candidate_count_total += len(request.documents)
        started = time.perf_counter()
        try:
            model.ensure_loaded()
            scores = model.score(request.query, request.documents)
            if len(scores) != len(request.documents) or any(not math.isfinite(value) for value in scores):
                raise ValueError("scorer returned incomplete or non-finite scores")
            results = [{"index": index, "relevance_score": value} for index, value in enumerate(scores)]
        except Exception as exc:
            with stats_lock:
                stats.failure_count += 1
                stats.current_rss_bytes = _rss()
            raise HTTPException(status_code=502, detail="rerank model request failed") from exc
        elapsed = (time.perf_counter() - started) * 1000
        with stats_lock:
            stats.success_count += 1
            stats.steady_latency_ms.append(elapsed)
            stats.current_rss_bytes = _rss()
            stats.peak_rss_bytes = max(stats.peak_rss_bytes, stats.current_rss_bytes)
        return {"results": results}

    return app


app = create_app()


if __name__ == "__main__":
    import uvicorn

    uvicorn.run("app:app", host="127.0.0.1", port=int(os.environ.get("PORT", "8090")))
