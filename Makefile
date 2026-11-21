SHELL := /bin/bash
.DEFAULT_GOAL := check
.NOTPARALLEL:
.PHONY: test lint lint-selftest guards check dev deadcode release-bytes build-bytes static ui-check

test lint lint-selftest guards check:
	@bash hack/check.sh $@

dev:
	@bash hack/dev.sh

deadcode:
	@source hack/steps.sh; python3 hack/guards/run.py deadcode

release-bytes:
	@bash hack/release-bytes.sh reproduce

build-bytes:
	@bash hack/release-bytes.sh once

static:
	@bash hack/check.sh lint
	@bash hack/check.sh guards
	@python3 hack/plans/parity.py --check

ui-check:
	@bash hack/ui-check.sh
