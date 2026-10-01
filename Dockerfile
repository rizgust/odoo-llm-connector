FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /odoo-gpt-mcp ./cmd/odoo-gpt-mcp

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /odoo-gpt-mcp /odoo-gpt-mcp
EXPOSE 8000
ENTRYPOINT ["/odoo-gpt-mcp"]
