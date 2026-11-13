# This is a multi-stage Dockerfile and requires >= Docker 17.05
# https://docs.docker.com/engine/userguide/eng-image/multistage-build/
FROM golang:1.26.10 AS builder

ENV GOPROXY=http://proxy.golang.org

RUN mkdir -p /src/initiative-api
WORKDIR /src/initiative-api

# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download && go mod verify

ADD . .
RUN CGO_ENABLED=0 GOOS=linux go build -v -o /bin/server ./cmd/initiative-api
RUN CGO_ENABLED=0 GOOS=linux go build -v -o /bin/reconciler ./cmd/reconciler

FROM alpine

ARG USERNAME=sweetrpg
ARG BUILD_NUMBER=unset
ARG BUILD_JOB=unset
ARG BUILD_SHA=unset
ARG BUILD_DATE=unset
ARG BUILD_VERSION=unset

RUN apk add --no-cache bash
RUN apk add --no-cache ca-certificates

RUN addgroup $USERNAME \
    && adduser -D -G $USERNAME $USERNAME

WORKDIR /app/

RUN mkdir -p /app/bin /app/config
COPY --from=builder /bin/server /app/bin/
COPY --from=builder /bin/reconciler /app/bin/

RUN echo "{\"number\":\"${BUILD_NUMBER}\",\"job\":\"${BUILD_JOB}\",\"sha\":\"${BUILD_SHA}\",\"date\":\"${BUILD_DATE}\",\"version\":\"${BUILD_VERSION}\"}" > /app/config/build-info.json
RUN chown -R ${USERNAME}:${USERNAME} /app

ENV GO_ENV=production
ENV BIND_ADDRESS=0.0.0.0:8000
ENV PORT="8000"
ENV GIN_MODE="release"
ENV VERSION=${BUILD_VERSION}

EXPOSE 8000

USER ${USERNAME}

CMD [ "/app/bin/server" ]
