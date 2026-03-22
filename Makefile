.PHONY: build start stop dev clean dashboard dashboard-build

build:
	go build -o pilot .
	npm install --production

start: build
	./pilot start

stop:
	./pilot stop

dev:
	go run . serve

clean:
	rm -f pilot

dashboard:
	cd dashboard && wails dev

dashboard-build:
	cd dashboard && wails build
