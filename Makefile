.PHONY: build start stop dev clean dashboard dashboard-dev dashboard-build

build:
	go build -o pilot .

start: build
	./pilot start

stop:
	./pilot stop

dev:
	go run . serve

clean:
	rm -f pilot

dashboard: build
	./pilot dashboard

dashboard-dev:
	cd dashboard && wails dev

dashboard-build:
	cd dashboard && wails build
