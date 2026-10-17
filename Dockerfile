# syntax=docker/dockerfile:1
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/lexai ./cmd/lexai

# Runtime: the binary plus the PDF/OCR command-line tools it shells out to.
FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates poppler-utils tesseract-ocr tesseract-ocr-pol \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --system --uid 10001 --home /app lexai
COPY --from=build /out/lexai /usr/local/bin/lexai
USER lexai
WORKDIR /app
EXPOSE 8443
ENTRYPOINT ["lexai"]
CMD ["serve"]
