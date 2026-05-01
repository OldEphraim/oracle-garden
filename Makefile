# Makefile — Oracle Garden
#
# Targets are added per phase — see STEPS.md.
# (Phase 1: migrate-up / migrate-down / migrate-create / db-shell.
#  Phase 14: seed.
#  Phase 16: test / lint / ci.
#  Phase 17: up / down / logs.)

.PHONY: help

help:
	@echo "Targets are added per phase — see STEPS.md."
