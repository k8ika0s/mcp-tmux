.PHONY: help install build test dev start clean

NPM ?= npm
RUN ?= $(NPM) run

help:
	@echo "make install   # install dependencies"
	@echo "make build     # compile TypeScript to dist/"
	@echo "make test      # run test suite (vitest)"
	@echo "make dev       # start dev mode (tsx watch)"
	@echo "make start     # run compiled server"
	@echo "make clean     # remove build artifacts"

install:
	$(NPM) install

build:
	$(RUN) build

test:
	$(RUN) test

dev:
	$(RUN) dev

start:
	$(RUN) start

clean:
	rm -rf dist
