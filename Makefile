.PHONY: build install dev clean

build:
	go build -o pilot .

install: build
	mkdir -p ~/.pilot
	cp pilot /usr/local/bin/pilot
	cp evaluator.mjs /usr/local/bin/evaluator.mjs
	cd . && npm install --production
	@test -f ~/.pilot/pilot.toml || cp pilot.example.toml ~/.pilot/pilot.toml
	@echo "Run 'pilot install' to configure Claude Code hooks"

dev:
	go run . serve

clean:
	rm -f pilot
