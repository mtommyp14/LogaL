# ── Stage 1: Build Go binary ──────────────────────────────────────────────────
FROM golang:1.22-alpine AS builder

WORKDIR /app
COPY go.mod go.sum main.go ui.html ./

RUN go build -ldflags="-s -w" -o logal .

# ── Stage 2: Runtime ───────────────────────────────────────────────────────────
FROM alpine:3.19

# stern version
ARG STERN_VERSION=1.34.0
# kubectl version
ARG KUBECTL_VERSION=v1.36.2

# Install dependencies
RUN apk add --no-cache ca-certificates curl

# Install kubectl
RUN curl -fsSL "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl" \
    -o /usr/local/bin/kubectl && \
    chmod +x /usr/local/bin/kubectl

# Install stern
RUN curl -fsSL "https://github.com/stern/stern/releases/download/v${STERN_VERSION}/stern_${STERN_VERSION}_linux_amd64.tar.gz" \
    -o /tmp/stern.tar.gz && \
    tar -xzf /tmp/stern.tar.gz -C /usr/local/bin stern && \
    chmod +x /usr/local/bin/stern && \
    rm /tmp/stern.tar.gz

# Copy binary
COPY --from=builder /app/logal /usr/local/bin/logal

EXPOSE 8080

ENV PORT=8080
ENV DATABASE_URL=postgres://postgres:postgres@localhost:5432/logal
ENV LOG_RETENTION_DAYS=3

ENTRYPOINT ["logal"]
