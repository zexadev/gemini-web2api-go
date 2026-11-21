# syntax=docker/dockerfile:1.7

# ─── Build stage ─────────────────────────────────────────────────────────────
# --platform=$BUILDPLATFORM: builder 永远跑在构建机的原生架构上,靠 GOARCH 交叉
# 编译出目标架构的二进制。多架构构建时如果让 builder 跟着目标平台走,arm64 那份
# 会在 QEMU 里模拟编译,Go 编译是 CPU 密集型,慢一个数量级。
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

WORKDIR /src

# Cache deps separately for faster rebuilds
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# TARGETARCH 由 buildx 注入(amd64 / arm64 ...)。经典 builder 不注入,兜底 amd64。
ARG TARGETARCH

# CGO disabled — modernc/sqlite is pure Go.
# -ldflags strip symbols to shave ~5MB off binary.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH:-amd64} go build \
    -ldflags="-s -w" \
    -trimpath \
    -o /out/gemini-web2api .

# ─── Runtime stage ───────────────────────────────────────────────────────────
# distroless/static: ~2MB base, no shell, no package manager — minimal attack surface.
# We pre-create /data with nonroot ownership in the builder so distroless
# (which has no `mkdir`) doesn't choke at startup.
# 也钉在构建机架构:这一层只是 mkdir + chown(数字 uid,跟架构无关),
# 没必要为它拉一份目标架构的 alpine 再进 QEMU。
FROM --platform=$BUILDPLATFORM alpine:3.20 AS prepare
RUN mkdir -p /out/data /out/app && chown -R 65532:65532 /out/data /out/app

FROM gcr.io/distroless/static-debian12:nonroot

# /data is owned by uid 65532 (nonroot) so sqlite can create gemini.db.
COPY --from=prepare --chown=65532:65532 /out/data /data
COPY --from=prepare --chown=65532:65532 /out/app /app

WORKDIR /app
COPY --from=builder /out/gemini-web2api /app/gemini-web2api

USER nonroot:nonroot

EXPOSE 8083

ENV DB_PATH=/data/gemini.db
ENV GEMINI_HOST=0.0.0.0
ENTRYPOINT ["/app/gemini-web2api"]
CMD ["--db", "/data/gemini.db", "--port", "8083"]
