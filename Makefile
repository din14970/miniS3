.PHONY: build test integration run clean

build:
	go build -o minis3 .

test:
	go test ./...

integration: build
	./tests/run_integration.sh

run: build
	./minis3 --addr :9000 --data-dir ./data --bucket bucket

clean:
	rm -f minis3
