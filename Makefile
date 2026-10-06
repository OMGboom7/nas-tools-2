.PHONY: backend-dev backend-test backend-build frontend-install frontend-dev frontend-build production-build production-up production-down go-only-build python-exit-status python-exit-enforce

backend-dev:
	cd backend && go run ./cmd/server

backend-test:
	cd backend && go test ./...

backend-build:
	cd backend && mkdir -p bin && go build -o bin/nas-tools-go ./cmd/server

frontend-install:
	cd frontend && npm install

frontend-dev:
	cd frontend && npm run dev

frontend-build:
	cd frontend && npm run build

production-build:
	docker build -f docker/production.Dockerfile -t nas-tools:react-go .

production-up:
	docker compose -f docker/compose.react-go.yml up -d --build

production-down:
	docker compose -f docker/compose.react-go.yml down

go-only-build:
	docker build -f docker/go-only.Dockerfile -t nas-tools:go-only-candidate .

python-exit-status:
	./scripts/check-python-exit.sh

python-exit-enforce:
	./scripts/check-python-exit.sh --enforce
