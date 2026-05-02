# Makefile — Sibyl Hub
#
# Targets are added per phase — see STEPS.md.
# Phase 1: migrate-up / migrate-down / migrate-create / db-shell.
# Later phases will add: seed (Phase 14), test / lint / ci (Phase 16),
# up / down / logs (Phase 17).

# Auto-load .env (ignored if missing). .env keys must be plain KEY=value
# (no spaces around = and no shell interpolation), which matches what
# .env.example produces.
ifneq (,$(wildcard ./.env))
include .env
export
endif

MIGRATIONS_DIR := api/migrations

.PHONY: help migrate-up migrate-down migrate-create db-shell

help:
	@echo "Sibyl Hub — available targets:"
	@echo "  migrate-up                  apply all pending migrations"
	@echo "  migrate-down                revert all applied migrations (clean state)"
	@echo "  migrate-create name=<name>  scaffold a new migration pair"
	@echo "  db-shell                    open psql against \$$DATABASE_URL"
	@echo
	@echo "More targets are added per phase — see STEPS.md."

migrate-up:
	@if [ -z "$(DATABASE_URL)" ]; then \
		echo "DATABASE_URL not set — copy .env.example to .env and fill it in."; exit 1; \
	fi
	migrate -path $(MIGRATIONS_DIR) -database "$(DATABASE_URL)" up

# `down -all` reverts every applied migration without prompting. Use
# `migrate -path $(MIGRATIONS_DIR) -database "$(DATABASE_URL)" down 1`
# directly when you want to revert a single step.
migrate-down:
	@if [ -z "$(DATABASE_URL)" ]; then \
		echo "DATABASE_URL not set — copy .env.example to .env and fill it in."; exit 1; \
	fi
	migrate -path $(MIGRATIONS_DIR) -database "$(DATABASE_URL)" down -all

migrate-create:
	@if [ -z "$(name)" ]; then \
		echo "usage: make migrate-create name=<name>"; exit 1; \
	fi
	migrate create -ext sql -dir $(MIGRATIONS_DIR) -seq $(name)

db-shell:
	@if [ -z "$(DATABASE_URL)" ]; then \
		echo "DATABASE_URL not set — copy .env.example to .env and fill it in."; exit 1; \
	fi
	psql "$(DATABASE_URL)"
