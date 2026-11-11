BIN := bin/githubpushbuilder

.PHONY: build test vet dev image clean

build:
	CGO_ENABLED=0 go build -trimpath -o $(BIN) ./cmd/githubpushbuilder

test:
	go test ./...

vet:
	go vet ./...

# Local run over plain http, with its data in ./data.
dev: build
	GPB_DATA_DIR=$${GPB_DATA_DIR:-./data} GPB_LISTEN=$${GPB_LISTEN:-127.0.0.1:8080} GPB_INSECURE_COOKIE=1 $(BIN) serve

image:
	docker build --platform linux/amd64 -t githubpushbuilder:latest .

clean:
	rm -rf bin
