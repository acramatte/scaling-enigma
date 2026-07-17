DATABASE_URL ?= postgres://semantic_search:semantic_search@localhost:5432/semantic_search?sslmode=disable

.PHONY: db-up db-down db-psql db-reset-dev migrate-up migrate-down migrate-status tools

db-up:
	docker compose up -d --wait postgres

db-down:
	docker compose down

db-psql:
	docker compose exec postgres sh -c 'psql -U "$$POSTGRES_USER" -d "$$POSTGRES_DB"'

db-reset-dev:
	docker compose exec postgres sh -c 'psql -U "$$POSTGRES_USER" -d "$$POSTGRES_DB" -v ON_ERROR_STOP=1 -c "DROP SCHEMA public CASCADE; CREATE SCHEMA public;"'
	$(MAKE) migrate-up

migrate-up:
	goose -dir migrations postgres "$(DATABASE_URL)" up

migrate-down:
	goose -dir migrations postgres "$(DATABASE_URL)" down

migrate-status:
	goose -dir migrations postgres "$(DATABASE_URL)" status

tools:
	go install github.com/pressly/goose/v3/cmd/goose@latest
