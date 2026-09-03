# syntax=docker/dockerfile:1
#
# The embedder, with the model already inside it.
#
# Baked and not pulled at start, because embed.FromEnv makes ONE real round trip
# at boot with a 30-second deadline and both binaries fatal when it fails: a
# ~274 MB model download would race that probe and lose, and the circuit breaker
# would roll back a stack that was about to work.
#
# NOTE: this image has never been built. It needs a running ollama server during
# the build to pull the model, which is why the RUN below starts one; whether
# that succeeds under BuildKit is [ACCOUNT-REQUIRED]-adjacent — it is unverified
# here and stated as unverified rather than assumed.
FROM ollama/ollama:0.33.0@sha256:08ddf4b4dbfdc4fc1f5b0fe535915998581085402e307b57bb308db68460e372

ARG EMBED_MODEL=nomic-embed-text

# 768 dimensions, which store.EmbeddingDim fixes and store.CheckDim refuses to
# deviate from at boot.
RUN /bin/ollama serve & \
    for i in $(seq 30); do /bin/ollama list >/dev/null 2>&1 && break; sleep 1; done; \
    /bin/ollama pull "$EMBED_MODEL" && \
    pkill ollama || true

ENV OLLAMA_HOST=0.0.0.0:11434
EXPOSE 11434
