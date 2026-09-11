import json
import math
import types

import pytest
from fastapi.testclient import TestClient

from app import create_app


class StubScorer:
    model_name = "BAAI/bge-reranker-v2-m3"
    revision = "revision-test"
    digest = "digest-test"
    license = "Apache-2.0"
    runtime = {"python": "3.12.test", "torch": "test", "transformers": "test"}

    def __init__(self, ready=True):
        self.ready = ready

    def ensure_loaded(self):
        if not self.ready:
            raise RuntimeError("not ready")

    def score(self, query, documents):
        return [float(len(documents) - i) for i in range(len(documents))]


class BadScorer(StubScorer):
    def __init__(self, scores):
        super().__init__()
        self.scores = scores

    def score(self, query, documents):
        return self.scores


def test_health_reports_not_ready_without_leaking_runtime_paths():
    client = TestClient(create_app(StubScorer(ready=False)))
    response = client.get("/health")
    assert response.status_code == 503
    body = response.json()
    assert body["ready"] is False
    assert "BAAI/bge-reranker-v2-m3" == body["model"]
    assert "/Users/" not in json.dumps(body)


def test_health_reports_fixed_identity_when_ready():
    client = TestClient(create_app(StubScorer()))
    response = client.get("/health")
    assert response.status_code == 200
    assert response.json() == {
        "ready": True,
        "model": "BAAI/bge-reranker-v2-m3",
        "revision": "revision-test",
        "digest": "digest-test",
        "license": "Apache-2.0",
        "runtime": {"python": "3.12.test", "torch": "test", "transformers": "test"},
    }


def test_rerank_returns_one_finite_score_for_each_original_index():
    client = TestClient(create_app(StubScorer()))
    response = client.post("/rerank", json={"model": "BAAI/bge-reranker-v2-m3", "query": "问题", "documents": ["甲", "乙", "丙"]})
    assert response.status_code == 200
    results = response.json()["results"]
    assert {item["index"] for item in results} == {0, 1, 2}
    assert all(math.isfinite(item["relevance_score"]) for item in results)


def test_rerank_rejects_wrong_model_before_scoring():
    client = TestClient(create_app(StubScorer()))
    response = client.post("/rerank", json={"model": "other-model", "query": "问题", "documents": ["甲"]})
    assert response.status_code == 422


@pytest.mark.parametrize("scores", [[1.0], [float("nan"), 1.0]])
def test_rerank_rejects_incomplete_or_non_finite_scorer_response(scores):
    client = TestClient(create_app(BadScorer(scores)))
    response = client.post("/rerank", json={"model": "BAAI/bge-reranker-v2-m3", "query": "问题", "documents": ["甲", "乙"]})
    assert response.status_code == 502
    assert client.get("/stats").json()["failure_count"] == 1


@pytest.mark.parametrize("documents", [[], [str(i) for i in range(51)]])
def test_rerank_rejects_empty_or_over_limit_documents(documents):
    client = TestClient(create_app(StubScorer()))
    response = client.post("/rerank", json={"model": "BAAI/bge-reranker-v2-m3", "query": "问题", "documents": documents})
    assert response.status_code == 422


def test_stats_exposes_counts_and_latency_but_never_prompt_or_scores():
    client = TestClient(create_app(StubScorer()))
    client.post("/rerank", json={"model": "BAAI/bge-reranker-v2-m3", "query": "SECRET_QUERY", "documents": ["SECRET_DOCUMENT"]})
    body = client.get("/stats").json()
    encoded = json.dumps(body)
    assert body["request_count"] == 1
    assert body["success_count"] == 1
    assert body["failure_count"] == 0
    assert body["candidate_count_total"] == 1
    assert body["steady_latency_ms"]
    assert body["current_rss_bytes"] > 0
    assert "SECRET_QUERY" not in encoded
    assert "SECRET_DOCUMENT" not in encoded
    assert "relevance_score" not in encoded


def test_model_scorer_pins_revision_and_honors_cache_dir(monkeypatch):
    captured = {}

    class FakeCrossEncoder:
        def __init__(self, model, **kwargs):
            captured["model"] = model
            captured.update(kwargs)

        def predict(self, pairs, **kwargs):
            return [1.0 for _ in pairs]

    monkeypatch.setitem(__import__("sys").modules, "sentence_transformers", types.SimpleNamespace(CrossEncoder=FakeCrossEncoder))
    monkeypatch.setitem(__import__("sys").modules, "torch", types.SimpleNamespace(__version__="test"))
    monkeypatch.setitem(__import__("sys").modules, "transformers", types.SimpleNamespace(__version__="test"))
    monkeypatch.setenv("HF_HOME", "/tmp/hify-rerank-cache")

    from app import ModelScorer

    scorer = ModelScorer()
    scorer.ensure_loaded()
    assert captured == {
        "model": "BAAI/bge-reranker-v2-m3",
        "revision": "953dc6f6f85a1b2dbfca4c34a2796e7dde08d41e",
        "cache_dir": "/tmp/hify-rerank-cache/hub",
    }
