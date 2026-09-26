.PHONY: setup run test build docker

PYTHON := $(if $(wildcard .venv/bin/python),.venv/bin/python,python3)

# Local Python environment and the pinned ONNX models used by the AI worker.
setup:
	python3 -m venv .venv
	.venv/bin/pip install --no-deps -r ai/requirements.txt
	.venv/bin/python ai/fetch_models.py models

run:
	@set -a; if [ -f .env ]; then . ./.env; fi; set +a; PYTHON_BIN=$(PYTHON) go run .

test:
	go test ./...
	$(PYTHON) -m unittest discover -s ai -p 'test_*.py'

build:
	go build -o bin/contextual-ad-lab .

docker:
	docker build -t contextual-ad-lab:local .
