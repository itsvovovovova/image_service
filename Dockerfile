FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY migrations ./migrations
RUN CGO_ENABLED=0 go build -trimpath -o /imagesvc ./cmd/imagesvc

FROM alpine:3.23
RUN apk add --no-cache ca-certificates
COPY --from=build /imagesvc /usr/local/bin/imagesvc
USER 65532:65532
ENTRYPOINT ["imagesvc"]
CMD ["api"]
