FROM golang:1.22 AS build
WORKDIR /src
COPY go.mod .
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /api ./cmd/api && CGO_ENABLED=0 go build -o /worker ./cmd/worker && CGO_ENABLED=0 go build -o /loadtest ./cmd/loadtest && CGO_ENABLED=0 go build -o /autoscaler ./cmd/autoscaler && CGO_ENABLED=0 go build -o /healthcheck ./cmd/healthcheck
FROM gcr.io/distroless/static-debian12
COPY --from=build /api /api
COPY --from=build /worker /worker
COPY --from=build /loadtest /loadtest
COPY --from=build /autoscaler /autoscaler
COPY --from=build /healthcheck /healthcheck
ENTRYPOINT ["/api"]
