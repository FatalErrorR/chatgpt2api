FROM --platform=$BUILDPLATFORM node:22-alpine AS web-build

WORKDIR /app/web

COPY web/package.json web/bun.lock ./
RUN npm install

COPY VERSION /app/VERSION
COPY CHANGELOG.md /app/CHANGELOG.md
COPY web ./
RUN NEXT_PUBLIC_APP_VERSION="$(cat /app/VERSION)" npm run build


FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /chatgpt2api . \
    && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /chatgpt2api-migrate ./cmd/migrate


FROM alpine:3.22 AS app

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

COPY --from=build /chatgpt2api /chatgpt2api-migrate ./
COPY --from=web-build /app/web/out ./web_dist
COPY VERSION ./

ENV PORT=80
EXPOSE 80

CMD ["./chatgpt2api"]
