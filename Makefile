.PHONY: run test build docker

run:
	@set -a; if [ -f .env ]; then . ./.env; fi; set +a; go run .

test:
	go test ./...
	python3 -m unittest discover -s ai -p 'test_*.py'

build:
	go build -o bin/contextual-ad-lab .

docker:
	docker build -t contextual-ad-lab:local .
