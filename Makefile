.PHONY: build run test vet fmt clean

build:
	go build -o relay ./cmd/relay

run: build
	./relay

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

clean:
	rm -f relay
	rm -rf data dist
