.PHONY: up dev down logs test fmt tidy

up:
	docker compose up --build

dev:
	docker compose up --build --watch

down:
	docker compose down

logs:
	docker compose logs -f api postgres

test:
	go test ./...

fmt:
	gofmt -w cmd internal

tidy:
	go mod tidy
