# https://gist.github.com/prwhite/8168133
help: ## Show this help
	@ echo 'Usage: make <target>'
	@ echo
	@ echo 'Available targets:'
	@ grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'

build: ## build the binary, more help after via `./bin/bittorrent -help`
	@ go build -o ./bin/bittorrent ./cmd/bittorrent

install: ## install the binary to $PATH, likely ~/go/bin
	@ go install ./cmd/bittorrent

uninstall: ## removes binary, assuming it's at ~/go/bin
	@ rm $$HOME/go/bin/bittorrent

example-magnet: ## run locally with a magnet link, for local development
	go run cmd/bittorrent/main.go -source $$(cat _examples/torrentfiles/nasa.magnet) -outdir downloads

example-torrentfile: ## run locally with a torrentfile, for local development
	go run cmd/bittorrent/main.go -source cat _examples/torrentfiles/nasa.torrent -outdir downloads
